//go:build integration
// +build integration

package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"opensbx/internal/api"
	"opensbx/internal/database"
	"opensbx/internal/docker"
	"opensbx/internal/images"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
	"opensbx/internal/service"
	"opensbx/models"

	"github.com/gin-gonic/gin"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const integrationTestImage = "node:25-alpine"
const integrationLifecycleTimeoutSeconds = 1800

// realRouter builds a Gin engine wired to the real Docker daemon.
func realRouter(t *testing.T) *gin.Engine {
	t.Helper()

	db := database.New(":memory:")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	dc := docker.New(repo)
	if err := dc.Ping(context.Background()); err != nil {
		if os.Getenv("OPENSBX_REQUIRE_DOCKER_INTEGRATION") == "1" {
			t.Fatalf("mandatory Docker integration: Docker unavailable (%v)", err)
		}
		t.Skipf("skipping integration test: Docker unavailable (%v)", err)
	}
	baselineCacheTags, err := dockerCacheTags()
	require.NoError(t, err, "snapshot exact Docker private-cache refs before the integration test")

	store, err := images.Open(t.TempDir())
	require.NoError(t, err)
	adapter := runtimeio.New(dc, dc, repo)
	app, err := service.New(context.Background(), adapter, adapter, store, repo)
	require.NoError(t, err)
	r := gin.New()
	h := api.New(app)
	h.RegisterHealthCheck(r)
	h.RegisterRoutes(r.Group("/v1"))
	// The isolated in-memory repository is the allow-list for every resource,
	// including recovery rows from a Create that failed before returning an ID.
	t.Cleanup(func() {
		rows, err := repo.FindAll()
		if err != nil {
			t.Errorf("read isolated Docker integration cleanup records: %v", err)
			return
		}
		for _, row := range rows {
			if !validIntegrationSandboxID(row.ID) {
				t.Errorf("refusing to clean invalid public ID from isolated integration DB: %q", row.ID)
				continue
			}
			cleanup := do(r, http.MethodDelete, "/v1/sandboxes/"+row.ID, nil)
			if cleanup.Code != http.StatusNoContent && cleanup.Code != http.StatusNotFound {
				t.Errorf("cleanup exact sandbox recorded in isolated integration DB %s/native %s: status=%d body=%s", row.ID, row.NativeID, cleanup.Code, cleanup.Body.String())
			}
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		caps, capsErr := dc.Capabilities(cleanupCtx)
		if capsErr != nil {
			t.Errorf("read isolated Docker cleanup platform: %v", capsErr)
			return
		}
		artifact, resolveErr := store.Resolve(cleanupCtx, integrationTestImage, v1.Platform{OS: caps.Platform.OS, Architecture: caps.Platform.Architecture, Variant: caps.Platform.Variant})
		if errors.Is(resolveErr, sandbox.ErrImageNotFound) {
			return
		}
		if resolveErr != nil {
			t.Errorf("resolve only integration-owned Docker cache for cleanup: %v", resolveErr)
			return
		}
		privateTag := "localhost/opensbx-cache:" + strings.TrimPrefix(artifact.Manifest.Digest.String(), "sha256:")
		if baselineCacheTags[privateTag] {
			return
		}
		exists, inspectErr := dc.ImageExists(cleanupCtx, privateTag)
		if inspectErr != nil {
			t.Errorf("inspect exact test Docker cache ref %s for cleanup: %v", privateTag, inspectErr)
			return
		}
		if exists {
			output, removeErr := exec.CommandContext(cleanupCtx, "docker", "image", "rm", privateTag).CombinedOutput()
			if removeErr != nil {
				t.Errorf("remove only newly introduced Docker cache ref %s: %v (%s)", privateTag, removeErr, strings.TrimSpace(string(output)))
			}
		}
	})
	return r
}

func TestIntegrationStrictModeFailsInsteadOfSkippingUnavailableDocker(t *testing.T) {
	if os.Getenv("OPENSBX_STRICT_DOCKER_CHILD") == "1" {
		t.Setenv("OPENSBX_REQUIRE_DOCKER_INTEGRATION", "1")
		t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
		realRouter(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestIntegrationStrictModeFailsInsteadOfSkippingUnavailableDocker$")
	var env []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "DOCKER_HOST=") || strings.HasPrefix(entry, "OPENSBX_REQUIRE_DOCKER_INTEGRATION=") || strings.HasPrefix(entry, "OPENSBX_STRICT_DOCKER_CHILD=") {
			continue
		}
		env = append(env, entry)
	}
	command.Env = append(env, "DOCKER_HOST=tcp://127.0.0.1:1", "OPENSBX_REQUIRE_DOCKER_INTEGRATION=1", "OPENSBX_STRICT_DOCKER_CHILD=1")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "mandatory Docker integration: Docker unavailable") {
		t.Fatalf("strict unavailable-Docker child error=%v output=%s; want explicit failure, not skip", err, output)
	}
}

