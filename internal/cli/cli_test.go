package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedRequest struct {
	method, uri string
	body        map[string]any
}

func cliFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request)) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	return server, strings.TrimPrefix(server.URL, "http://")
}

func TestSixShortAliasesIssueSameOperationsAsGroupedCommands(t *testing.T) {
	for _, tc := range []struct {
		name        string
		short, full []string
	}{
		{"create", []string{"create", "node:22"}, []string{"sandbox", "create", "node:22"}},
		{"list", []string{"ls"}, []string{"sandbox", "list"}},
		{"inspect", []string{"inspect", "demo"}, []string{"sandbox", "inspect", "demo"}},
		{"exec", []string{"exec", "demo", "--", "node", "--help", "-h", "", "-leading", "line\nnext"}, []string{"command", "exec", "demo", "--", "node", "--help", "-h", "", "-leading", "line\nnext"}},
		{"logs", []string{"logs", "demo", "cmd-1"}, []string{"command", "logs", "demo", "cmd-1"}},
		{"remove", []string{"rm", "demo"}, []string{"sandbox", "remove", "demo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(args []string) ([]recordedRequest, string, string, error) {
				var mu sync.Mutex
				var requests []recordedRequest
				server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if r.Body != nil {
						_ = json.NewDecoder(r.Body).Decode(&body)
					}
					mu.Lock()
					requests = append(requests, recordedRequest{method: r.Method, uri: r.RequestURI, body: body})
					mu.Unlock()
					serveCLIResponse(w, r)
				})
				_ = server
				var out, stderr bytes.Buffer
				if delimiter := indexOf(args, "--"); delimiter >= 0 {
					args = append(args[:delimiter], append([]string{"--addr", addr}, args[delimiter:]...)...)
				} else {
					args = append(args, "--addr", addr)
				}
				err := Run(context.Background(), args, strings.NewReader(""), &out, &stderr)
				sort.Slice(requests, func(i, j int) bool {
					return requests[i].method+requests[i].uri < requests[j].method+requests[j].uri
				})
				return requests, out.String(), stderr.String(), err
			}
			shortRequests, shortOut, shortErr, err := run(tc.short)
			if err != nil {
				t.Fatalf("short command: %v (stderr %q)", err, shortErr)
			}
			fullRequests, fullOut, fullErr, err := run(tc.full)
			if err != nil {
				t.Fatalf("grouped command: %v (stderr %q)", err, fullErr)
			}
			if !equalJSON(t, shortRequests, fullRequests) || shortOut != fullOut {
				t.Fatalf("alias mismatch\nshort requests=%#v output=%q\nfull requests=%#v output=%q", shortRequests, shortOut, fullRequests, fullOut)
			}
			if shortErr != fullErr {
				t.Fatalf("diagnostics differ: short=%q grouped=%q", shortErr, fullErr)
			}
			if tc.name == "exec" {
				for _, request := range shortRequests {
					if request.method == http.MethodPost && request.uri == "/v1/sandboxes/sbx-123/cmd" {
						args, _ := request.body["args"].([]any)
						if !equalJSON(t, args, []string{"--help", "-h", "", "-leading", "line\nnext"}) {
							t.Fatalf("guest args not preserved: %#v", request.body)
						}
					}
				}
			}
		})
	}
}

func serveCLIResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/sandboxes" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo","image":"node:22","status":"running"}]}`)
	case r.URL.Path == "/v1/sandboxes" && r.Method == http.MethodPost:
		_, _ = io.WriteString(w, `{"id":"sbx-123","name":"demo"}`)
	case r.URL.Path == "/v1/sandboxes/sbx-123" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/v1/sandboxes/sbx-123" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"id":"sbx-123","name":"demo"}`)
	case r.URL.Path == "/v1/sandboxes/sbx-123/cmd" && r.Method == http.MethodPost:
		_, _ = io.WriteString(w, `{"command":{"id":"cmd-1","sandbox_id":"sbx-123","name":"node"}}`)
	case r.URL.Path == "/v1/sandboxes/sbx-123/cmd/cmd-1" && r.URL.Query().Get("wait") == "true":
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"command\":{\"id\":\"cmd-1\",\"sandbox_id\":\"sbx-123\",\"exit_code\":0}}\n")
	case r.URL.Path == "/v1/sandboxes/sbx-123/cmd/cmd-1/logs" && r.URL.Query().Get("stream") == "true":
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"hello\\n\"}\n")
	case r.URL.Path == "/v1/sandboxes/sbx-123/cmd/cmd-1/logs":
		_, _ = io.WriteString(w, `{"stdout":"hello\n","stderr":""}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"unexpected route"}`)
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

func TestCreatePayloadIncludesRepeatedOptionsAndValidatedLimits(t *testing.T) {
	var got map[string]any
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes" {
			_ = json.NewDecoder(r.Body).Decode(&got)
		}
		serveCLIResponse(w, r)
	})
	defer server.Close()
	var out, stderr bytes.Buffer
	args := []string{"create", "--addr", addr, "--port", "3000", "node:22", "-p", "8080/udp", "--ttl", "90s", "--memory", "1GiB", "--cpus", "1.5", "-e", "A=first,second", "--env", "A=last,comma", "-e", "B="}
	err := Run(context.Background(), args, strings.NewReader(""), &out, &stderr)
	if err != nil {
		t.Fatalf("Run args=%q: %v; stderr=%s", args, err, stderr.String())
	}
	if got["image"] != "node:22" || got["timeout"] != float64(90) {
		t.Fatalf("create body = %#v", got)
	}
	ports, _ := got["ports"].([]any)
	if !equalJSON(t, ports, []string{"3000", "8080/udp"}) {
		t.Errorf("ports = %#v", ports)
	}
	env, _ := got["env"].([]any)
	if !equalJSON(t, env, []string{"A=first,second", "A=last,comma", "B="}) {
		t.Errorf("env = %#v", env)
	}
	resources, _ := got["resources"].(map[string]any)
	if resources["memory"] != float64(1024) || resources["cpus"] != 1.5 {
		t.Errorf("resources = %#v", resources)
	}
}

