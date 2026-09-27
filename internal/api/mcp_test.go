package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"opensbx/internal/sandbox"
	"opensbx/models"
)

type mcpDockerStub struct {
	sandbox.Application
	fail           bool
	waitError      error
	createRequest  sandbox.CreateOptions
	execSandboxID  string
	execRequest    sandbox.ProcessRequest
	waitSandboxID  string
	waitCommandID  string
	readSandboxID  string
	readPath       string
	writeSandboxID string
	writePath      string
	writeContent   string
}

func (s *mcpDockerStub) maybeFail() error {
	if s.fail {
		return errors.New("docker fixture failure")
	}
	return nil
}

func (s *mcpDockerStub) Ping(context.Context) error { return s.maybeFail() }
func (s *mcpDockerStub) List(context.Context) ([]sandbox.Summary, error) {
	return []sandbox.Summary{{ID: "sb-1", Name: "demo"}}, s.maybeFail()
}
func (s *mcpDockerStub) Create(_ context.Context, req sandbox.CreateOptions) (sandbox.Created, error) {
	s.createRequest = req
	return sandbox.Created{ID: "sb-1", Name: "demo", URL: "http://demo.localhost:3000"}, s.maybeFail()
}
func (s *mcpDockerStub) Inspect(context.Context, sandbox.SandboxID) (sandbox.Detail, error) {
	return sandbox.Detail{Summary: sandbox.Summary{ID: "sb-1", Name: "demo"}}, s.maybeFail()
}
func (s *mcpDockerStub) Start(context.Context, sandbox.SandboxID) (sandbox.Started, error) {
	return sandbox.Started{Status: "started"}, s.maybeFail()
}
func (s *mcpDockerStub) Stop(context.Context, sandbox.SandboxID) error { return s.maybeFail() }
func (s *mcpDockerStub) Restart(context.Context, sandbox.SandboxID) (sandbox.Started, error) {
	return sandbox.Started{Status: "restarted"}, s.maybeFail()
}
func (s *mcpDockerStub) GetNetwork(context.Context, sandbox.SandboxID) (sandbox.Network, error) {
	return sandbox.Network{Main: sandbox.Port{Number: 3000, Protocol: "tcp"}}, s.maybeFail()
}
func (s *mcpDockerStub) Remove(context.Context, sandbox.SandboxID) error { return s.maybeFail() }
func (s *mcpDockerStub) Pause(context.Context, sandbox.SandboxID) error  { return s.maybeFail() }
func (s *mcpDockerStub) Resume(context.Context, sandbox.SandboxID) error { return s.maybeFail() }
func (s *mcpDockerStub) RenewExpiration(context.Context, sandbox.SandboxID, time.Duration) error {
	return s.maybeFail()
}
func (s *mcpDockerStub) ExecCommand(_ context.Context, sandboxID sandbox.SandboxID, req sandbox.ProcessRequest) (sandbox.Command, error) {
	s.execSandboxID, s.execRequest = string(sandboxID), req
	return sandbox.Command{ID: "cmd-1", Name: req.Command, SandboxID: sandboxID}, s.maybeFail()
}
func (s *mcpDockerStub) GetCommand(context.Context, sandbox.SandboxID, sandbox.CommandID) (sandbox.Command, error) {
	return sandbox.Command{ID: "cmd-1"}, s.maybeFail()
}
func (s *mcpDockerStub) ListCommands(context.Context, sandbox.SandboxID) ([]sandbox.Command, error) {
	return []sandbox.Command{{ID: "cmd-1"}}, s.maybeFail()
}
func (s *mcpDockerStub) KillCommand(context.Context, sandbox.SandboxID, sandbox.CommandID, int) (sandbox.Command, error) {
	return sandbox.Command{ID: "cmd-1"}, s.maybeFail()
}
func (s *mcpDockerStub) GetCommandLogs(context.Context, sandbox.SandboxID, sandbox.CommandID) (sandbox.Logs, error) {
	return sandbox.Logs{Stdout: "hello"}, s.maybeFail()
}
func (s *mcpDockerStub) WaitCommand(_ context.Context, sandboxID sandbox.SandboxID, commandID sandbox.CommandID) (sandbox.Command, error) {
	s.waitSandboxID, s.waitCommandID = string(sandboxID), string(commandID)
	if s.waitError != nil {
		return sandbox.Command{}, s.waitError
	}
	return sandbox.Command{ID: "cmd-1", Name: "done"}, s.maybeFail()
}
func (s *mcpDockerStub) Stats(context.Context, sandbox.SandboxID) (sandbox.Stats, error) {
	return sandbox.Stats{CPU: 1.5}, s.maybeFail()
}
func (s *mcpDockerStub) ReadFile(_ context.Context, sandboxID sandbox.SandboxID, path string) (string, error) {
	s.readSandboxID, s.readPath = string(sandboxID), path
	return "contents", s.maybeFail()
}
func (s *mcpDockerStub) WriteFile(_ context.Context, sandboxID sandbox.SandboxID, path, content string) error {
	s.writeSandboxID, s.writePath, s.writeContent = string(sandboxID), path, content
	return s.maybeFail()
}
func (s *mcpDockerStub) DeleteFile(context.Context, sandbox.SandboxID, string) error {
	return s.maybeFail()
}
func (s *mcpDockerStub) ListDir(_ context.Context, _ sandbox.SandboxID, path string) (string, error) {
	return path, s.maybeFail()
}
func (s *mcpDockerStub) PullImage(context.Context, string) error         { return s.maybeFail() }
func (s *mcpDockerStub) RemoveImage(context.Context, string, bool) error { return s.maybeFail() }
func (s *mcpDockerStub) InspectImage(context.Context, string) (sandbox.ImageDetail, error) {
	return sandbox.ImageDetail{ImageSummary: sandbox.ImageSummary{ID: "image-1"}}, s.maybeFail()
}
func (s *mcpDockerStub) ListImages(context.Context) ([]sandbox.ImageSummary, error) {
	return []sandbox.ImageSummary{{ID: "image-1"}}, s.maybeFail()
}

