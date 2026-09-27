//go:build appleintegration

package applecontainer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"opensbx/internal/api"
	"opensbx/internal/database"
	"opensbx/internal/images"
	"opensbx/internal/proxy"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
	"opensbx/internal/service"
)

const appleIntegrationStepTimeout = 20 * time.Second

type registryRequestCounter struct {
	transport http.RoundTripper
	mu        sync.Mutex
	requests  int
}

func (r *registryRequestCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests++
	r.mu.Unlock()
	return r.transport.RoundTrip(req)
}
func (r *registryRequestCounter) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.requests }

func appleStep(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), appleIntegrationStepTimeout)
}

func startAppleSingleListener(t *testing.T, app *service.Service, runtimeClient *Client, repo *database.Repository) (*http.Client, *net.TCPAddr) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app.SetAddress(listener.Addr())
	proxyServer := proxy.New(repo)
	proxyServer.SetResolver(app.Route)
	runtimeClient.SetCacheInvalidator(proxyServer.InvalidateCache)
	management := gin.New()
	handler := api.New(app)
	handler.RegisterHealthCheck(management)
	handler.RegisterRoutes(management.Group("/v1"))
	mcp := api.NewMCPHandler(app)
	management.Any("/v1/mcp", gin.WrapH(mcp))
	management.Any("/v1/mcp/*path", gin.WrapH(mcp))
	server := &http.Server{Handler: proxy.LocalHandler(listener.Addr(), management, proxyServer.Handler()), ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-serveDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("Apple API listener serve error: %v", err)
		}
	})
	return &http.Client{Timeout: 3 * time.Second}, listener.Addr().(*net.TCPAddr)
}