func TestCreateRejectsInvalidOptionsBeforeAnyHTTPRequest(t *testing.T) {
	requests := 0
	server, addr := cliFixture(t, func(http.ResponseWriter, *http.Request) { requests++ })
	defer server.Close()
	for _, args := range [][]string{
		{"create", "node:22", "--ttl", "1.5s"},
		{"create", "node:22", "--ttl", "-1s"},
		{"create", "node:22", "--memory", "512MB"},
		{"create", "node:22", "--memory", "8193MiB"},
		{"create", "node:22", "--cpus", "NaN"},
		{"create", "node:22", "--cpus", "4.1"},
		{"create", "node:22", "--port", "65536"},
		{"create", "node:22", "--port", "80/sctp"},
		{"create", "node:22", "--port", "3000,8080"},
		{"create", "node:22", "--env", "=empty"},
		{"create", "node:22", "--env", "A=bad\x00value"},
	} {
		var out, stderr bytes.Buffer
		withAddr := append(append([]string(nil), args...), "--addr", addr)
		if err := Run(context.Background(), withAddr, strings.NewReader(""), &out, &stderr); err == nil {
			t.Errorf("Run(%q) unexpectedly succeeded", args)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid local options made %d HTTP requests", requests)
	}
}

func TestExecRejectsMissingWorkingDirectoryBeforeStartingGuestCommand(t *testing.T) {
	commandCalls := 0
	var execBody map[string]any
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sandboxes" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"}]}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sbx-123/cmd" {
			commandCalls++
			_ = json.NewDecoder(r.Body).Decode(&execBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"command":{"id":"cmd-1","sandbox_id":"sbx-123"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer server.Close()
	var out, stderr bytes.Buffer
	err := Run(context.Background(), []string{"exec", "demo", "--addr", addr, "--cwd", "--", "node", "--version"}, strings.NewReader(""), &out, &stderr)
	if err == nil {
		t.Fatalf("exec accepted --cwd without a value and started %d guest commands", commandCalls)
	}
	if commandCalls != 0 {
		t.Fatalf("missing --cwd value started %d guest commands with body=%#v (stderr=%q)", commandCalls, execBody, stderr.String())
	}
}

func TestResourceCommandMatrixUsesExpectedHTTPVerbPathAndPayload(t *testing.T) {
	tests := []struct {
		name, method, uri string
		args              []string
		body              map[string]any
	}{
		{"sandbox inspect", http.MethodGet, "/v1/sandboxes/sbx-123", []string{"sandbox", "inspect", "demo"}, nil},
		{"sandbox start", http.MethodPost, "/v1/sandboxes/sbx-123/start", []string{"sandbox", "start", "demo"}, nil},
		{"sandbox stop", http.MethodPost, "/v1/sandboxes/sbx-123/stop", []string{"sandbox", "stop", "demo"}, nil},
		{"sandbox restart", http.MethodPost, "/v1/sandboxes/sbx-123/restart", []string{"sandbox", "restart", "demo"}, nil},
		{"sandbox pause", http.MethodPost, "/v1/sandboxes/sbx-123/pause", []string{"sandbox", "pause", "demo"}, nil},
		{"sandbox resume", http.MethodPost, "/v1/sandboxes/sbx-123/resume", []string{"sandbox", "resume", "demo"}, nil},
		{"sandbox renew", http.MethodPost, "/v1/sandboxes/sbx-123/renew-expiration", []string{"sandbox", "renew", "demo", "--ttl", "2m"}, map[string]any{"timeout": float64(120)}},
		{"sandbox stats", http.MethodGet, "/v1/sandboxes/sbx-123/stats", []string{"sandbox", "stats", "demo"}, nil},
		{"sandbox network", http.MethodGet, "/v1/sandboxes/sbx-123/network", []string{"sandbox", "network", "demo"}, nil},
		{"command list", http.MethodGet, "/v1/sandboxes/sbx-123/cmd", []string{"command", "list", "demo"}, nil},
		{"command inspect", http.MethodGet, "/v1/sandboxes/sbx-123/cmd/cmd-9", []string{"command", "inspect", "demo", "cmd-9"}, nil},
		{"command wait", http.MethodGet, "/v1/sandboxes/sbx-123/cmd/cmd-9?wait=true", []string{"command", "wait", "demo", "cmd-9"}, nil},
		{"command kill", http.MethodPost, "/v1/sandboxes/sbx-123/cmd/cmd-9/kill", []string{"command", "kill", "demo", "cmd-9", "--signal", "SIGINT"}, map[string]any{"signal": float64(2)}},
		{"command logs snapshot", http.MethodGet, "/v1/sandboxes/sbx-123/cmd/cmd-9/logs", []string{"command", "logs", "demo", "cmd-9"}, nil},
		{"command logs follow", http.MethodGet, "/v1/sandboxes/sbx-123/cmd/cmd-9/logs?stream=true", []string{"command", "logs", "demo", "cmd-9", "--follow"}, nil},
		{"command exec detached", http.MethodPost, "/v1/sandboxes/sbx-123/cmd", []string{"command", "exec", "demo", "--detach", "--cwd", "/work", "-e", "A=B", "--", "node", "--version"}, map[string]any{"command": "node", "args": []any{"--version"}, "cwd": "/work", "env": map[string]any{"A": "B"}}},
		{"file read", http.MethodGet, "/v1/sandboxes/sbx-123/files?path=%2Ftmp%2Fa+b", []string{"file", "read", "demo", "/tmp/a b"}, nil},
		{"file list default root", http.MethodGet, "/v1/sandboxes/sbx-123/files/list?path=%2F", []string{"file", "list", "demo"}, nil},
		{"file write", http.MethodPut, "/v1/sandboxes/sbx-123/files?path=%2Ftmp%2Fx", []string{"file", "write", "demo", "/tmp/x"}, map[string]any{"content": "text"}},
		{"file remove", http.MethodDelete, "/v1/sandboxes/sbx-123/files?path=%2Ftmp%2Fx", []string{"file", "remove", "demo", "/tmp/x"}, nil},
		{"sandbox remove", http.MethodDelete, "/v1/sandboxes/sbx-123", []string{"sandbox", "remove", "demo"}, nil},
		{"health", http.MethodGet, "/v1/health", []string{"health"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var actual []recordedRequest
			server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if r.Body != nil {
					_ = json.NewDecoder(r.Body).Decode(&body)
				}
				mu.Lock()
				actual = append(actual, recordedRequest{r.Method, r.RequestURI, body})
				mu.Unlock()
				if r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"}]}`)
					return
				}
				if r.URL.Query().Get("wait") == "true" {
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = io.WriteString(w, "{\"command\":{\"id\":\"cmd-9\",\"sandbox_id\":\"sbx-123\",\"exit_code\":0}}\n")
					return
				}
				if r.URL.Query().Get("stream") == "true" {
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"out\"}\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/sandboxes/sbx-123/cmd/cmd-9/logs":
					_, _ = io.WriteString(w, `{"stdout":"out","stderr":"err"}`)
				case "/v1/sandboxes/sbx-123/cmd/cmd-9":
					_, _ = io.WriteString(w, `{"command":{"id":"cmd-9","sandbox_id":"sbx-123","exit_code":0}}`)
				case "/v1/sandboxes/sbx-123/cmd":
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, `{"commands":[]}`)
					} else {
						_, _ = io.WriteString(w, `{"command":{"id":"cmd-9","sandbox_id":"sbx-123"}}`)
					}
				case "/v1/sandboxes/sbx-123/files":
					_, _ = io.WriteString(w, `{"path":"/tmp/a b","content":"text"}`)
				case "/v1/sandboxes/sbx-123/files/list":
					_, _ = io.WriteString(w, `{"path":"/","output":"entry\n"}`)
				default:
					_, _ = io.WriteString(w, `{}`)
				}
			})
			defer server.Close()
			args := append([]string(nil), tc.args...)
			if delimiter := indexOf(args, "--"); delimiter >= 0 {
				args = append(args[:delimiter], append([]string{"--addr", addr}, args[delimiter:]...)...)
			} else {
				args = append(args, "--addr", addr)
			}
			input := ""
			if tc.name == "file write" {
				input = "text"
			}
			var out, stderr bytes.Buffer
			err := Run(context.Background(), args, strings.NewReader(input), &out, &stderr)
			if err != nil {
				t.Fatalf("Run(%q): %v; stderr=%q", args, err, stderr.String())
			}
			var selected []recordedRequest
			for _, request := range actual {
				if request.uri != "/v1/sandboxes" {
					selected = append(selected, request)
				}
			}
			if len(selected) != 1 {
				t.Fatalf("actual requests = %#v; wanted one operation request", actual)
			}
			got := selected[0]
			if got.method != tc.method || got.uri != tc.uri || !equalJSON(t, got.body, tc.body) {
				t.Fatalf("operation request = %+v, want %s %s body=%#v", got, tc.method, tc.uri, tc.body)
			}
		})
	}
}