func dockerCacheTags() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "image", "ls", "--no-trunc", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, ref := range strings.Split(string(output), "\n") {
		if ref != "<none>:<none>" && ref != "" {
			refs[ref] = true
		}
	}
	return refs, nil
}

func validIntegrationSandboxID(id string) bool {
	if !strings.HasPrefix(id, "sbx-") || len(id) != len("sbx-")+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "sbx-"))
	return err == nil
}

func ensureTestImage(t *testing.T, r *gin.Engine, image string) {
	t.Helper()

	check := do(r, "GET", "/v1/images/"+image, nil)
	if check.Code == http.StatusOK {
		return
	}
	if check.Code != http.StatusNotFound {
		require.FailNowf(t, "image check failed", "check should return 200 or 404: %d %s", check.Code, check.Body.String())
	}

	w := do(r, "POST", "/v1/images/pull", map[string]any{"image": image})
	require.Equal(t, http.StatusOK, w.Code,
		"image %q is not available locally and pull failed: %s", image, w.Body.String())
}

func TestIntegration_FullLifecycle(t *testing.T) {
	r := realRouter(t)
	testImage := integrationTestImage
	ensureTestImage(t, r, testImage)

	// 1. Create a sandbox using a lightweight image.
	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":   testImage,
		"timeout": integrationLifecycleTimeoutSeconds,
	})
	require.Equal(t, http.StatusCreated, w.Code, "create should return 201: %s", w.Body.String())

	var created models.CreateSandboxResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	id := created.ID

	// Cleanup: always remove the sandbox at the end.
	defer func() {
		do(r, "DELETE", "/v1/sandboxes/"+id, nil)
	}()

	// 2. List sandboxes — our container should be there.
	w = do(r, "GET", "/v1/sandboxes", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), id[:12])

	// 3. Inspect the sandbox — should return curated fields.
	w = do(r, "GET", "/v1/sandboxes/"+id, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var detail models.SandboxDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &detail))
	assert.Equal(t, id, detail.ID)
	assert.True(t, detail.Running)
	assert.NotNil(t, detail.ExpiresAt, "sandbox should have an expiration time")

	// 4. Execute a command (async).
	w = do(r, "POST", "/v1/sandboxes/"+id+"/cmd", map[string]any{
		"command": "echo",
		"args":    []string{"hello"},
	})
	assert.Equal(t, http.StatusOK, w.Code)

	var cmdResp models.CommandResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cmdResp))
	assert.NotEmpty(t, cmdResp.Command.ID)
	assert.Equal(t, "echo", cmdResp.Command.Name)
	assert.Nil(t, cmdResp.Command.ExitCode, "exit_code should be nil initially")
	cmdID := cmdResp.Command.ID

	// 5. Wait for command to finish.
	w = do(r, "GET", "/v1/sandboxes/"+id+"/cmd/"+cmdID+"?wait=true", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/x-ndjson")

	// Parse the ND-JSON stream — should have at least one line with exit_code.
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var lastCmd models.CommandResponse
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		json.Unmarshal([]byte(line), &lastCmd)
	}
	require.NotNil(t, lastCmd.Command.ExitCode, "final status should have exit_code")
	assert.Equal(t, 0, *lastCmd.Command.ExitCode)

	// 6. List commands — should have 1 entry.
	w = do(r, "GET", "/v1/sandboxes/"+id+"/cmd", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var listResp models.CommandListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	assert.GreaterOrEqual(t, len(listResp.Commands), 1)

	// 7. Stream logs — should contain "hello".
	w = do(r, "GET", "/v1/sandboxes/"+id+"/cmd/"+cmdID+"/logs", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "hello")

	// 8. Execute a long-running command and kill it.
	w = do(r, "POST", "/v1/sandboxes/"+id+"/cmd", map[string]any{
		"command": "sleep",
		"args":    []string{"3600"},
	})
	assert.Equal(t, http.StatusOK, w.Code)

	var sleepResp models.CommandResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sleepResp))
	sleepID := sleepResp.Command.ID

	// Kill the command.
	w = do(r, "POST", "/v1/sandboxes/"+id+"/cmd/"+sleepID+"/kill", map[string]any{"signal": 15})
	assert.Equal(t, http.StatusOK, w.Code)
	var killedCmd models.CommandResponse
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		status := do(r, "GET", "/v1/sandboxes/"+id+"/cmd/"+sleepID, nil)
		require.Equal(t, http.StatusOK, status.Code, "inspect killed command: %s", status.Body.String())
		require.NoError(t, json.Unmarshal(status.Body.Bytes(), &killedCmd))
		if killedCmd.Command.ExitCode != nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("kill endpoint did not terminate its command within bounded wait; latest response=%s", status.Body.String())
		case <-ticker.C:
		}
	}
	assert.NotEqual(t, 0, *killedCmd.Command.ExitCode, "killed command should have non-zero exit code")

	// Waiting after observing completion must return the final ND-JSON state.
	w = do(r, "GET", "/v1/sandboxes/"+id+"/cmd/"+sleepID+"?wait=true", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	scanner = bufio.NewScanner(strings.NewReader(w.Body.String()))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		json.Unmarshal([]byte(line), &lastCmd)
	}
	require.NotNil(t, lastCmd.Command.ExitCode)
	assert.NotEqual(t, 0, *lastCmd.Command.ExitCode, "killed command should have non-zero exit code")

	// 9. Write a file.
	w = do(r, "PUT", "/v1/sandboxes/"+id+"/files?path=/tmp/test.txt", map[string]any{
		"content": "integration-test",
	})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "written")

	// 10. Read the file back.
	w = do(r, "GET", "/v1/sandboxes/"+id+"/files?path=/tmp/test.txt", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "integration-test")

	// 11. List directory.
	w = do(r, "GET", "/v1/sandboxes/"+id+"/files/list?path=/tmp", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "test.txt")

	// 12. Delete the file.
	w = do(r, "DELETE", "/v1/sandboxes/"+id+"/files?path=/tmp/test.txt", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)

	// 13. Pause the sandbox.
	w = do(r, "POST", "/v1/sandboxes/"+id+"/pause", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "paused")

	// 14. Resume the sandbox.
	w = do(r, "POST", "/v1/sandboxes/"+id+"/resume", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "resumed")

	// 15. Renew expiration.
	w = do(r, "POST", "/v1/sandboxes/"+id+"/renew-expiration", map[string]any{
		"timeout": 120,
	})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "renewed")

	// 16. Restart the sandbox — should return new ports and expiration.
	w = do(r, "POST", "/v1/sandboxes/"+id+"/restart", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var restarted models.RestartResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &restarted))
	assert.Equal(t, "restarted", restarted.Status)
	assert.NotNil(t, restarted.Ports, "restart should return port mappings")
	assert.NotNil(t, restarted.ExpiresAt, "restart should return expiration time")

	// 17. Stop the sandbox.
	w = do(r, "POST", "/v1/sandboxes/"+id+"/stop", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "stopped")

	// 18. Delete the sandbox.
	w = do(r, "DELETE", "/v1/sandboxes/"+id, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)

	// 19. Inspect deleted sandbox should return 404.
	w = do(r, "GET", "/v1/sandboxes/"+id, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestIntegration_ImmediateKillWaitsForExecStartupAndCompletes(t *testing.T) {
	r := realRouter(t)
	ensureTestImage(t, r, integrationTestImage)

	created := do(r, http.MethodPost, "/v1/sandboxes", map[string]any{
		"image":   integrationTestImage,
		"timeout": 300,
	})
	require.Equal(t, http.StatusCreated, created.Code, "create immediate-kill test sandbox: %s", created.Body.String())
	var sandboxCreated models.CreateSandboxResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &sandboxCreated))
	require.NotEmpty(t, sandboxCreated.ID)

	for iteration := 0; iteration < 10; iteration++ {
		started := do(r, http.MethodPost, "/v1/sandboxes/"+sandboxCreated.ID+"/cmd", map[string]any{
			"command": "sleep",
			"args":    []string{"3600"},
		})
		require.Equal(t, http.StatusOK, started.Code, "cycle %d start sleep: %s", iteration, started.Body.String())
		var command models.CommandResponse
		require.NoError(t, json.Unmarshal(started.Body.Bytes(), &command))
		require.NotEmpty(t, command.Command.ID)

		// Deliberately signal immediately after the asynchronous ExecCommand response.
		killed := doIntegrationBounded(t, r, http.MethodPost, "/v1/sandboxes/"+sandboxCreated.ID+"/cmd/"+command.Command.ID+"/kill", map[string]any{"signal": 15}, 5*time.Second)
		require.Equal(t, http.StatusOK, killed.Code, "cycle %d immediate KillCommand: %s", iteration, killed.Body.String())

		waited := doIntegrationBounded(t, r, http.MethodGet, "/v1/sandboxes/"+sandboxCreated.ID+"/cmd/"+command.Command.ID+"?wait=true", nil, 5*time.Second)
		require.Equal(t, http.StatusOK, waited.Code, "cycle %d wait for killed command: %s", iteration, waited.Body.String())
		scanner := bufio.NewScanner(strings.NewReader(waited.Body.String()))
		var finished models.CommandResponse
		for scanner.Scan() {
			if scanner.Text() != "" {
				require.NoError(t, json.Unmarshal(scanner.Bytes(), &finished))
			}
		}
		require.NoError(t, scanner.Err())
		require.NotNil(t, finished.Command.ExitCode, "cycle %d wait=true must return the terminal command state", iteration)
		require.NotEqual(t, 0, *finished.Command.ExitCode, "cycle %d sleep should exit after SIGTERM", iteration)
	}
}

