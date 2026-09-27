//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
	"opensbx/internal/applecontainer"
	"opensbx/internal/images"
	"opensbx/models"
)

const importedImage = "e2e-imported:local"

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	for _, scenario := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"AuthenticationAndOrigins", h.authentication},
		{"ImagesPullExportImport", h.images},
		{"SandboxCommandsFilesDomainAndLifecycle", h.sandboxWorkflow},
		{"ExpirationRenewal", h.expiration},
		{"MCPOverHTTP", h.mcpWorkflow},
		{"ShutdownAndPersistence", h.persistence},
	} {
		if !t.Run(scenario.name, scenario.run) {
			return // Dependent scenarios must not obscure the first failure.
		}
	}
	require.NoError(t, h.emptyDB(), "normal API cleanup must remove all sandbox and command records")
}

func (h *harness) authentication(t *testing.T) {
	for _, key := range []string{"", "wrong-key"} {
		status, _, err := h.request("GET", "/v1/sandboxes", nil, key, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, status)
		status, _, err = h.request("POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}, key, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, status)
	}
	h.api(t, "GET", "/v1/sandboxes", nil, 200, nil)
	status, _, err := h.request("GET", "/v1/sandboxes", nil, h.key, map[string]string{"Origin": "http://foreign.example"})
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, status)
}

func (h *harness) images(t *testing.T) {
	h.api(t, "POST", "/v1/sandboxes", map[string]any{"image": "e2e-missing:local"}, 400, nil)
	h.api(t, "POST", "/v1/images/pull", map[string]any{"image": workload}, 200, nil)
	var image models.ImageDetail
	h.api(t, "GET", "/v1/images/"+workload, nil, 200, &image)
	require.Equal(t, "linux", image.OS)
	require.Positive(t, image.Size)
	require.NotEmpty(t, image.ID)
	listed := h.api(t, "GET", "/v1/images", nil, 200, nil)
	require.Contains(t, string(listed), image.ID)

	// Determine the exact private cache handle before any native materialization,
	// including creates that could fail before committing ownership metadata.
	store, err := images.Open(h.data)
	require.NoError(t, err)
	platform, err := v1.ParsePlatform(h.platform)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	artifact, err := store.Resolve(ctx, workload, *platform)
	require.NoError(t, err)
	h.cache = "localhost/opensbx-cache:" + strings.TrimPrefix(artifact.Manifest.Digest.String(), "sha256:")
	if h.runtime == "container" {
		h.cache, err = applecontainer.CacheReference(artifact.Manifest.Digest.String())
		require.NoError(t, err)
	} else if !h.baseline[h.cache] {
		// Untagging our new cache could also delete a pre-existing dangling config.
		// Refuse that ambiguous ownership case before native materialization.
		config, err := artifact.Image.ConfigName()
		require.NoError(t, err)
		b, inspectErr := h.native("image", "inspect", config.String())
		require.Error(t, inspectErr, "workload config already exists without its managed cache tag; use an isolated Docker daemon to guarantee cleanup preserves existing images")
		require.Contains(t, string(b), "No such image", "must establish absence, not treat a runtime failure as absence")
	}
	archive := filepath.Join(h.root, "workload.tar")
	h.cli(t, "export", "--output", archive, workload)
	h.cli(t, "remove", workload)
	h.api(t, "GET", "/v1/images/"+workload, nil, 404, nil)
	h.cli(t, "import", "--reference", importedImage, archive)
	h.cli(t, "inspect", importedImage)
	h.api(t, "GET", "/v1/images/"+importedImage, nil, 200, &image)
	require.Equal(t, "linux", image.OS)
	require.Equal(t, platform.Architecture, image.Architecture)
	imported, err := store.Resolve(ctx, importedImage, *platform)
	require.NoError(t, err)
	require.Equal(t, artifact.Manifest.Digest, imported.Manifest.Digest, "archive round trip must retain the executable artifact")
}

func (h *harness) create(t *testing.T, ports []string) models.CreateSandboxResponse {
	t.Helper()
	var created models.CreateSandboxResponse
	h.api(t, "POST", "/v1/sandboxes", models.CreateSandboxRequest{Image: importedImage, Ports: ports, Timeout: 300}, 201, &created)
	require.NoError(t, h.captureOwnership())
	require.Contains(t, h.owned, created.ID)
	require.NotEqual(t, created.ID, h.owned[created.ID])
	states, err := h.inventory()
	require.NoError(t, err)
	require.Equal(t, "running", states[h.owned[created.ID]], "the created sandbox must actually run in the selected runtime")
	return created
}

func (h *harness) inspect(t *testing.T, id string) models.SandboxDetail {
	t.Helper()
	var detail models.SandboxDetail
	h.api(t, "GET", "/v1/sandboxes/"+id, nil, 200, &detail)
	return detail
}