func TestFileWritePreservesEmptyUnicodeInputAndEncodedGuestPath(t *testing.T) {
	for _, input := range []string{"", "line\n終 €"} {
		var requestPath string
		var body map[string]any
		server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"}]}`)
				return
			}
			requestPath = r.RequestURI
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"written"}`)
		})
		var out, stderr bytes.Buffer
		err := Run(context.Background(), []string{"file", "write", "demo", "/tmp/a b/終.txt", "--addr", addr}, strings.NewReader(input), &out, &stderr)
		server.Close()
		if err != nil {
			t.Fatalf("write %q: %v", input, err)
		}
		if !strings.Contains(requestPath, "path=%2Ftmp%2Fa+b%2F%E7%B5%82.txt") || body["content"] != input {
			t.Errorf("request URI=%q body=%#v want exact %q", requestPath, body, input)
		}
	}
}

func TestFileWriteRejectsInvalidUTF8AndInteractiveInputBeforeHTTP(t *testing.T) {
	requests := 0
	server, addr := cliFixture(t, func(http.ResponseWriter, *http.Request) { requests++ })
	defer server.Close()
	var out, stderr bytes.Buffer
	err := Run(context.Background(), []string{"file", "write", "demo", "/tmp/x", "--addr", addr}, strings.NewReader(string([]byte{0xff})), &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 input error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("invalid input made %d requests", requests)
	}
}

func TestHelpAndNativeCompletionAreOfflineAndShellSpecific(t *testing.T) {
	for _, tc := range []struct {
		shell   string
		markers []string
	}{
		{"bash", []string{"# bash completion V2 for opensbx", "__start_opensbx()", "complete -o default -F __start_opensbx opensbx"}},
		{"zsh", []string{"#compdef opensbx", "_arguments"}},
		{"fish", []string{"# fish completion for opensbx", "complete -c opensbx -e", "__opensbx_perform_completion"}},
	} {
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"completion", tc.shell}, strings.NewReader(""), &out, io.Discard); err != nil {
			t.Fatalf("completion %s: %v", tc.shell, err)
		}
		for _, marker := range tc.markers {
			if !strings.Contains(out.String(), marker) {
				t.Errorf("native %s completion omitted %q: %s", tc.shell, marker, out.String())
			}
		}
	}
	var help bytes.Buffer
	if err := Run(context.Background(), []string{"exec", "--help"}, strings.NewReader(""), &help, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"--detach", "cwd", "--env", "addr"} {
		if !strings.Contains(help.String(), item) {
			t.Errorf("exec help omitted %s", item)
		}
	}
	var suggestions bytes.Buffer
	if err := Complete(nil, &suggestions); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(suggestions.String(), "sandbox") || !strings.Contains(suggestions.String(), "exec") {
		t.Fatalf("offline root completion missing common commands: %q", suggestions.String())
	}
}

func TestDynamicCompletionReadsSandboxHintsWithoutMutation(t *testing.T) {
	var calls []string
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sandboxes" {
			t.Errorf("completion made unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"},{"id":"unsafe","name":"bad name\u001b"}]}`)
	})
	defer server.Close()
	t.Setenv("ADDR", addr)
	var out bytes.Buffer
	if err := Complete([]string{"inspect", "sbx"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !equalJSON(t, calls, []string{"GET /v1/sandboxes"}) {
		t.Fatalf("completion requests = %v, want one read-only list", calls)
	}
	if !equalJSON(t, lines, []string{"sbx-123", ":4"}) {
		t.Fatalf("prefix-filtered/sanitized completion suggestions = %q", out.String())
	}
	calls = nil
	out.Reset()
	if err := Complete([]string{"exec", "demo", "--", "--he"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("completion after guest -- queried management API: %v", calls)
	}
}

func TestCLIUsesAPIKeyAndKeepsDataOutputSeparateFromDiagnostics(t *testing.T) {
	t.Setenv("API_KEY", "cli-token")
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer cli-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxes":[]}`)
	})
	defer server.Close()
	var out, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"ls", "--json", "--addr", addr}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != `{"sandboxes":[]}` || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
}

