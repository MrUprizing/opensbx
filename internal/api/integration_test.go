//go:build integration
// +build integration

package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"testing"

	"open-sandbox/internal/api"
	"open-sandbox/internal/database"
	"open-sandbox/internal/docker"
	"open-sandbox/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const integrationTestImage = "node:25-alpine"

// realRouter builds a Gin engine wired to the real Docker daemon.
func realRouter(t *testing.T) *gin.Engine {
	t.Helper()

	db := database.New(":memory:")
	repo := database.NewRepository(db)
	dc := docker.New(repo)
	if err := dc.Ping(context.Background()); err != nil {
		t.Skipf("skipping integration test: Docker unavailable (%v)", err)
	}

	r := gin.New()
	h := api.New(dc, "localhost", ":3000")
	h.RegisterHealthCheck(r)
	h.RegisterRoutes(r.Group("/v1"))
	return r
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
		"timeout": 300,
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

	// Wait and verify it exited with non-zero.
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

	// DELETE is idempotent: removing a nonexistent sandbox cleans DB and returns 204.
	w := do(r, "DELETE", "/v1/sandboxes/nonexistent", nil)
	assert.Equal(t, http.StatusNoContent, w.Code, "DELETE nonexistent should return 204")
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
