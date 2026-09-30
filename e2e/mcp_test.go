//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"opensbx/models"
)

type bearerTransport struct {
	base http.RoundTripper
	key  string
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(clone)
}

func (h *harness) mcpWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := mcp.NewClient(&mcp.Implementation{Name: "opensbx-e2e", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: h.endpoint + "/v1/mcp", MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearerTransport{transport, h.key}, Timeout: 60 * time.Second},
	}, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, session.Close()) }()
	tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	require.NoError(t, err)
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	require.Subset(t, names, []string{"sandbox_create", "command_exec", "file_write", "file_read", "sandbox_delete"})
	call := func(name string, args map[string]any, into any) {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err, name)
		require.False(t, result.IsError, "%s: %+v", name, result.Content)
		if into != nil {
			require.NotEmpty(t, result.Content)
			text, ok := result.Content[0].(*mcp.TextContent)
			require.True(t, ok)
			require.NoError(t, json.Unmarshal([]byte(text.Text), into))
		}
	}
	if h.cliSandboxID != "" {
		var fromCLI models.SandboxDetail
		call("sandbox_get", map[string]any{"id": h.cliSandboxID}, &fromCLI)
		require.Equal(t, h.cliSandboxID, fromCLI.ID, "MCP must inspect the resource created and used by CLI")
		call("sandbox_delete", map[string]any{"id": h.cliSandboxID}, nil)
		h.api(t, "GET", "/v1/sandboxes/"+h.cliSandboxID, nil, 404, nil)
		states, err := h.inventory()
		require.NoError(t, err)
		require.NotContains(t, states, h.owned[h.cliSandboxID], "MCP must delete the same CLI-owned native resource")
		h.cliSandboxID = ""
	}
	var sb models.CreateSandboxResponse
	call("sandbox_create", map[string]any{"image": importedImage, "timeout": 300}, &sb)
	require.NoError(t, h.captureOwnership())
	require.Contains(t, h.owned, sb.ID)
	var command models.CommandResponse
	call("command_exec", map[string]any{"sandbox_id": sb.ID, "command": "echo", "args": []string{sb.ID}, "wait": true}, &command)
	require.NotNil(t, command.Command.ExitCode)
	require.Zero(t, *command.Command.ExitCode)
	var logs models.CommandLogsResponse
	call("command_logs", map[string]any{"sandbox_id": sb.ID, "command_id": command.Command.ID}, &logs)
	require.Equal(t, sb.ID+"\n", logs.Stdout)
	call("file_write", map[string]any{"sandbox_id": sb.ID, "path": "/tmp/mcp.txt", "content": sb.ID}, nil)
	var read models.FileReadResponse
	call("file_read", map[string]any{"sandbox_id": sb.ID, "path": "/tmp/mcp.txt"}, &read)
	require.Equal(t, sb.ID, read.Content)
	call("sandbox_delete", map[string]any{"id": sb.ID}, nil)
	h.api(t, "GET", "/v1/sandboxes/"+sb.ID, nil, 404, nil)
	states, err := h.inventory()
	require.NoError(t, err)
	require.NotContains(t, states, h.owned[sb.ID])
}