func TestHumanMetadataEscapesTerminalControlsButJSONPreservesValues(t *testing.T) {
	const hostile = "name\x1b[31mred\x1b[0m\x1b]52;c;payload\x07"
	value := map[string]any{
		"sandbox": map[string]any{"name": hostile},
		"command": map[string]any{"args": []any{hostile}},
	}
	var humanOutput bytes.Buffer
	if err := output(&humanOutput, value, false); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(humanOutput.String(), "\x1b\x07") {
		t.Fatalf("human metadata contains terminal control characters: %q", humanOutput.String())
	}
	var jsonOutput bytes.Buffer
	if err := output(&jsonOutput, value, true); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	command := decoded["command"].(map[string]any)
	args := command["args"].([]any)
	if args[0] != hostile {
		t.Fatalf("JSON data changed source metadata: got %q, want %q", args[0], hostile)
	}
}

func TestSandboxListEscapesHumanNamesAndPreservesJSONNames(t *testing.T) {
	const hostile = "sandbox\x1b[31mred\x1b[0m\x1b]0;owned\x07"
	server, addr := cliFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []map[string]string{{"id": "sbx-1", "name": hostile, "image": "node", "status": "running"}}})
	})
	defer server.Close()
	for _, tc := range []struct {
		name string
		args []string
		json bool
	}{
		{"human", []string{"ls"}, false},
		{"json", []string{"ls", "--json"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string(nil), tc.args...), "--addr", addr)
			var out, stderr bytes.Buffer
			if err := Run(context.Background(), args, strings.NewReader(""), &out, &stderr); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.json {
				var result struct {
					Sandboxes []map[string]any `json:"sandboxes"`
				}
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Sandboxes) != 1 || result.Sandboxes[0]["name"] != hostile {
					t.Fatalf("JSON list changed source name: %q err=%v", out.String(), err)
				}
			} else if strings.ContainsAny(out.String(), "\x1b\x07") {
				t.Fatalf("human sandbox list contains terminal controls: %q", out.String())
			}
		})
	}
}

func TestForegroundJSONDoesNotSucceedWhenLogStreamReportsTruncation(t *testing.T) {
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sbx-123/cmd":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"command":{"id":"cmd-1","sandbox_id":"sbx-123"}}`)
		case r.URL.Query().Get("wait") == "true":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, "{\"command\":{\"id\":\"cmd-1\",\"sandbox_id\":\"sbx-123\",\"exit_code\":0}}\n")
		case r.URL.Query().Get("stream") == "true":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, "{\"type\":\"error\",\"data\":\"log history was overwritten\"}\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	var out, stderr bytes.Buffer
	err := Run(context.Background(), []string{"exec", "demo", "--json", "--addr", addr, "--", "node", "script.js"}, strings.NewReader(""), &out, &stderr)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "error") {
		t.Fatalf("foreground accepted truncated log stream: err=%v stdout=%q", err, out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("foreground emitted successful/partial JSON after stream truncation: %q", out.String())
	}
}

