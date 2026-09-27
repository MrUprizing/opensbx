//go:build integration && appleintegration

package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/v1"

	"opensbx/internal/api"
	"opensbx/internal/applecontainer"
	"opensbx/internal/database"
	"opensbx/internal/docker"
	"opensbx/internal/images"
	"opensbx/internal/proxy"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
	"opensbx/internal/service"
)

const (
	crossRuntimeImage    = "node:25-alpine"
	crossRuntimeDataDir  = "/var/folders/5c/jb6nw4ts7533rm_vydyhrcz40000gp/T/opencode/opensbx-apple-diagnosis/data"
	crossRuntimeTimeout  = 180 * time.Second
	crossRuntimeTestPort = uint16(3000)
)

type deniedRegistryTransport struct{ requests atomic.Int64 }

func (d *deniedRegistryTransport) RoundTrip(*http.Request) (*http.Response, error) {
	d.requests.Add(1)
	return nil, errors.New("unexpected registry request in prepared-store integration")
}

type cacheInvalidator interface {
	SetCacheInvalidator(func(string))
}

type runtimeStack struct {
	app          *service.Service
	repo         *database.Repository
	caps         sandbox.Capabilities
	engine       interface{ Shutdown(context.Context) }
	cleanupCache func() error
}

type runtimeObservation struct {
	publicID string
	nativeID string
	name     string
	root     string
	manifest string
	file     string
	stdout   string
	stderr   string
	exitCode int
}