func (h *harness) command(t *testing.T, id, command string, args ...string) models.CommandDetail {
	t.Helper()
	var response models.CommandResponse
	h.api(t, "POST", "/v1/sandboxes/"+id+"/cmd", models.ExecCommandRequest{Command: command, Args: args}, 200, &response)
	require.NotEmpty(t, response.Command.ID)
	return response.Command
}

func (h *harness) waitCommand(t *testing.T, id, cmd string) models.CommandDetail {
	t.Helper()
	var response models.CommandResponse
	eventually(t, 20*time.Second, "command must terminate", func() (bool, string) {
		h.api(t, "GET", "/v1/sandboxes/"+id+"/cmd/"+cmd, nil, 200, &response)
		return response.Command.ExitCode != nil, fmt.Sprintf("command=%+v", response.Command)
	})
	return response.Command
}

func (h *harness) remove(t *testing.T, id string) {
	t.Helper()
	h.api(t, "DELETE", "/v1/sandboxes/"+id, nil, 204, nil)
	h.api(t, "GET", "/v1/sandboxes/"+id, nil, 404, nil)
	states, err := h.inventory()
	require.NoError(t, err)
	require.NotContains(t, states, h.owned[id], "API deletion must remove the real resource")
}

func (h *harness) startApp(t *testing.T, id, marker string) {
	t.Helper()
	script := fmt.Sprintf(`require('http').createServer((req,res)=>{res.setHeader('Content-Type','text/plain');res.end(%q+'|'+req.url)}).listen(3000,'0.0.0.0')`, marker)
	h.api(t, "PUT", "/v1/sandboxes/"+id+"/files?path=/tmp/app.js", models.FileWriteRequest{Content: script}, 200, nil)
	h.command(t, id, "node", "/tmp/app.js")
}

func expectApp(t *testing.T, rawURL, marker string) {
	t.Helper()
	eventually(t, 20*time.Second, "returned sandbox domain must serve its app", func() (bool, string) {
		status, body, err := appRequest(rawURL)
		return err == nil && status == 200 && body == marker, fmt.Sprintf("url=%s status=%d body=%q err=%v", rawURL, status, body, err)
	})
}

func expectUnavailable(t *testing.T, rawURL string) {
	t.Helper()
	status, body, err := appRequest(rawURL)
	require.NoError(t, err, "the proxy itself must remain reachable")
	require.GreaterOrEqual(t, status, 400, "stopped/deleted sandbox served success: %s", body)
}

func (h *harness) sandboxWorkflow(t *testing.T) {
	sb := h.create(t, []string{"3000"})
	detail := h.inspect(t, sb.ID)
	require.True(t, detail.Running)
	require.Equal(t, sb.URL, detail.URL)
	require.NotEmpty(t, sb.URL)
	u, err := url.Parse(sb.URL)
	require.NoError(t, err)
	require.Equal(t, sb.Name+".localhost", u.Hostname())
	control, err := url.Parse(h.endpoint)
	require.NoError(t, err)
	require.Equal(t, control.Port(), u.Port())
	require.Contains(t, string(h.api(t, "GET", "/v1/sandboxes", nil, 200, nil)), sb.ID)
	var network models.SandboxNetwork
	h.api(t, "GET", "/v1/sandboxes/"+sb.ID+"/network", nil, 200, &network)
	require.Equal(t, "3000/tcp", network.MainPort)
	require.NotEmpty(t, network.PortsMap[network.MainPort])
	var stats models.SandboxStats
	h.api(t, "GET", "/v1/sandboxes/"+sb.ID+"/stats", nil, 200, &stats)
	require.Positive(t, stats.Memory.Limit)

	cmd := h.command(t, sb.ID, "sh", "-c", "printf 'e2e-stdout'; printf 'e2e-stderr' >&2; exit 7")
	finished := h.waitCommand(t, sb.ID, cmd.ID)
	require.Equal(t, 7, *finished.ExitCode)
	var logs models.CommandLogsResponse
	h.api(t, "GET", "/v1/sandboxes/"+sb.ID+"/cmd/"+cmd.ID+"/logs", nil, 200, &logs)
	require.Equal(t, "e2e-stdout", logs.Stdout)
	require.Equal(t, "e2e-stderr", logs.Stderr)
	stream := h.api(t, "GET", "/v1/sandboxes/"+sb.ID+"/cmd/"+cmd.ID+"/logs?stream=true", nil, 200, nil)
	require.Contains(t, string(stream), "e2e-stdout")
	require.Contains(t, string(stream), "e2e-stderr")
	long := h.command(t, sb.ID, "sleep", "300")
	h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/cmd/"+long.ID+"/kill", map[string]int{"signal": 15}, 200, nil)
	require.NotZero(t, *h.waitCommand(t, sb.ID, long.ID).ExitCode)

	file := "/v1/sandboxes/" + sb.ID + "/files?path=/tmp/e2e.txt"
	h.api(t, "PUT", file, models.FileWriteRequest{Content: sb.ID}, 200, nil)
	var read models.FileReadResponse
	h.api(t, "GET", file, nil, 200, &read)
	require.Equal(t, sb.ID, read.Content)
	require.Contains(t, string(h.api(t, "GET", "/v1/sandboxes/"+sb.ID+"/files/list?path=/tmp", nil, 200, nil)), "e2e.txt")

	h.startApp(t, sb.ID, sb.ID)
	for _, path := range []string{"/", "/v1/health", "/swagger/index.html", "/v1/mcp"} {
		expectApp(t, sb.URL+path, sb.ID+"|"+path)
	}
	if h.runtime == "docker" {
		h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/pause", nil, 200, nil)
		h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/resume", nil, 200, nil)
		expectApp(t, sb.URL+"/", sb.ID+"|/")
	} else {
		unsupported := h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/pause", nil, 400, nil)
		require.Contains(t, string(unsupported), "unsupported")
	}
	h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/stop", nil, 200, nil)
	require.False(t, h.inspect(t, sb.ID).Running)
	expectUnavailable(t, sb.URL)
	h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/start", nil, 200, nil)
	h.api(t, "GET", file, nil, 200, &read)
	require.Equal(t, sb.ID, read.Content, "stop/start must retain files")
	h.startApp(t, sb.ID, sb.ID)
	expectApp(t, h.inspect(t, sb.ID).URL+"/", sb.ID+"|/")
	h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/restart", nil, 200, nil)
	h.startApp(t, sb.ID, sb.ID)
	expectApp(t, h.inspect(t, sb.ID).URL+"/", sb.ID+"|/")
	h.api(t, "DELETE", file, nil, 204, nil)
	deleted := h.command(t, sb.ID, "sh", "-c", "test ! -e /tmp/e2e.txt")
	require.Zero(t, *h.waitCommand(t, sb.ID, deleted.ID).ExitCode, "delete must remove the actual guest file")
	h.remove(t, sb.ID)
	expectUnavailable(t, sb.URL)
}