func TestFileListHumanOutputEscapesControlsWhileReadAndJSONPreserveText(t *testing.T) {
	const hostile = "entry\x1b[2J\x1b]0;owned\x07\tvalue\n"
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sandboxes" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/sandboxes/sbx-123/files/list" {
			_ = json.NewEncoder(w).Encode(map[string]string{"path": "/tmp", "output": hostile})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"path": "/tmp/x", "content": hostile})
	})
	defer server.Close()
	for _, tc := range []struct {
		name     string
		args     []string
		wantRaw  bool
		wantJSON bool
	}{
		{"human list", []string{"file", "list", "demo", "/tmp"}, false, false},
		{"json list", []string{"file", "list", "demo", "/tmp", "--json"}, true, true},
		{"human read", []string{"file", "read", "demo", "/tmp/x"}, true, false},
		{"json read", []string{"file", "read", "demo", "/tmp/x", "--json"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string(nil), tc.args...), "--addr", addr)
			var out, stderr bytes.Buffer
			if err := Run(context.Background(), args, strings.NewReader(""), &out, &stderr); err != nil {
				t.Fatalf("Run: %v; stderr=%q", err, stderr.String())
			}
			if tc.wantJSON {
				var response map[string]string
				if err := json.Unmarshal(out.Bytes(), &response); err != nil {
					t.Fatalf("JSON response %q: %v", out.String(), err)
				}
				key := "content"
				if strings.Contains(tc.name, "list") {
					key = "output"
				}
				if response[key] != hostile {
					t.Fatalf("JSON %s changed source text: %q", key, response[key])
				}
			} else if tc.wantRaw {
				if out.String() != hostile {
					t.Fatalf("raw file read changed source bytes: got %q, want %q", out.String(), hostile)
				}
			} else if strings.ContainsAny(out.String(), "\x1b\x07") {
				t.Fatalf("human file listing contains terminal controls: %q", out.String())
			}
		})
	}
}

func TestForegroundJSONCaptureHasEightMiBTotalBound(t *testing.T) {
	const limit = 8 << 20
	for _, tc := range []struct {
		name       string
		stdoutSize int
		stderrSize int
		wantErr    bool
	}{
		{"exact aggregate limit", limit / 2, limit / 2, false},
		{"aggregate across both streams exceeds limit by one", limit / 2, limit/2 + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canceled := make(chan struct{})
			server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"}]}`)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sbx-123/cmd":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"command":{"id":"cmd-1","sandbox_id":"sbx-123"}}`)
				case r.URL.Query().Get("wait") == "true":
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = io.WriteString(w, "{\"command\":{\"id\":\"cmd-1\",\"sandbox_id\":\"sbx-123\",\"exit_code\":0}}\n")
				case r.URL.Query().Get("stream") == "true":
					w.Header().Set("Content-Type", "application/x-ndjson")
					flusher, _ := w.(http.Flusher)
					encoder := json.NewEncoder(w)
					chunk := strings.Repeat("x", 64<<10)
					write := func(kind string, size int) error {
						for remaining := size; remaining > 0; {
							part := chunk
							if remaining < len(part) {
								part = part[:remaining]
							}
							if err := encoder.Encode(map[string]string{"type": kind, "data": part}); err != nil {
								return err
							}
							remaining -= len(part)
						}
						return nil
					}
					if err := write("stdout", tc.stdoutSize); err != nil {
						return
					}
					if err := write("stderr", tc.stderrSize); err != nil {
						return
					}
					if flusher != nil {
						flusher.Flush()
					}
					if tc.wantErr {
						select {
						case <-r.Context().Done():
							close(canceled)
						case <-time.After(15 * time.Second):
						}
					}
				case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sbx-123/cmd/cmd-1/kill":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
					w.WriteHeader(http.StatusNotFound)
				}
			})
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			err := Run(ctx, []string{"exec", "demo", "--json", "--addr", addr, "--", "node", "script.js"}, strings.NewReader(""), &out, &stderr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("oversized aggregate unexpectedly succeeded, outputBytes=%d", out.Len())
				}
				if out.Len() != 0 {
					t.Fatalf("overflow emitted partial/success JSON (%d bytes)", out.Len())
				}
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("oversized JSON capture did not cancel its log observer")
				}
				if !strings.Contains(strings.ToLower(err.Error()), "limit") && !strings.Contains(strings.ToLower(err.Error()), "8 mib") {
					t.Fatalf("oversized aggregate error=%v; want clear capture-limit failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact-boundary JSON exec failed: %v; stderr=%q", err, stderr.String())
			}
			var result struct {
				Stdout string `json:"stdout"`
				Stderr string `json:"stderr"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("exact-boundary output is not one JSON object: %v", err)
			}
			if len(result.Stdout)+len(result.Stderr) != limit || len(result.Stdout) != tc.stdoutSize || len(result.Stderr) != tc.stderrSize {
				t.Fatalf("captured stdout/stderr sizes = %d/%d; want %d/%d", len(result.Stdout), len(result.Stderr), tc.stdoutSize, tc.stderrSize)
			}
		})
	}
}