func TestIntegration_ManagedOCIImagePortability(t *testing.T) {
	if os.Getenv("OPENSBX_CROSS_RUNTIME_INTEGRATION") != "1" {
		t.Skip("set OPENSBX_CROSS_RUNTIME_INTEGRATION=1 to opt in to live Docker and Apple Container portability")
	}
	dataDir := os.Getenv("OPENSBX_CROSS_RUNTIME_DATA_DIR")
	if dataDir == "" {
		dataDir = crossRuntimeDataDir
	}
	if _, err := os.Stat(filepath.Join(dataDir, "images", "catalog.json")); err != nil {
		t.Fatalf("prepared isolated OCI store is required at %s: %v", dataDir, err)
	}

	registryTransport := &deniedRegistryTransport{}
	store, err := images.Open(dataDir, images.WithTransport(registryTransport))
	if err != nil {
		t.Fatalf("open already-prepared shared OCI store: %v", err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	artifact, err := store.Resolve(ctx, crossRuntimeImage, platform)
	cancel()
	if err != nil {
		t.Fatalf("resolve prepared %s artifact without registry access: %v", crossRuntimeImage, err)
	}
	configName, err := artifact.Image.ConfigName()
	if err != nil {
		t.Fatalf("read prepared image config digest: %v", err)
	}
	rootDigest, manifestDigest := artifact.Root.Digest.String(), artifact.Manifest.Digest.String()
	t.Logf("shared OCI artifact reference=%s platform=%s root=%s selected_manifest=%s config=%s", crossRuntimeImage, platform.String(), rootDigest, manifestDigest, configName.String())
	if registryTransport.requests.Load() != 0 {
		t.Fatalf("prepared-store resolution made %d registry requests", registryTransport.requests.Load())
	}

	dockerRepo := crossRuntimeRepository(t, "docker")
	appleRepo := crossRuntimeRepository(t, "apple")
	dockerClient := docker.New(dockerRepo)
	dockerStack := newDockerRuntimeStack(t, dockerClient, dockerRepo, store)
	appleRunner, err := applecontainer.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve already-installed Apple Container CLI: %v", err)
	}
	appleClient := applecontainer.New(appleRepo, appleRunner, nil)
	appleStack := newRuntimeStack(t, appleClient, appleClient, appleRepo, store, nil)

	for name, stack := range map[string]*runtimeStack{"Docker": dockerStack, "Apple": appleStack} {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 20*time.Second)
		pingErr := stack.app.Ping(pingCtx)
		pingCancel()
		if pingErr != nil {
			t.Fatalf("%s runtime must already be running: %v", name, pingErr)
		}
		if stack.caps.Platform.OS != platform.OS || stack.caps.Platform.Architecture != platform.Architecture {
			t.Fatalf("%s native platform=%+v; want %s", name, stack.caps.Platform, platform.String())
		}
	}

	privateDockerTag := "localhost/opensbx-cache:" + strings.TrimPrefix(manifestDigest, "sha256:")
	dockerTagExisted, err := dockerClient.ImageExists(context.Background(), privateDockerTag)
	if err != nil {
		t.Fatalf("inspect exact Docker cache tag before test: %v", err)
	}
	dockerConfigExisted, err := dockerClient.ImageExists(context.Background(), configName.String())
	if err != nil {
		t.Fatalf("inspect Docker config digest before test: %v", err)
	}
	dockerCacheOwned := !dockerTagExisted && !dockerConfigExisted

	appleCacheRef, err := applecontainer.CacheReference(manifestDigest)
	if err != nil {
		t.Fatalf("derive exact Apple cache reference: %v", err)
	}
	appleCacheExisted, err := appleImageExists(appleRunner, appleCacheRef)
	if err != nil {
		t.Fatalf("inspect exact Apple cache ref before test: %v", err)
	}
	appleCacheOwned := !appleCacheExisted
	t.Logf("native cache ownership baseline: Docker tag_existed=%t config_existed=%t; Apple exact_ref_existed=%t", dockerTagExisted, dockerConfigExisted, appleCacheExisted)
	dockerStack.cleanupCache = func() error {
		if !dockerCacheOwned {
			return nil
		}
		exists, err := dockerClient.ImageExists(context.Background(), privateDockerTag)
		if err != nil || !exists {
			return err
		}
		return runExactDockerImageRemoval(t, privateDockerTag)
	}
	appleStack.cleanupCache = func() error {
		if !appleCacheOwned {
			return nil
		}
		exists, existsErr := appleImageExists(appleRunner, appleCacheRef)
		if existsErr != nil || !exists {
			return existsErr
		}
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer deleteCancel()
		return appleRunner.Run(deleteCtx, []string{"image", "delete", "--force", appleCacheRef}, nil, io.Discard, io.Discard)
	}

	dockerListener := startCrossRuntimeListener(t, dockerStack.app, dockerClient, dockerRepo)
	dockerDiagnostics := func() string {
		return inspectDockerTestArtifact(dockerClient, privateDockerTag, configName.String())
	}
	var dockerFirst runtimeObservation
	dockerPassed := t.Run("Docker", func(t *testing.T) {
		dockerFirst = exerciseCrossRuntime(t, dockerStack.app, dockerRepo, crossRuntimeImage, rootDigest, manifestDigest, dockerListener, dockerDiagnostics)
		t.Logf("Docker managed-artifact execution PASS: public=%s native=%s name=%s root=%s manifest=%s file=%q exit=%d stdout=%q stderr=%q", dockerFirst.publicID, dockerFirst.nativeID, dockerFirst.name, dockerFirst.root, dockerFirst.manifest, dockerFirst.file, dockerFirst.exitCode, dockerFirst.stdout, dockerFirst.stderr)
		if err := removeCrossRuntimeSandbox(t, dockerStack.app, dockerRepo, dockerFirst.publicID); err != nil {
			t.Fatalf("remove first Docker sandbox before switching runtime: %v", err)
		}
		if err := verifyOwnershipRemoved(dockerRepo, dockerFirst.publicID); err != nil {
			t.Fatal(err)
		}

		if dockerCacheOwned {
			if err := runExactDockerImageRemoval(t, privateDockerTag); err != nil {
				t.Fatalf("remove only newly introduced Docker test cache %s: %v", privateDockerTag, err)
			}
			t.Logf("Docker cache loss: deleted only newly introduced ref %s", privateDockerTag)
		} else {
			t.Log("Docker cache deletion/reimport not covered: matching cache handle existed before the test and is preserved")
		}
		dockerRecovery := exerciseCrossRuntime(t, dockerStack.app, dockerRepo, crossRuntimeImage, rootDigest, manifestDigest, nil, dockerDiagnostics)
		t.Logf("Docker offline cache-recovery PASS: public=%s native=%s root=%s manifest=%s", dockerRecovery.publicID, dockerRecovery.nativeID, dockerRecovery.root, dockerRecovery.manifest)
		if err := removeCrossRuntimeSandbox(t, dockerStack.app, dockerRepo, dockerRecovery.publicID); err != nil {
			t.Fatalf("remove Docker cache-recovery sandbox: %v", err)
		}
	})

	var appleFirst runtimeObservation
	applePassed := t.Run("AppleContainer", func(t *testing.T) {
		appleFirst = exerciseCrossRuntime(t, appleStack.app, appleRepo, crossRuntimeImage, rootDigest, manifestDigest, nil, nil)
		t.Logf("Apple managed-artifact execution PASS: public=%s native=%s name=%s root=%s manifest=%s file=%q exit=%d stdout=%q stderr=%q", appleFirst.publicID, appleFirst.nativeID, appleFirst.name, appleFirst.root, appleFirst.manifest, appleFirst.file, appleFirst.exitCode, appleFirst.stdout, appleFirst.stderr)
		if err := removeCrossRuntimeSandbox(t, appleStack.app, appleRepo, appleFirst.publicID); err != nil {
			t.Fatalf("remove Apple sandbox created from the same shared OCI artifact: %v", err)
		}
		if err := verifyOwnershipRemoved(appleRepo, appleFirst.publicID); err != nil {
			t.Fatal(err)
		}

		if appleCacheOwned {
			deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 30*time.Second)
			deleteErr := appleRunner.Run(deleteCtx, []string{"image", "delete", "--force", appleCacheRef}, nil, io.Discard, io.Discard)
			deleteCancel()
			if deleteErr != nil {
				t.Fatalf("delete only newly introduced Apple test cache %s: %v", appleCacheRef, deleteErr)
			}
		} else {
			t.Log("Apple cache deletion/reimport not covered: exact cache reference existed before the test and is preserved")
		}
		appleRecovery := exerciseCrossRuntime(t, appleStack.app, appleRepo, crossRuntimeImage, rootDigest, manifestDigest, nil, nil)
		t.Logf("Apple offline cache-recovery PASS: public=%s native=%s root=%s manifest=%s", appleRecovery.publicID, appleRecovery.nativeID, appleRecovery.root, appleRecovery.manifest)
		if err := removeCrossRuntimeSandbox(t, appleStack.app, appleRepo, appleRecovery.publicID); err != nil {
			t.Fatalf("remove Apple cache-recovery sandbox: %v", err)
		}
	})

	if dockerPassed && applePassed && (dockerFirst.root != appleFirst.root || dockerFirst.manifest != appleFirst.manifest) {
		t.Fatalf("runtime OCI identity differs: Docker root/manifest=%s/%s Apple=%s/%s", dockerFirst.root, dockerFirst.manifest, appleFirst.root, appleFirst.manifest)
	}
	if dockerPassed && applePassed && (dockerFirst.file != appleFirst.file || dockerFirst.stdout != appleFirst.stdout || dockerFirst.stderr != appleFirst.stderr || dockerFirst.exitCode != appleFirst.exitCode) {
		t.Fatalf("runtime execution differs: Docker=%+v Apple=%+v", dockerFirst, appleFirst)
	}
	if registryTransport.requests.Load() != 0 {
		t.Fatalf("runtime creation or cache recovery made %d registry requests from prepared store", registryTransport.requests.Load())
	}
	if dockerPassed && applePassed {
		t.Logf("cross-runtime parity PASS: Docker public=%s native=%s name=%s Apple public=%s native=%s name=%s canonical_root=%s selected_manifest=%s script_exit=%d stdout=%q stderr=%q registry_requests=%d", dockerFirst.publicID, dockerFirst.nativeID, dockerFirst.name, appleFirst.publicID, appleFirst.nativeID, appleFirst.name, rootDigest, manifestDigest, dockerFirst.exitCode, dockerFirst.stdout, dockerFirst.stderr, registryTransport.requests.Load())
	} else {
		t.Logf("cross-runtime parity incomplete: Docker subtest passed=%t Apple subtest passed=%t; shared root=%s manifest=%s", dockerPassed, applePassed, rootDigest, manifestDigest)
	}
}