func doIntegrationBounded(t *testing.T, r *gin.Engine, method, target string, body any, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&encoded).Encode(body))
	}
	request, err := http.NewRequest(method, target, &encoded)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	return response
}

func TestIntegration_NotFound(t *testing.T) {
	r := realRouter(t)

	endpoints := []struct {
		method string
		url    string
		body   any
	}{
		{"GET", "/v1/sandboxes/nonexistent", nil},
		{"POST", "/v1/sandboxes/nonexistent/stop", nil},
		{"POST", "/v1/sandboxes/nonexistent/restart", nil},
		{"POST", "/v1/sandboxes/nonexistent/pause", nil},
		{"POST", "/v1/sandboxes/nonexistent/resume", nil},
		{"POST", "/v1/sandboxes/nonexistent/renew-expiration", map[string]any{"timeout": 60}},
		{"POST", "/v1/sandboxes/nonexistent/cmd", map[string]any{"command": "echo"}},
	}

	for _, e := range endpoints {
		w := do(r, e.method, e.url, e.body)
		assert.Equal(t, http.StatusNotFound, w.Code, "%s %s should return 404", e.method, e.url)
	}

	// A missing sandbox is reported as not found; only image deletion treats a
	// missing reference as idempotent when force=true.
	w := do(r, "DELETE", "/v1/sandboxes/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, w.Code, "DELETE nonexistent should return 404")
}

func TestIntegration_DefaultResourceLimits(t *testing.T) {
	r := realRouter(t)
	testImage := integrationTestImage
	ensureTestImage(t, r, testImage)

	// Create a sandbox without specifying resource limits
	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":   testImage,
		"timeout": 60,
	})
	require.Equal(t, http.StatusCreated, w.Code, "create should return 201: %s", w.Body.String())

	var created models.CreateSandboxResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	id := created.ID

	defer func() {
		do(r, "DELETE", "/v1/sandboxes/"+id, nil)
	}()

	// Inspect the sandbox to verify default resource limits.
	w = do(r, "GET", "/v1/sandboxes/"+id, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var detailResp models.SandboxDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &detailResp))

	// Verify defaults: 1GB RAM, 1 vCPU
	assert.Equal(t, int64(1024), detailResp.Resources.Memory, "Default memory should be 1024 MB")
	assert.Equal(t, 1.0, detailResp.Resources.CPUs, "Default CPUs should be 1.0")
}

func TestIntegration_ImagePull(t *testing.T) {
	r := realRouter(t)

	testImage := integrationTestImage

	w := do(r, "POST", "/v1/images/pull", map[string]any{
		"image": testImage,
	})
	require.Equal(t, http.StatusOK, w.Code, "pull should return 200: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "pulled")
	assert.Contains(t, w.Body.String(), testImage)

	var response models.ImagePullResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "pulled", response.Status)
	assert.Equal(t, testImage, response.Image)
}