func TestForegroundCancellationSignalsOnlyOwnedCommandAndReturns130(t *testing.T) {
	observations := make(chan struct{}, 2)
	var mu sync.Mutex
	var requests []string
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-123","name":"demo"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sbx-123/cmd":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"command":{"id":"cmd-1","sandbox_id":"sbx-123"}}`)
		case r.URL.Query().Get("wait") == "true":
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, `{"command":{"id":"cmd-1","sandbox_id":"sbx-123"}}`)
			w.(http.Flusher).Flush()
			observations <- struct{}{}
			<-r.Context().Done()
		case r.URL.Query().Get("stream") == "true":
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			observations <- struct{}{}
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cmd/cmd-1/kill"):
			var body map[string]int
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["signal"] != 2 {
				t.Errorf("interrupt kill body = %#v", body)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out, stderr bytes.Buffer
	go func() {
		done <- Run(ctx, []string{"exec", "demo", "--addr", addr, "--", "sleep", "60"}, strings.NewReader(""), &out, &stderr)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-observations:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("foreground exec did not start both observation streams")
		}
	}
	cancel()
	select {
	case err := <-done:
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != 130 {
			t.Fatalf("foreground cancellation error = %v, want exit 130", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreground exec did not finish cancellation cleanup")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range requests {
		if strings.Contains(request, "/stop") || strings.Contains(request, "/sandboxes/sbx-123/kill") {
			t.Fatalf("interrupt affected sandbox or non-command resource: %s", request)
		}
	}
	killCount := 0
	for _, request := range requests {
		if request == "POST /v1/sandboxes/sbx-123/cmd/cmd-1/kill" {
			killCount++
		}
	}
	if killCount != 1 {
		t.Fatalf("owned command SIGINT requests=%d, requests=%v", killCount, requests)
	}
}

func TestGlobalAddressWorksBeforeCommandAfterCommandAndBetweenGroupAndLeaf(t *testing.T) {
	requests := 0
	server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sandboxes" {
			t.Errorf("unexpected management request: %s %s", r.Method, r.URL.RequestURI())
		}
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxes":[]}`)
	})
	defer server.Close()
	for _, args := range [][]string{
		{"--addr", addr, "ls"},
		{"ls", "--addr", addr},
		{"sandbox", "--addr", addr, "list"},
		{"sandbox", "list", "--addr", addr},
		{"-addr", addr, "ls"},
	} {
		var out, stderr bytes.Buffer
		if err := Run(context.Background(), args, strings.NewReader(""), &out, &stderr); err != nil {
			t.Errorf("Run(%q): %v stderr=%q", args, err, stderr.String())
		}
	}
	if requests != 5 {
		t.Fatalf("address position variants made %d requests, want 5", requests)
	}
}

func TestExecuteHooksStayOfflineForHelpAndMalformedCommands(t *testing.T) {
	hookCalls := map[string]int{}
	hooks := ServerHooks{
		Foreground: func([]string, io.Writer) error { hookCalls["foreground"]++; return nil },
		Start:      func([]string, io.Writer) error { hookCalls["start"]++; return nil },
		Stop:       func([]string, io.Writer) error { hookCalls["stop"]++; return nil },
	}
	for _, args := range [][]string{
		{"--help"}, {"help", "start"}, {"start", "--help"}, {"stop", "--help"},
		{"start", "unexpected"}, {"start", "--unknown"}, {"stop", "extra"}, {"sandbox", "create"},
	} {
		var out, stderr bytes.Buffer
		err := Execute(context.Background(), args, strings.NewReader(""), &out, &stderr, hooks)
		isHelp := len(args) == 1 && args[0] == "--help" || len(args) == 2 && (args[1] == "--help" || args[0] == "help")
		if isHelp && err != nil {
			t.Errorf("Execute(%q) help error: %v", args, err)
		}
		if !isHelp && err == nil {
			t.Errorf("Execute(%q) malformed command unexpectedly succeeded", args)
		}
	}
	if len(hookCalls) != 0 {
		t.Fatalf("hooks ran for help/invalid args before dispatch: %v", hookCalls)
	}
	var output bytes.Buffer
	if err := Execute(context.Background(), nil, strings.NewReader(""), &output, io.Discard, hooks); err != nil {
		t.Fatal(err)
	}
	if hookCalls["foreground"] != 1 {
		t.Fatalf("legacy no-args path did not dispatch foreground hook: %v", hookCalls)
	}
}

func TestLegacyServerFlagsReachInjectedHooksAndRemainIsolatedBetweenCalls(t *testing.T) {
	for _, tc := range []struct {
		name, hook string
		args       []string
	}{
		{"root legacy flags", "foreground", []string{"-runtime", "container", "-addr", "127.0.0.1:18101", "-data-dir", "/tmp/state-a", "-log-file", "/tmp/a.log", "-legacy-db", "/tmp/a.db"}},
		{"daemon flags", "start", []string{"start", "-runtime", "docker", "-addr", "127.0.0.1:18102", "-data-dir", "/tmp/state-b", "-log-file", "/tmp/b.log", "-legacy-db", "/tmp/b.db"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called string
			var seen []string
			hooks := ServerHooks{
				Foreground: func(args []string, _ io.Writer) error {
					called, seen = "foreground", append([]string(nil), args...)
					return nil
				},
				Start: func(args []string, _ io.Writer) error {
					called, seen = "start", append([]string(nil), args...)
					return nil
				},
			}
			var out bytes.Buffer
			if err := Execute(context.Background(), tc.args, strings.NewReader(""), &out, io.Discard, hooks); err != nil {
				t.Fatalf("Execute(%q): %v", tc.args, err)
			}
			if called != tc.hook {
				t.Fatalf("called hook=%q, want %q", called, tc.hook)
			}
			joined := strings.Join(seen, " ")
			for _, value := range []string{"--addr", "--runtime", "--data-dir", "--log-file", "--legacy-db"} {
				if !strings.Contains(joined, value) {
					t.Errorf("legacy option %s omitted from hook args: %q", value, seen)
				}
			}
		})
	}
}