func crossRuntimeRepository(t *testing.T, name string) *database.Repository {
	t.Helper()
	db := database.New(filepath.Join(t.TempDir(), name+".sqlite"))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("open %s test database: %v", name, err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return database.NewRepository(db)
}

func newDockerRuntimeStack(t *testing.T, client *docker.Client, repo *database.Repository, store *images.Store) *runtimeStack {
	t.Helper()
	stack := newRuntimeStack(t, client, client, repo, store, client)
	stack.engine = client
	return stack
}

func newRuntimeStack(t *testing.T, engine runtimeio.Engine, cache runtimeio.NativeCache, repo *database.Repository, store *images.Store, invalidator cacheInvalidator) *runtimeStack {
	t.Helper()
	adapter := runtimeio.New(engine, cache, repo)
	app, err := service.New(context.Background(), adapter, adapter, store, repo)
	if err != nil {
		t.Fatalf("construct runtime-neutral service: %v", err)
	}
	if invalidator != nil {
		invalidator.SetCacheInvalidator(nil)
	}
	caps, err := cache.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("read %s runtime capabilities: %v", caps.Runtime, err)
	}
	stack := &runtimeStack{app: app, repo: repo, caps: caps}
	t.Cleanup(func() {
		rows, err := repo.FindAll()
		if err != nil {
			t.Errorf("read %s integration ownership records: %v", runtimeLabel(cache), err)
			return
		}
		for _, row := range rows {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			removeErr := app.Remove(cleanupCtx, sandbox.SandboxID(row.ID))
			cancel()
			if removeErr != nil && !errors.Is(removeErr, sandbox.ErrNotFound) {
				t.Errorf("remove only test-owned %s sandbox %s (native %s): %v", runtimeLabel(cache), row.ID, row.NativeID, removeErr)
			}
		}
		if stack.cleanupCache != nil {
			if err := stack.cleanupCache(); err != nil {
				t.Errorf("remove only newly introduced %s managed image cache: %v", runtimeLabel(cache), err)
			}
		}
		if stack.engine != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			stack.engine.Shutdown(shutdownCtx)
			cancel()
		}
	})
	return stack
}