func (h *harness) expiration(t *testing.T) {
	sb := h.create(t, nil)
	path := "/v1/sandboxes/" + sb.ID + "/renew-expiration"
	h.api(t, "POST", path, map[string]int{"timeout": 4}, 200, nil)
	old := h.inspect(t, sb.ID).ExpiresAt
	require.NotNil(t, old)
	h.api(t, "POST", path, map[string]int{"timeout": 9}, 200, nil)
	newExpiry := h.inspect(t, sb.ID).ExpiresAt
	require.NotNil(t, newExpiry)
	require.True(t, newExpiry.After(*old))
	eventually(t, 7*time.Second, "sandbox must survive the superseded deadline", func() (bool, string) {
		detail := h.inspect(t, sb.ID)
		require.True(t, detail.Running, "sandbox stopped before renewed expiration")
		return time.Now().After(old.Add(300 * time.Millisecond)), "waiting for original expiration"
	})
	eventually(t, 20*time.Second, "renewed expiration must stop the real sandbox", func() (bool, string) {
		return !h.inspect(t, sb.ID).Running, "sandbox still running"
	})
	states, err := h.inventory()
	require.NoError(t, err)
	require.Contains(t, []string{"stopped", "exited"}, states[h.owned[sb.ID]])
	h.remove(t, sb.ID)
}

func (h *harness) persistence(t *testing.T) {
	sb := h.create(t, nil)
	file := "/v1/sandboxes/" + sb.ID + "/files?path=/tmp/persist.txt"
	h.api(t, "PUT", file, models.FileWriteRequest{Content: sb.ID}, 200, nil)
	h.command(t, sb.ID, "sleep", "300")
	require.NoError(t, h.stop(), "SIGTERM must gracefully stop the real process")
	states, err := h.inventory()
	require.NoError(t, err)
	require.Contains(t, []string{"stopped", "exited"}, states[h.owned[sb.ID]], "shutdown must stop the native sandbox")
	h.start(t)
	detail := h.inspect(t, sb.ID)
	require.Equal(t, sb.ID, detail.ID)
	require.False(t, detail.Running)
	h.api(t, "POST", "/v1/sandboxes/"+sb.ID+"/start", nil, 200, nil)
	var read models.FileReadResponse
	h.api(t, "GET", file, nil, 200, &read)
	require.Equal(t, sb.ID, read.Content)
	require.Contains(t, string(h.api(t, "GET", "/v1/sandboxes/"+sb.ID+"/cmd", nil, 200, nil)), "sleep")
	h.remove(t, sb.ID)
	h.cli(t, "remove", importedImage)
	var remaining []models.ImageSummary
	require.NoError(t, json.Unmarshal(h.cli(t, "list"), &remaining))
	require.Empty(t, remaining, "managed image catalog must be empty before directory cleanup")
}