func TestCobraBooleanValuesAndDetachShorthandChooseExpectedExecutionMode(t *testing.T) {
	t.Run("quiet false retains normal create response", func(t *testing.T) {
		server, addr := cliFixture(t, serveCLIResponse)
		defer server.Close()
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"create", "node:22", "--quiet=false", "--addr", addr}, strings.NewReader(""), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "id:") || strings.TrimSpace(out.String()) == "sbx-123" {
			t.Fatalf("--quiet=false altered human create output: %q", out.String())
		}
	})
	t.Run("detach false observes streams", func(t *testing.T) {
		var mu sync.Mutex
		var requests []string
		server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, r.Method+" "+r.URL.RequestURI())
			mu.Unlock()
			serveCLIResponse(w, r)
		})
		defer server.Close()
		if err := Run(context.Background(), []string{"exec", "demo", "--addr", addr, "--detach=false", "--", "node", "--help"}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		joined := strings.Join(requests, "\n")
		mu.Unlock()
		if !strings.Contains(joined, "wait=true") || !strings.Contains(joined, "stream=true") || strings.Contains(joined, "/kill") {
			t.Fatalf("--detach=false did not remain a foreground observer: %s", joined)
		}
	})
	t.Run("detach shorthand avoids observation", func(t *testing.T) {
		var mu sync.Mutex
		var requests []string
		server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, r.Method+" "+r.URL.RequestURI())
			mu.Unlock()
			serveCLIResponse(w, r)
		})
		defer server.Close()
		if err := Run(context.Background(), []string{"exec", "demo", "-d", "--addr", addr, "--", "node", "--help"}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(requests) != 2 || requests[1] != "POST /v1/sandboxes/sbx-123/cmd" {
			t.Fatalf("-d unexpectedly observed a detached command: %v", requests)
		}
	})
}

func TestFlagsAndLocalParsersEnforceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		bad   bool
	}{
		{"", 0, false}, {"0s", 0, false}, {"15m", 900, false}, {"1.5s", 0, true}, {"-1s", 0, true}, {"1ns", 0, true}, {"999999999999999999999h", 0, true},
	} {
		got, err := durationSeconds(tc.value, false)
		if (err != nil) != tc.bad || err == nil && got != tc.want {
			t.Errorf("durationSeconds(%q) = %d, %v", tc.value, got, err)
		}
	}
	for _, value := range []string{"512MiB", "1GiB", "1000MiB", "0B"} {
		if _, err := memoryMiB(value); err != nil {
			t.Errorf("memoryMiB(%q): %v", value, err)
		}
	}
	for _, value := range []string{"512MB", "8193MiB", "1", "-1GiB", "1.2MiB"} {
		if _, err := memoryMiB(value); err == nil {
			t.Errorf("memoryMiB(%q) accepted invalid limit", value)
		}
	}
	for _, value := range []string{"HUP", "SIGINT", "term", "64"} {
		if _, err := signalNumber(value); err != nil {
			t.Errorf("signalNumber(%q): %v", value, err)
		}
	}
	for _, value := range []string{"0", "65", "unknown"} {
		if _, err := signalNumber(value); err == nil {
			t.Errorf("signalNumber(%q) accepted invalid signal", value)
		}
	}
	if parsed, err := url.ParseRequestURI("/v1/files?path=%2Ftmp%2Fx"); err != nil || parsed.Query().Get("path") != "/tmp/x" {
		t.Fatalf("query encoding fixture invalid: %v %v", parsed, err)
	}
}