func runtimeLabel(cache runtimeio.NativeCache) string {
	if caps, err := cache.Capabilities(context.Background()); err == nil && caps.Runtime != "" {
		return caps.Runtime
	}
	return "runtime"
}

func startCrossRuntimeListener(t *testing.T, app *service.Service, invalidator cacheInvalidator, repo *database.Repository) *net.TCPAddr {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for sandbox Host routing test: %v", err)
	}
	app.SetAddress(listener.Addr())
	proxyServer := proxy.New(repo)
	proxyServer.SetResolver(app.Route)
	invalidator.SetCacheInvalidator(proxyServer.InvalidateCache)
	management := gin.New()
	handler := api.New(app)
	handler.RegisterHealthCheck(management)
	handler.RegisterRoutes(management.Group("/v1"))
	server := &http.Server{Handler: proxy.LocalHandler(listener.Addr(), management, proxyServer.Handler()), ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		if err := <-serveDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("cross-runtime API listener error: %v", err)
		}
	})
	return listener.Addr().(*net.TCPAddr)
}

func exerciseCrossRuntime(t *testing.T, app *service.Service, repo *database.Repository, image, root, manifest string, listener *net.TCPAddr, diagnose func() string) runtimeObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), crossRuntimeTimeout)
	created, err := app.Create(ctx, sandbox.CreateOptions{Image: image, Ports: []sandbox.Port{{Number: crossRuntimeTestPort, Protocol: "tcp"}}, Timeout: 300 * time.Second})
	cancel()
	if err != nil {
		if diagnose != nil {
			t.Logf("Docker image state after failed managed-image create: %s", diagnose())
		}
		t.Fatalf("create sandbox from managed OCI image %s: %v", image, err)
	}
	if !strings.HasPrefix(string(created.ID), "sbx-") {
		t.Fatalf("unexpected public sandbox ID %q", created.ID)
	}
	row, err := repo.FindByID(string(created.ID))
	if err != nil || row == nil {
		t.Fatalf("read sandbox ownership for %s: row=%+v err=%v", created.ID, row, err)
	}
	if row.ID != string(created.ID) || row.NativeID == "" || row.NativeID == row.ID || row.ImageRoot != root || row.ImageManifest != manifest {
		t.Fatalf("public/native/provenance ownership mismatch: row=%+v expected root=%s manifest=%s", row, root, manifest)
	}
	detailCtx, detailCancel := context.WithTimeout(context.Background(), 30*time.Second)
	detail, detailErr := app.Inspect(detailCtx, created.ID)
	detailCancel()
	if detailErr != nil || string(detail.Image) != root {
		t.Fatalf("public image identity for %s=%s err=%v; want canonical OCI root %s", created.ID, detail.Image, detailErr, root)
	}

	const guestPath = "/tmp/opensbx-shared-runtime-proof.txt"
	const fileContent = "shared managed OCI store across runtimes"
	fileCtx, fileCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = app.WriteFile(fileCtx, created.ID, guestPath, fileContent)
	fileCancel()
	if err != nil {
		t.Fatalf("write guest file on %s sandbox %s: %v", runtimeLabelForApp(app), created.ID, err)
	}
	fileCtx, fileCancel = context.WithTimeout(context.Background(), 30*time.Second)
	gotFile, err := app.ReadFile(fileCtx, created.ID, guestPath)
	fileCancel()
	if err != nil || gotFile != fileContent {
		t.Fatalf("read guest file on %s sandbox: content=%q err=%v", runtimeLabelForApp(app), gotFile, err)
	}

	commandCtx, commandCancel := context.WithTimeout(context.Background(), 30*time.Second)
	command, err := app.ExecCommand(commandCtx, created.ID, sandbox.ProcessRequest{Command: "sh", Args: []string{"-c", "printf shared-out; printf shared-err >&2; exit 7"}})
	commandCancel()
	if err != nil {
		t.Fatalf("start shared stdout/stderr/exit command on %s: %v", runtimeLabelForApp(app), err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	finished, err := app.WaitCommand(waitCtx, created.ID, command.ID)
	waitCancel()
	if err != nil || finished.ExitCode == nil || *finished.ExitCode != 7 {
		t.Fatalf("command completion on %s: detail=%+v err=%v", runtimeLabelForApp(app), finished, err)
	}
	logsCtx, logsCancel := context.WithTimeout(context.Background(), 30*time.Second)
	logs, err := app.GetCommandLogs(logsCtx, created.ID, command.ID)
	logsCancel()
	if err != nil || logs.Stdout != "shared-out" || logs.Stderr != "shared-err" {
		t.Fatalf("command output on %s: logs=%+v err=%v", runtimeLabelForApp(app), logs, err)
	}

	if listener != nil {
		program := "require('http').createServer((_,res)=>res.end('docker-host-route-ok')).listen(3000,'0.0.0.0')"
		serveCtx, serveCancel := context.WithTimeout(context.Background(), 30*time.Second)
		serverCommand, serveErr := app.ExecCommand(serveCtx, created.ID, sandbox.ProcessRequest{Command: "node", Args: []string{"-e", program}})
		serveCancel()
		if serveErr != nil {
			t.Fatalf("start Node HTTP service in Docker sandbox: %v", serveErr)
		}
		networkCtx, networkCancel := context.WithTimeout(context.Background(), 30*time.Second)
		network, networkErr := app.GetNetwork(networkCtx, created.ID)
		networkCancel()
		if networkErr != nil {
			t.Fatalf("read Docker published port: %v", networkErr)
		}
		var hostPort uint16
		for _, published := range network.Ports {
			if published.Guest.Number == crossRuntimeTestPort && published.Guest.Protocol == "tcp" {
				hostPort = published.Host
				break
			}
		}
		if hostPort == 0 {
			t.Fatalf("Docker sandbox does not publish test port: %+v", network)
		}
		if err := waitCrossRuntimeNode(uint16(hostPort)); err != nil {
			t.Fatal(err)
		}
		friendlyURL, err := url.Parse(created.URL)
		if err != nil || friendlyURL.Scheme != "http" || friendlyURL.Host != fmt.Sprintf("%s.localhost:%d", created.Name, listener.Port) {
			t.Fatalf("Docker sandbox friendly URL=%q err=%v listener=%d", created.URL, err, listener.Port)
		}
		for _, routePath := range []string{"/", "/v1/health"} {
			request, err := http.NewRequest(http.MethodGet, "http://"+listener.String()+routePath, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = friendlyURL.Host
			response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
			if err != nil {
				t.Fatalf("request Docker app through generated localhost Host at %s: %v", routePath, err)
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "docker-host-route-ok" {
				t.Fatalf("Docker Host-routed %s response status=%d body=%q err=%v; sandbox /v1/* must not reach the management API", routePath, response.StatusCode, body, readErr)
			}
		}
		killCtx, killCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, killErr := app.KillCommand(killCtx, created.ID, serverCommand.ID, 15)
		killCancel()
		if killErr != nil {
			t.Fatalf("stop Docker Node host-routing service: %v", killErr)
		}
	}

	return runtimeObservation{publicID: string(created.ID), nativeID: row.NativeID, name: created.Name, root: string(detail.Image), manifest: row.ImageManifest, file: gotFile, stdout: logs.Stdout, stderr: logs.Stderr, exitCode: *finished.ExitCode}
}

func waitCrossRuntimeNode(hostPort uint16) error {
	deadline := time.Now().Add(20 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d", hostPort))
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && string(body) == "docker-host-route-ok" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("Docker Node service did not become ready on host port %d", hostPort)
}

func runtimeLabelForApp(app *service.Service) string {
	return "runtime-neutral"
}

func removeCrossRuntimeSandbox(t *testing.T, app *service.Service, repo *database.Repository, publicID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := app.Remove(ctx, sandbox.SandboxID(publicID)); err != nil {
		return err
	}
	return nil
}

func verifyOwnershipRemoved(repo *database.Repository, publicID string) error {
	row, err := repo.FindByID(publicID)
	if err != nil {
		return err
	}
	if row != nil {
		return fmt.Errorf("sandbox ownership row %s remains after exact removal", publicID)
	}
	return nil
}

func runExactDockerImageRemoval(t *testing.T, reference string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "image", "rm", reference).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker image rm %s: %w (%s)", reference, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func inspectDockerTestArtifact(client *docker.Client, cacheTag, configDigest string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tagExists, tagErr := client.ImageExists(ctx, cacheTag)
	configExists, configErr := client.ImageExists(ctx, configDigest)
	output, inspectErr := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}} {{.RepoTags}}", cacheTag, configDigest).CombinedOutput()
	return fmt.Sprintf("tag_exists=%t tag_error=%v config_exists=%t config_error=%v inspect_error=%v inspect=%q", tagExists, tagErr, configExists, configErr, inspectErr, strings.TrimSpace(string(output)))
}

func appleImageExists(runner applecontainer.Runner, reference string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var output []byte
	err := runner.Run(ctx, []string{"image", "inspect", reference}, nil, &byteBuffer{data: &output}, &byteBuffer{data: &output})
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(string(output)), "image not found") {
		return false, nil
	}
	return false, fmt.Errorf("container image inspect %s failed: %w (%s)", reference, err, strings.TrimSpace(string(output)))
}

type byteBuffer struct{ data *[]byte }

func (b *byteBuffer) Write(p []byte) (int, error) {
	*b.data = append(*b.data, p...)
	return len(p), nil
}
