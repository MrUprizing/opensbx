//go:build appleintegration

package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/docker"
	"opensbx/models"
)

const appleIntegrationStepTimeout = 20 * time.Second

func appleStep(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), appleIntegrationStepTimeout)
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
	client := New(repo, runner, nil)
	// The fresh temporary repository is the cleanup allow-list. Register before
	// Create so recovery records from a failed rollback are included as well.
	t.Cleanup(func() {
		rows, err := repo.FindAll()
		if err != nil {
			t.Errorf("read isolated Apple integration cleanup records: %v", err)
			return
		}
		for _, row := range rows {
			if !validID(row.ID) {
				t.Errorf("refusing to clean invalid sandbox ID from isolated test database: %q", row.ID)
				continue
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 25*time.Second)
			err := client.Remove(cleanupCtx, row.ID)
			cleanupCancel()
			if err != nil && !errors.Is(err, docker.ErrNotFound) {
				t.Errorf("cleanup of sandbox recorded only in isolated test database (%s) failed: %v", row.ID, err)
			}
		}
	})

	ctx, cancel := appleStep(t)
	err = client.Ping(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Apple service must already be running at validated version 1.4.1: %v", err)
	}
	ctx, cancel = appleStep(t)
	imageInfo, err := client.InspectImage(ctx, image)
	cancel()
	if err != nil {
		t.Fatalf("required image %q is not locally available (automatic pull is disabled): %v", image, err)
	}
	if imageInfo.OS != "linux" || imageInfo.Architecture != "arm64" {
		t.Fatalf("test image must have a local linux/arm64 variant; got %s/%s", imageInfo.OS, imageInfo.Architecture)
	}

	isNode := strings.Contains(strings.ToLower(image), "node")
	ports := []string(nil)
	if isNode {
		ports = []string{"3000/tcp"}
	} else {
		t.Logf("HTTP published-port subtest skipped: %q is not identified as a Node image; core shell/file/exec/lifecycle smoke continues", image)
	}
	ctx, cancel = appleStep(t)
	created, err := client.Create(ctx, models.CreateSandboxRequest{Image: image, Ports: ports, Timeout: 600})
	cancel()
	if err != nil {
		t.Fatalf("create from pre-pulled image without implicit pull: %v", err)
	}
	if created.ID == "" || !validID(created.ID) {
		t.Fatalf("backend returned unexpected non-owned sandbox identifier %q", created.ID)
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

	var serviceCommand models.CommandDetail
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
		hostPort := network.PortsMap["3000/tcp"]
		if hostPort == "" {
			t.Fatalf("published test port missing from backend network map: %+v", network)
		}
		if err := waitForNodeEndpoint(t, hostPort); err != nil {
			t.Fatal(err)
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
	err = client.RenewExpiration(ctx, created.ID, 1)
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
	if row, err := repo.FindByID(created.ID); err != nil || row != nil {
		t.Fatalf("sandbox persistence after exact-ID removal: row=%+v err=%v", row, err)
	}
	ctx, cancel = appleStep(t)
	_, err = client.Inspect(ctx, created.ID)
	cancel()
	if !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("Inspect removed generated sandbox error=%v, want not found", err)
	}
}

func integrationExec(t *testing.T, client *Client, sandbox, command string, args []string) (models.CommandDetail, error) {
	t.Helper()
	ctx, cancel := appleStep(t)
	defer cancel()
	return client.ExecCommand(ctx, sandbox, models.ExecCommandRequest{Command: command, Args: args})
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