func newMCPTestSession(t *testing.T, d sandbox.Application) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	addMCPTools(server, &facade{app: d})
	addMCPContext(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("connect test MCP server: %v", err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect test MCP client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestMCPToolsExposeAndExecutePublicOperations(t *testing.T) {
	d := &mcpDockerStub{}
	session := newMCPTestSession(t, d)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools() error: %v", err)
	}
	if len(tools.Tools) != 26 {
		t.Fatalf("ListTools() returned %d tools, want 26", len(tools.Tools))
	}

	cases := []struct {
		name string
		args map[string]any
	}{
		{"system_health", map[string]any{}},
		{"sandbox_list", map[string]any{}},
		{"sandbox_create", map[string]any{"image": "alpine", "ports": []string{"3000"}, "timeout": 60, "env": []string{"A=B"}}},
		{"sandbox_get", map[string]any{"id": "sb-1"}},
		{"sandbox_delete", map[string]any{"id": "sb-1"}},
		{"sandbox_start", map[string]any{"id": "sb-1"}},
		{"sandbox_stop", map[string]any{"id": "sb-1"}},
		{"sandbox_restart", map[string]any{"id": "sb-1"}},
		{"sandbox_pause", map[string]any{"id": "sb-1"}},
		{"sandbox_resume", map[string]any{"id": "sb-1"}},
		{"sandbox_renew_expiration", map[string]any{"id": "sb-1", "timeout": 120}},
		{"sandbox_stats", map[string]any{"id": "sb-1"}},
		{"sandbox_network_get", map[string]any{"id": "sb-1"}},
		{"command_exec", map[string]any{"sandbox_id": "sb-1", "command": "echo", "args": []string{"hi"}, "cwd": "/tmp", "env": map[string]string{"A": "B"}, "wait": true}},
		{"command_list", map[string]any{"id": "sb-1"}},
		{"command_get", map[string]any{"sandbox_id": "sb-1", "command_id": "cmd-1", "wait": true}},
		{"command_kill", map[string]any{"sandbox_id": "sb-1", "command_id": "cmd-1", "signal": 15}},
		{"command_logs", map[string]any{"sandbox_id": "sb-1", "command_id": "cmd-1"}},
		{"file_read", map[string]any{"sandbox_id": "sb-1", "path": "/tmp/a"}},
		{"file_write", map[string]any{"sandbox_id": "sb-1", "path": "/tmp/a", "content": "a"}},
		{"file_delete", map[string]any{"sandbox_id": "sb-1", "path": "/tmp/a"}},
		{"file_list", map[string]any{"sandbox_id": "sb-1", "path": ""}},
		{"image_list", map[string]any{}},
		{"image_get", map[string]any{"id": "image-1"}},
		{"image_pull", map[string]any{"image": "alpine"}},
		{"image_delete", map[string]any{"id": "image-1", "force": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool() error: %v", err)
			}
			if result.IsError {
				t.Fatalf("CallTool() returned tool error: %+v", result.Content)
			}
			if len(result.Content) == 0 {
				t.Fatal("CallTool() returned no content")
			}
		})
	}
	if got := d.createRequest; got.Image != "alpine" || got.Timeout != 60*time.Second || len(got.Ports) != 1 || got.Ports[0].String() != "3000/tcp" || len(got.Env) != 1 || got.Env[0] != "A=B" {
		t.Fatalf("sandbox_create() request mapping = %+v", got)
	}
	if got := d.createRequest.Resources; got != (sandbox.ResourceLimits{}) {
		t.Fatalf("sandbox_create() unexpectedly passed unspecified resources: %+v", got)
	}
	if d.execSandboxID != "sb-1" || d.execRequest.Command != "echo" || len(d.execRequest.Args) != 1 || d.execRequest.Args[0] != "hi" || d.execRequest.Cwd != "/tmp" || d.execRequest.Env["A"] != "B" || d.waitSandboxID != "sb-1" || d.waitCommandID != "cmd-1" {
		t.Fatalf("command_exec() mapping: sandbox=%q request=%+v wait=%q/%q", d.execSandboxID, d.execRequest, d.waitSandboxID, d.waitCommandID)
	}
	if d.readSandboxID != "sb-1" || d.readPath != "/tmp/a" || d.writeSandboxID != "sb-1" || d.writePath != "/tmp/a" || d.writeContent != "a" {
		t.Fatalf("file tool mappings: read=%q/%q write=%q/%q/%q", d.readSandboxID, d.readPath, d.writeSandboxID, d.writePath, d.writeContent)
	}
	commandResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "command_exec", Arguments: map[string]any{"sandbox_id": "sb-1", "command": "echo", "args": []string{"hi"}, "cwd": "/tmp", "env": map[string]string{"A": "B"}, "wait": true}})
	if err != nil || commandResult.IsError {
		t.Fatalf("command_exec() result = %+v, %v", commandResult, err)
	}
	var commandResponse models.CommandResponse
	if err := json.Unmarshal([]byte(commandResult.Content[0].(*mcp.TextContent).Text), &commandResponse); err != nil || commandResponse.Command.ID != "cmd-1" || commandResponse.Command.Name != "done" {
		t.Fatalf("command_exec() structured response = %+v, decode error %v", commandResponse, err)
	}
	fileResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "file_read", Arguments: map[string]any{"sandbox_id": "sb-1", "path": "/tmp/a"}})
	if err != nil || fileResult.IsError {
		t.Fatalf("file_read() result = %+v, %v", fileResult, err)
	}
	var fileResponse models.FileReadResponse
	if err := json.Unmarshal([]byte(fileResult.Content[0].(*mcp.TextContent).Text), &fileResponse); err != nil || fileResponse.Path != "/tmp/a" || fileResponse.Content != "contents" {
		t.Fatalf("file_read() structured response = %+v, decode error %v", fileResponse, err)
	}
	writeResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "file_write", Arguments: map[string]any{"sandbox_id": "sb-1", "path": "/tmp/a", "content": "body"}})
	if err != nil || writeResult.IsError {
		t.Fatalf("file_write() result = %+v, %v", writeResult, err)
	}
	var writeResponse map[string]string
	if err := json.Unmarshal([]byte(writeResult.Content[0].(*mcp.TextContent).Text), &writeResponse); err != nil || writeResponse["path"] != "/tmp/a" || writeResponse["status"] != "written" {
		t.Fatalf("file_write() structured response = %v, decode error %v", writeResponse, err)
	}
	created, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "sandbox_create", Arguments: map[string]any{"image": "alpine"}})
	if err != nil || created.IsError {
		t.Fatalf("sandbox_create() error = %v, result = %+v", err, created)
	}
	var response models.CreateSandboxResponse
	if err := json.Unmarshal([]byte(created.Content[0].(*mcp.TextContent).Text), &response); err != nil {
		t.Fatalf("decode create result: %v", err)
	}
	if response.URL != "http://demo.localhost:3000" {
		t.Fatalf("create URL = %q, want local proxy URL", response.URL)
	}
}