func TestExecuteUsesFreshCobraTreesAndSeparatesWritersAcrossCalls(t *testing.T) {
	var callsMu sync.Mutex
	firstCalls, secondCalls := 0, 0
	firstServer, firstAddr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		firstCalls++
		callsMu.Unlock()
		if r.URL.Path != "/v1/sandboxes" {
			t.Errorf("first tree unexpected request %s", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxes":[{"id":"first-id","name":"first"}]}`)
	})
	defer firstServer.Close()
	secondServer, secondAddr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		secondCalls++
		callsMu.Unlock()
		if r.URL.Path != "/v1/sandboxes" {
			t.Errorf("second tree unexpected request %s", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxes":[{"id":"second-id","name":"second"}]}`)
	})
	defer secondServer.Close()
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	var canceledOut, canceledErr bytes.Buffer
	if err := Execute(canceledCtx, []string{"ls", "--json", "--addr", firstAddr}, strings.NewReader("canceled input"), &canceledOut, &canceledErr, ServerHooks{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Execute error=%v; want context cancellation", err)
	}
	callsMu.Lock()
	first := firstCalls
	callsMu.Unlock()
	if first != 0 {
		t.Fatalf("canceled invocation reached API %d times", first)
	}
	var firstOut, firstErr, secondOut, secondErr bytes.Buffer
	if err := Execute(context.Background(), []string{"ls", "--json", "--addr", firstAddr}, strings.NewReader("first input"), &firstOut, &firstErr, ServerHooks{}); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if err := Execute(context.Background(), []string{"ls", "--json", "--addr", secondAddr}, strings.NewReader("second input"), &secondOut, &secondErr, ServerHooks{}); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if !strings.Contains(firstOut.String(), "first-id") || strings.Contains(firstOut.String(), "second-id") {
		t.Fatalf("first invocation output leaked or used stale endpoint: %q", firstOut.String())
	}
	if !strings.Contains(secondOut.String(), "second-id") || strings.Contains(secondOut.String(), "first-id") {
		t.Fatalf("second invocation output leaked or used stale endpoint: %q", secondOut.String())
	}
	if firstErr.Len() != 0 || secondErr.Len() != 0 {
		t.Fatalf("diagnostics crossed invocations: first=%q second=%q", firstErr.String(), secondErr.String())
	}
	callsMu.Lock()
	first, second := firstCalls, secondCalls
	callsMu.Unlock()
	if first != 1 || second != 1 {
		t.Fatalf("fresh invocations used stale clients/trees: first requests=%d second requests=%d", first, second)
	}
}

func TestInjectedServerHooksSkipHelpAndMalformedCommands(t *testing.T) {
	calls := map[string]int{}
	var foregroundArgs []string
	hooks := ServerHooks{
		Foreground: func(args []string, out io.Writer) error {
			calls["foreground"]++
			foregroundArgs = append([]string(nil), args...)
			_, err := io.WriteString(out, "foreground-hook\n")
			return err
		},
		Start: func(args []string, _ io.Writer) error {
			calls["start"]++
			return nil
		},
		Stop: func(args []string, _ io.Writer) error {
			calls["stop"]++
			return nil
		},
	}
	for _, args := range [][]string{{"--help"}, {"start", "--help"}, {"start", "extra"}, {"start", "--unknown-option"}, {"stop", "extra"}} {
		var out, stderr bytes.Buffer
		err := Execute(context.Background(), args, strings.NewReader(""), &out, &stderr, hooks)
		if args[len(args)-1] == "--help" {
			if err != nil {
				t.Errorf("help invocation %q: %v", args, err)
			}
		} else if err == nil {
			t.Errorf("malformed invocation %q unexpectedly succeeded", args)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("server hooks ran for help/invalid commands: %v", calls)
	}
	var rootOut bytes.Buffer
	if err := Execute(context.Background(), nil, strings.NewReader(""), &rootOut, io.Discard, hooks); err != nil {
		t.Fatal(err)
	}
	if calls["foreground"] != 1 || !strings.Contains(rootOut.String(), "foreground-hook") || len(foregroundArgs) == 0 {
		t.Fatalf("legacy no-args foreground hook calls=%v args=%q output=%q", calls, foregroundArgs, rootOut.String())
	}
}

func TestLegacyServerFlagsReachForegroundAndDaemonHooksWithoutStartingAnything(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantHook string
	}{
		{"foreground legacy single-dash flags", []string{"-runtime", "container", "-addr", "127.0.0.1:18091", "-data-dir", "/tmp/opensbx-state", "-log-file", "/tmp/opensbx.log", "-legacy-db", "/tmp/legacy.db"}, "foreground"},
		{"daemon options after command", []string{"start", "-runtime", "docker", "-addr", "127.0.0.1:18092", "-data-dir", "/tmp/opensbx-start", "-log-file", "/tmp/start.log", "-legacy-db", "/tmp/start-legacy.db"}, "start"},
		{"global address before daemon command", []string{"--addr", "127.0.0.1:18093", "stop", "--data-dir", "/tmp/opensbx-stop"}, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := ""
			var actual []string
			hooks := ServerHooks{
				Foreground: func(args []string, _ io.Writer) error {
					called, actual = "foreground", append([]string(nil), args...)
					return nil
				},
				Start: func(args []string, _ io.Writer) error {
					called, actual = "start", append([]string(nil), args...)
					return nil
				},
				Stop: func(args []string, _ io.Writer) error {
					called, actual = "stop", append([]string(nil), args...)
					return nil
				},
			}
			var out, stderr bytes.Buffer
			if err := Execute(context.Background(), tc.args, strings.NewReader(""), &out, &stderr, hooks); err != nil {
				t.Fatalf("Execute(%q): %v; stderr=%q", tc.args, err, stderr.String())
			}
			if called != tc.wantHook {
				t.Fatalf("hook = %q, want %q; args=%q", called, tc.wantHook, actual)
			}
			joined := strings.Join(actual, " ")
			for _, expected := range []string{"--addr", "127.0.0.1:"} {
				if !strings.Contains(joined, expected) {
					t.Errorf("hook arguments omitted %q: %q", expected, actual)
				}
			}
			if tc.wantHook != "stop" || strings.Contains(strings.Join(tc.args, " "), "-legacy-db") {
				for _, expected := range []string{"--runtime", "--data-dir", "--log-file", "--legacy-db"} {
					if !strings.Contains(joined, expected) {
						t.Errorf("hook arguments omitted legacy option %q: %q", expected, actual)
					}
				}
			}
		})
	}
}

func TestPflagBooleanFalseAndShorthandKeepOperationModeExplicit(t *testing.T) {
	t.Run("quiet false retains normal create output", func(t *testing.T) {
		server, addr := cliFixture(t, serveCLIResponse)
		defer server.Close()
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"create", "node:22", "--quiet=false", "--addr", addr}, strings.NewReader(""), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "id:") || strings.TrimSpace(out.String()) == "sbx-123" {
			t.Fatalf("--quiet=false changed human create rendering: %q", out.String())
		}
	})
	t.Run("detach false observes command instead of detaching", func(t *testing.T) {
		var mu sync.Mutex
		var requests []string
		server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, r.Method+" "+r.URL.RequestURI())
			mu.Unlock()
			serveCLIResponse(w, r)
		})
		defer server.Close()
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"exec", "demo", "--addr", addr, "--detach=false", "--", "node", "--help"}, strings.NewReader(""), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := append([]string(nil), requests...)
		mu.Unlock()
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, "POST /v1/sandboxes/sbx-123/cmd\n") || !strings.Contains(joined, "wait=true") || !strings.Contains(joined, "stream=true") {
			t.Fatalf("--detach=false did not perform foreground observation: %v", got)
		}
		if strings.Contains(joined, "/kill") {
			t.Fatalf("boolean false unexpectedly triggered a kill: %v", got)
		}
	})
	t.Run("detach shorthand returns without observation", func(t *testing.T) {
		var mu sync.Mutex
		var requests []string
		server, addr := cliFixture(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, r.Method+" "+r.URL.RequestURI())
			mu.Unlock()
			serveCLIResponse(w, r)
		})
		defer server.Close()
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"exec", "demo", "-d", "--addr", addr, "--", "node", "--help"}, strings.NewReader(""), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := append([]string(nil), requests...)
		mu.Unlock()
		if len(got) != 2 || got[1] != "POST /v1/sandboxes/sbx-123/cmd" {
			t.Fatalf("-d shorthand should resolve and create once without wait/log requests: %v", got)
		}
	})
}