func getFriendlySandbox(t *testing.T, client *http.Client, addr *net.TCPAddr, friendlyHost, path string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://"+addr.String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = friendlyHost
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("single-listener request Host=%q path=%q: %v", friendlyHost, path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestAppleRuntimeEndToEnd(t *testing.T) {
	if os.Getenv("OPENSBX_APPLE_INTEGRATION") != "1" {
		t.Skip("set OPENSBX_APPLE_INTEGRATION=1 to opt in; this test contacts an already-running Apple container service")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skipf("Apple container integration requires darwin/arm64; got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	image := strings.TrimSpace(os.Getenv("OPENSBX_APPLE_TEST_IMAGE"))
	if image == "" {
		t.Fatal("OPENSBX_APPLE_TEST_IMAGE must name an image already pulled locally; this test never pulls images")
	}

	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	runner, err := Resolve(resolveCtx)
	resolveCancel()
	if err != nil {
		t.Fatalf("Apple container CLI prerequisite: %v", err)
	}
	db := database.New(filepath.Join(t.TempDir(), "apple-integration.db"))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	runtimeClient := New(repo, runner, nil)
	dataDir := strings.TrimSpace(os.Getenv("OPENSBX_APPLE_TEST_DATA_DIR"))
	if dataDir == "" {
		dataDir = filepath.Join(t.TempDir(), "opensbx-data")
	}
	registryCounter := &registryRequestCounter{transport: http.DefaultTransport}
	store, err := images.Open(dataDir, images.WithTransport(registryCounter))
	if err != nil {
		t.Fatal(err)
	}
	adapter := runtimeio.New(runtimeClient, runtimeClient, repo)
	client, err := service.New(context.Background(), adapter, adapter, store, repo)
	if err != nil {
		t.Fatalf("construct runtime-neutral service: %v", err)
	}
	var managedCacheReference string
	// The fresh temporary repository is the cleanup allow-list. Register before
	// Create so recovery records from a failed rollback are included as well.
	t.Cleanup(func() {
		rows, err := repo.FindAll()
		if err != nil {
			t.Errorf("read isolated Apple integration cleanup records: %v", err)
			return
		}
		for _, row := range rows {
			if !validID(row.NativeID) {
				t.Errorf("refusing to clean invalid native ID from isolated test database: %q", row.NativeID)
				continue
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 25*time.Second)
			err := client.Remove(cleanupCtx, sandbox.SandboxID(row.ID))
			cleanupCancel()
			if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
				t.Errorf("cleanup of sandbox recorded only in isolated test database (%s) failed: %v", row.ID, err)
			}
		}
		if managedCacheReference != "" {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 25*time.Second)
			cleanupErr := runner.Run(cleanupCtx, []string{"image", "delete", "--force", managedCacheReference}, nil, io.Discard, io.Discard)
			cleanupCancel()
			if cleanupErr != nil {
				t.Errorf("cleanup generated OpenSBX cache ref %q failed: %v", managedCacheReference, cleanupErr)
			}
		}
	})

	ctx, cancel := appleStep(t)
	err = client.Ping(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Apple service must already be running at validated version 1.4.1: %v", err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	ctx, cancel = appleStep(t)
	artifact, resolveErr := store.Resolve(ctx, image, platform)
	cancel()
	if errors.Is(resolveErr, sandbox.ErrImageNotFound) {
		// Default opt-in runs explicitly prepare only this named test artifact.
		// A caller may supply the reviewed reusable diagnostic store to avoid repulling.
		ctx, cancel = appleStep(t)
		err = client.PullImage(ctx, image)
		cancel()
		if err != nil {
			t.Fatalf("pull test image %q into isolated OpenSBX OCI store: %v", image, err)
		}
		ctx, cancel = appleStep(t)
		artifact, err = store.Resolve(ctx, image, platform)
		cancel()
		if err != nil {
			t.Fatalf("resolve pulled OCI platform: %v", err)
		}
	} else if resolveErr != nil {
		t.Fatalf("resolve preprepared OCI platform: %v", resolveErr)
	}
	registryRequestsAfterPull := registryCounter.count()
	managedCacheReference, err = CacheReference(artifact.Manifest.Digest.String())
	if err != nil {
		t.Fatalf("derive cleanup handle for generated native cache image: %v", err)
	}
	ctx, cancel = appleStep(t)
	imageInfo, err := client.InspectImage(ctx, image)
	cancel()
	if err != nil {
		t.Fatalf("inspect owned OCI image %q: %v", image, err)
	}
	if imageInfo.OS != "linux" || imageInfo.Architecture != "arm64" {
		t.Fatalf("test image must have a local linux/arm64 variant; got %s/%s", imageInfo.OS, imageInfo.Architecture)
	}

	isNode := strings.Contains(strings.ToLower(image), "node")
	ports := []sandbox.Port(nil)
	if isNode {
		ports = []sandbox.Port{{Number: 3000, Protocol: "tcp"}}
	} else {
		t.Logf("HTTP published-port subtest skipped: %q is not identified as a Node image; core shell/file/exec/lifecycle smoke continues", image)
	}
	var apiClient *http.Client
	var apiAddress *net.TCPAddr
	if isNode {
		apiClient, apiAddress = startAppleSingleListener(t, client, runtimeClient, repo)
	}
	ctx, cancel = appleStep(t)
	created, err := client.Create(ctx, sandbox.CreateOptions{Image: image, Ports: ports, Timeout: 600 * time.Second})
	cancel()
	if err != nil {
		t.Fatalf("create from pre-pulled image without implicit pull: %v", err)
	}
	if !validPublicSandboxID(string(created.ID)) {
		t.Fatalf("backend returned unexpected non-owned sandbox identifier %q", created.ID)
	}
	var friendlyURL *url.URL
	if isNode {
		friendlyURL, err = url.Parse(created.URL)
		if err != nil {
			t.Fatalf("parse friendly sandbox URL %q: %v", created.URL, err)
		}
		wantPort := strconv.Itoa(apiAddress.Port)
		if friendlyURL.Scheme != "http" || friendlyURL.Host != string(created.ID)+".localhost:"+wantPort {
			t.Fatalf("sandbox URL %q does not use generated sandbox Host and shared API listener port %s", created.URL, wantPort)
		}
		status, body := getFriendlySandbox(t, apiClient, apiAddress, "localhost:"+wantPort, "/v1/health")
		if status != http.StatusOK || !strings.Contains(body, "healthy") {
			t.Fatalf("control API on shared listener status=%d body=%q", status, body)
		}
	}
	if got := registryCounter.count(); got != registryRequestsAfterPull {
		t.Fatalf("create performed implicit registry access: count before=%d after=%d", registryRequestsAfterPull, got)
	}

	const nestedPath = "tmp/opensbx-integration/nested/result.txt"
	const persistedPath = "tmp/opensbx-integration/persist.txt"
	ctx, cancel = appleStep(t)
	err = client.WriteFile(ctx, created.ID, nestedPath, "nested file survived lifecycle")
	cancel()
	if err != nil {
		t.Fatalf("write nested guest file: %v", err)
	}
	ctx, cancel = appleStep(t)
	content, err := client.ReadFile(ctx, created.ID, nestedPath)
	cancel()
	if err != nil || content != "nested file survived lifecycle" {
		t.Fatalf("read nested guest file=%q err=%v", content, err)
	}
	ctx, cancel = appleStep(t)
	err = client.DeleteFile(ctx, created.ID, nestedPath)
	cancel()
	if err != nil {
		t.Fatalf("delete nested guest file: %v", err)
	}
	ctx, cancel = appleStep(t)
	err = client.WriteFile(ctx, created.ID, persistedPath, "persistent across stop/start")
	cancel()
	if err != nil {
		t.Fatalf("write lifecycle marker: %v", err)
	}

	command, err := integrationExec(t, client, created.ID, "sh", []string{"-c", "printf guest-out; printf guest-err >&2; exit 7"})
	if err != nil {
		t.Fatalf("start command with stdout/stderr/nonzero exit: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	finished, err := client.WaitCommand(ctx, created.ID, command.ID)
	cancel()
	if err != nil || finished.ExitCode == nil || *finished.ExitCode != 7 {
		t.Fatalf("command exit detail=%+v err=%v", finished, err)
	}
	ctx, cancel = appleStep(t)
	logs, err := client.GetCommandLogs(ctx, created.ID, command.ID)
	cancel()
	if err != nil || !strings.Contains(logs.Stdout, "guest-out") || !strings.Contains(logs.Stderr, "guest-err") {
		t.Fatalf("command logs=%+v err=%v", logs, err)
	}

	first, err := integrationExec(t, client, created.ID, "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("start first identical sleep: %v", err)
	}
	second, err := integrationExec(t, client, created.ID, "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("start second identical sleep: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("identical commands shared ID %q", first.ID)
	}
	ctx, cancel = appleStep(t)
	_, err = client.KillCommand(ctx, created.ID, first.ID, 15)
	cancel()
	if err != nil {
		t.Fatalf("send guest SIGTERM to first command: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
	firstDone, err := client.WaitCommand(ctx, created.ID, first.ID)
	cancel()
	if err != nil || firstDone.ExitCode == nil || *firstDone.ExitCode == 0 {
		t.Fatalf("SIGTERM command did not finish nonzero: detail=%+v err=%v", firstDone, err)
	}
	ctx, cancel = appleStep(t)
	sibling, err := client.GetCommand(ctx, created.ID, second.ID)
	cancel()
	if err != nil || sibling.ExitCode != nil {
		t.Fatalf("signaling one command affected identical sibling: detail=%+v err=%v", sibling, err)
	}
	ctx, cancel = appleStep(t)
	_, err = client.KillCommand(ctx, created.ID, second.ID, 9)
	cancel()
	if err != nil {
		t.Fatalf("send guest SIGKILL to sibling: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
	secondDone, err := client.WaitCommand(ctx, created.ID, second.ID)
	cancel()
	if err != nil || secondDone.ExitCode == nil || *secondDone.ExitCode == 0 {
		t.Fatalf("SIGKILL command did not finish nonzero: detail=%+v err=%v", secondDone, err)
	}

	third, err := integrationExec(t, client, created.ID, "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("start SIGKILL target: %v", err)
	}
	fourth, err := integrationExec(t, client, created.ID, "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("start live identical SIGKILL sibling: %v", err)
	}
	ctx, cancel = appleStep(t)
	_, err = client.KillCommand(ctx, created.ID, third.ID, 9)
	cancel()
	if err != nil {
		t.Fatalf("send guest SIGKILL while identical sibling is live: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
	thirdDone, err := client.WaitCommand(ctx, created.ID, third.ID)
	cancel()
	if err != nil || thirdDone.ExitCode == nil || *thirdDone.ExitCode == 0 {
		t.Fatalf("isolated SIGKILL target did not finish nonzero: detail=%+v err=%v", thirdDone, err)
	}
	ctx, cancel = appleStep(t)
	liveSibling, err := client.GetCommand(ctx, created.ID, fourth.ID)
	cancel()
	if err != nil || liveSibling.ExitCode != nil {
		t.Fatalf("SIGKILL affected its identical live sibling: detail=%+v err=%v", liveSibling, err)
	}
	ctx, cancel = appleStep(t)
	_, err = client.KillCommand(ctx, created.ID, fourth.ID, 15)
	cancel()
	if err != nil {
		t.Fatalf("clean up exact remaining sibling command %s: %v", fourth.ID, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
	_, err = client.WaitCommand(ctx, created.ID, fourth.ID)
	cancel()
	if err != nil {
		t.Fatalf("wait for remaining sibling command %s: %v", fourth.ID, err)
	}

	var serviceCommand sandbox.Command
	if isNode {
		nodeProgram := "require('http').createServer((_,res)=>res.end('apple-runtime-ok')).listen(3000,'0.0.0.0')"
		serviceCommand, err = integrationExec(t, client, created.ID, "node", []string{"-e", nodeProgram})
		if err != nil {
			t.Fatalf("start Node localhost test service: %v", err)
		}
		ctx, cancel = appleStep(t)
		network, networkErr := client.GetNetwork(ctx, created.ID)
		cancel()
		if networkErr != nil {
			t.Fatalf("get published localhost port: %v", networkErr)
		}
		var hostPort string
		for _, published := range network.Ports {
			if published.Guest == (sandbox.Port{Number: 3000, Protocol: "tcp"}) {
				hostPort = strconv.Itoa(int(published.Host))
				break
			}
		}
		if hostPort == "" {
			t.Fatalf("published test port missing from backend network map: %+v", network)
		}
		if err := waitForNodeEndpoint(t, hostPort); err != nil {
			t.Fatal(err)
		}
		status, body := getFriendlySandbox(t, apiClient, apiAddress, friendlyURL.Host, "/")
		if status != http.StatusOK || body != "apple-runtime-ok" {
			t.Fatalf("friendly sandbox Host route status=%d body=%q", status, body)
		}
		ctx, cancel = appleStep(t)
		_, err = client.KillCommand(ctx, created.ID, serviceCommand.ID, 15)
		cancel()
		if err != nil {
			t.Fatalf("stop Node test server: %v", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
		_, err = client.WaitCommand(ctx, created.ID, serviceCommand.ID)
		cancel()
		if err != nil {
			t.Fatalf("wait for Node test server to stop: %v", err)
		}
	}

	ctx, cancel = appleStep(t)
	err = client.Stop(ctx, created.ID)
	cancel()
	if err != nil {
		t.Fatalf("stop generated sandbox: %v", err)
	}
	if isNode {
		status, _ := getFriendlySandbox(t, apiClient, apiAddress, friendlyURL.Host, "/")
		if status != http.StatusBadGateway {
			t.Fatalf("stopped sandbox Host route reused stale port, status=%d", status)
		}
	}
	ctx, cancel = appleStep(t)
	_, err = client.Start(ctx, created.ID)
	cancel()
	if err != nil {
		t.Fatalf("restart same sandbox filesystem: %v", err)
	}
	ctx, cancel = appleStep(t)
	content, err = client.ReadFile(ctx, created.ID, persistedPath)
	cancel()
	if err != nil || content != "persistent across stop/start" {
		t.Fatalf("sandbox filesystem was not preserved: content=%q err=%v", content, err)
	}
	ctx, cancel = appleStep(t)
	err = client.RenewExpiration(ctx, created.ID, time.Second)
	cancel()
	if err != nil {
		t.Fatalf("schedule short TTL for generated sandbox: %v", err)
	}
	deadline := time.NewTimer(12 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		ctx, cancel = appleStep(t)
		detail, inspectErr := client.Inspect(ctx, created.ID)
		cancel()
		if inspectErr != nil {
			t.Fatalf("inspect sandbox during TTL stop: %v", inspectErr)
		}
		if !detail.Running {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("renewed one-second TTL did not stop the generated sandbox within the bounded wait")
		case <-ticker.C:
		}
	}

	ctx, cancel = appleStep(t)
	err = client.Remove(ctx, created.ID)
	cancel()
	if err != nil {
		t.Fatalf("remove only generated sandbox %s: %v", created.ID, err)
	}
	if row, err := repo.FindByID(string(created.ID)); err != nil || row != nil {
		t.Fatalf("sandbox persistence after exact-ID removal: row=%+v err=%v", row, err)
	}
	ctx, cancel = appleStep(t)
	_, err = client.Inspect(ctx, created.ID)
	cancel()
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Inspect removed generated sandbox error=%v, want not found", err)
	}
	// Remove only the cache reference derived from this test's selected manifest,
	// then recreate from the already prepared temp OCI store with no registry path.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), appleIntegrationStepTimeout)
	err = runner.Run(cleanupCtx, []string{"image", "delete", "--force", managedCacheReference}, nil, io.Discard, io.Discard)
	cleanupCancel()
	if err != nil {
		t.Fatalf("drop only generated native cache ref %q: %v", managedCacheReference, err)
	}
	managedCacheReference, err = CacheReference(artifact.Manifest.Digest.String())
	if err != nil {
		t.Fatalf("prepare exact cleanup handle for recovery cache: %v", err)
	}
	ctx, cancel = appleStep(t)
	recovered, err := client.Create(ctx, sandbox.CreateOptions{Image: image, Timeout: 600 * time.Second})
	cancel()
	if err != nil {
		t.Fatalf("offline cache-miss recovery from prepared OCI store: %v", err)
	}
	if recovered.ID == created.ID {
		t.Fatalf("offline recovery reused public sandbox ID %q", recovered.ID)
	}
	row, err := repo.FindByID(string(recovered.ID))
	if err != nil || row == nil || row.ImageRoot != artifact.Root.Digest.String() || row.ImageManifest != artifact.Manifest.Digest.String() {
		t.Fatalf("offline recovered provenance row=%+v err=%v", row, err)
	}
	if got := registryCounter.count(); got != registryRequestsAfterPull {
		t.Fatalf("offline cache recovery downloaded from registry: before=%d after=%d", registryRequestsAfterPull, got)
	}
	ctx, cancel = appleStep(t)
	err = client.Remove(ctx, recovered.ID)
	cancel()
	if err != nil {
		t.Fatalf("remove offline recovery sandbox %s: %v", recovered.ID, err)
	}
}

func integrationExec(t *testing.T, client sandbox.Process, sandboxID sandbox.SandboxID, command string, args []string) (sandbox.Command, error) {
	t.Helper()
	ctx, cancel := appleStep(t)
	defer cancel()
	return client.ExecCommand(ctx, sandboxID, sandbox.ProcessRequest{Command: command, Args: args})
}

func validPublicSandboxID(id string) bool {
	if !strings.HasPrefix(id, "sbx-") || len(id) != len("sbx-")+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "sbx-"))
	return err == nil
}

func waitForNodeEndpoint(t *testing.T, hostPort string) error {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	url := "http://127.0.0.1:" + hostPort
	for {
		response, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && string(body) == "apple-runtime-ok" {
				return nil
			}
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("published localhost Node service did not become ready at %s", url)
		case <-ticker.C:
		}
	}
}