func TestMCPToolsReportValidationAndDockerErrors(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"sandbox_create", map[string]any{"image": ""}},
		{"sandbox_create", map[string]any{"image": "alpine", "timeout": -1}},
		{"sandbox_create", map[string]any{"image": "alpine", "resources": map[string]any{"memory": 9000, "cpus": 1.0}}},
		{"sandbox_create", map[string]any{"image": "alpine", "resources": map[string]any{"memory": 1, "cpus": 5.0}}},
		{"sandbox_get", map[string]any{"id": ""}},
		{"sandbox_delete", map[string]any{"id": ""}},
		{"sandbox_start", map[string]any{"id": ""}},
		{"sandbox_stop", map[string]any{"id": ""}},
		{"sandbox_restart", map[string]any{"id": ""}},
		{"sandbox_pause", map[string]any{"id": ""}},
		{"sandbox_resume", map[string]any{"id": ""}},
		{"sandbox_renew_expiration", map[string]any{"id": "sb-1", "timeout": 0}},
		{"sandbox_stats", map[string]any{"id": ""}},
		{"sandbox_network_get", map[string]any{"id": ""}},
		{"command_exec", map[string]any{"sandbox_id": "sb-1", "command": ""}},
		{"command_exec", map[string]any{"sandbox_id": "", "command": "echo"}},
		{"command_list", map[string]any{"id": ""}},
		{"command_get", map[string]any{"sandbox_id": "sb-1", "command_id": ""}},
		{"command_kill", map[string]any{"sandbox_id": "sb-1", "command_id": "cmd-1", "signal": 0}},
		{"command_logs", map[string]any{"sandbox_id": "sb-1", "command_id": ""}},
		{"file_read", map[string]any{"sandbox_id": "sb-1", "path": ""}},
		{"file_write", map[string]any{"sandbox_id": "sb-1", "path": "", "content": ""}},
		{"file_delete", map[string]any{"sandbox_id": "sb-1", "path": ""}},
		{"file_list", map[string]any{"sandbox_id": "", "path": "/"}},
		{"image_get", map[string]any{"id": ""}},
		{"image_pull", map[string]any{"image": ""}},
		{"image_delete", map[string]any{"id": ""}},
	}
	session := newMCPTestSession(t, &mcpDockerStub{})
	for i, tc := range cases {
		t.Run(tc.name+string(rune('a'+i)), func(t *testing.T) {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err == nil && !result.IsError {
				t.Fatalf("CallTool(%s) should report invalid arguments", tc.name)
			}
		})
	}

	failingSession := newMCPTestSession(t, &mcpDockerStub{fail: true})
	for _, call := range []struct {
		name string
		args map[string]any
	}{{"system_health", map[string]any{}}, {"sandbox_list", map[string]any{}}, {"file_read", map[string]any{"sandbox_id": "sb", "path": "/x"}}, {"image_list", map[string]any{}}} {
		result, err := failingSession.CallTool(context.Background(), &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			t.Fatalf("CallTool(%s) transport error: %v", call.name, err)
		}
		if !result.IsError {
			t.Errorf("CallTool(%s) should surface Docker failure", call.name)
		}
	}
}

func TestMCPResourcesAndPromptExposeGuidance(t *testing.T) {
	session := newMCPTestSession(t, &mcpDockerStub{})
	resources, err := session.ListResources(context.Background(), &mcp.ListResourcesParams{})
	if err != nil || len(resources.Resources) != 2 {
		t.Fatalf("ListResources() = %v, %v; want two docs", resources, err)
	}
	for _, uri := range []string{"opensbx://docs/how-it-works", "opensbx://docs/quickstart"} {
		got, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err != nil || len(got.Contents) != 1 || got.Contents[0].Text == "" {
			t.Fatalf("ReadResource(%q) = %v, %v", uri, got, err)
		}
	}
	if _, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "opensbx://docs/missing"}); err == nil {
		t.Fatal("ReadResource() for unknown URI should fail")
	}
	prompt, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "sandbox_workflow", Arguments: map[string]string{"goal": "run tests"}})
	if err != nil || len(prompt.Messages) != 1 {
		t.Fatalf("GetPrompt() = %v, %v", prompt, err)
	}
	text := prompt.Messages[0].Content.(*mcp.TextContent).Text
	if !containsAll(text, "Goal: run tests", "Sandbox ID: <create_one_first>") {
		t.Fatalf("prompt omitted user context/default sandbox guidance: %s", text)
	}
	prompt, err = session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "sandbox_workflow", Arguments: map[string]string{"goal": "inspect", "sandbox_id": "sb-7"}})
	if err != nil || !containsAll(prompt.Messages[0].Content.(*mcp.TextContent).Text, "Sandbox ID: sb-7") {
		t.Fatalf("prompt with sandbox id = %v, %v", prompt, err)
	}
}

func TestMCPBackendFailuresAreToolErrorsNotSuccessfulPayloads(t *testing.T) {
	// Each tool has its own handler: cover returned-value and mutation-only
	// operations across sandbox lifecycle, commands, files and images.
	session := newMCPTestSession(t, &mcpDockerStub{fail: true})
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"sandbox_create", map[string]any{"image": "alpine"}},
		{"sandbox_get", map[string]any{"id": "sb-1"}},
		{"sandbox_delete", map[string]any{"id": "sb-1"}},
		{"sandbox_start", map[string]any{"id": "sb-1"}},
		{"sandbox_stop", map[string]any{"id": "sb-1"}},
		{"sandbox_restart", map[string]any{"id": "sb-1"}},
		{"sandbox_pause", map[string]any{"id": "sb-1"}},
		{"sandbox_resume", map[string]any{"id": "sb-1"}},
		{"sandbox_renew_expiration", map[string]any{"id": "sb-1", "timeout": 60}},
		{"sandbox_stats", map[string]any{"id": "sb-1"}},
		{"sandbox_network_get", map[string]any{"id": "sb-1"}},
		{"command_exec", map[string]any{"sandbox_id": "sb-1", "command": "echo"}},
		{"command_get", map[string]any{"sandbox_id": "sb-1", "command_id": "cmd-1"}},
		{"command_list", map[string]any{"id": "sb-1"}},
		{"command_kill", map[string]any{"sandbox_id": "sb-1", "command_id": "cmd-1", "signal": 15}},
		{"command_logs", map[string]any{"sandbox_id": "sb-1", "command_id": "cmd-1"}},
		{"file_write", map[string]any{"sandbox_id": "sb-1", "path": "/work/file", "content": "text"}},
		{"file_delete", map[string]any{"sandbox_id": "sb-1", "path": "/work/file"}},
		{"file_list", map[string]any{"sandbox_id": "sb-1", "path": "/work"}},
		{"image_get", map[string]any{"id": "alpine"}},
		{"image_pull", map[string]any{"image": "alpine"}},
		{"image_delete", map[string]any{"id": "alpine"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("backend failure became a protocol error: %v", err)
			}
			if !result.IsError || len(result.Content) != 1 {
				t.Fatalf("expected one tool error, got %+v", result)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !strings.Contains(text.Text, "docker fixture failure") {
				t.Fatalf("tool error lost backend failure: %+v", result.Content)
			}
			if result.StructuredContent != nil {
				t.Fatalf("failed operation returned a successful structured payload: %+v", result.StructuredContent)
			}
		})
	}
}

func TestMCPWaitFailuresDoNotReturnAnUnfinishedCommandAsSuccess(t *testing.T) {
	for _, tool := range []string{"command_exec", "command_get"} {
		t.Run(tool, func(t *testing.T) {
			d := &mcpDockerStub{waitError: errors.New("waiting interrupted")}
			session := newMCPTestSession(t, d)
			args := map[string]any{"sandbox_id": "sb-1", "wait": true}
			if tool == "command_exec" {
				args["command"] = "sleep"
			} else {
				args["command_id"] = "cmd-1"
			}
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || len(result.Content) != 1 {
				t.Fatalf("expected wait failure, got %+v", result)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !strings.Contains(text.Text, "waiting interrupted") {
				t.Fatalf("missing wait error: %+v", result.Content)
			}
			if d.waitSandboxID != "sb-1" || d.waitCommandID != "cmd-1" {
				t.Fatalf("wait routed to %q/%q", d.waitSandboxID, d.waitCommandID)
			}
		})
	}
}

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
