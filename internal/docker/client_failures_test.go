package docker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
)

func TestLifecycleReportsPostMutationInspectFailure(t *testing.T) {
	for _, operation := range []string{"create", "start", "restart"} {
		t.Run(operation, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			sequence := []int{http.StatusInternalServerError}
			if operation == "start" {
				sequence = []int{0, http.StatusInternalServerError}
				fixture.running = false
			}
			fixture.responses = map[string][]int{"GET /containers/container-1/json": sequence}
			var err error
			switch operation {
			case "create":
				_, err = dc.Create(ctx, runtimeio.CreateSandboxRequest{Image: "alpine"})
			case "start":
				seedDockerLifecycleOwner(t, dc, "container-1")
				_, err = dc.Start(ctx, "container-1")
			case "restart":
				seedDockerLifecycleOwner(t, dc, "container-1")
				_, err = dc.Restart(ctx, "container-1")
			}
			require.ErrorContains(t, err, "fixture error", "a failed post-operation inspect must not be reported as success")
			fixture.mu.Lock()
			requests := append([]string(nil), fixture.requests...)
			fixture.mu.Unlock()
			var expected []string
			switch operation {
			case "create":
				expected = []string{"GET /images/alpine/json", "POST /containers/create", "POST /containers/container-1/start", "GET /containers/container-1/json", "DELETE /containers/container-1"}
			case "start":
				expected = []string{"GET /containers/container-1/json", "POST /containers/container-1/start", "GET /containers/container-1/json"}
			case "restart":
				expected = []string{"POST /containers/container-1/restart", "GET /containers/container-1/json"}
			}
			assert.Equal(t, expected, requests, "failure must occur after the mutation, not during preflight")
		})
	}
}

func TestGetNetworkMapsDaemonErrorsForPersistedSandbox(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			require.NoError(t, dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo", Port: "3000/tcp"}))
			fixture.fail["GET /containers/container-1/json"] = status
			network, err := dc.GetNetwork(context.Background(), "container-1")
			require.Error(t, err)
			assert.Empty(t, network.PortsMap)
			if status == http.StatusNotFound {
				assert.ErrorIs(t, err, ErrNotFound)
			} else {
				assert.ErrorContains(t, err, "fixture error")
				assert.NotErrorIs(t, err, ErrNotFound)
			}
		})
	}
}

func TestRemovePreservesRecordsOnDaemonFailureAndRetriesCleanup(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	require.NoError(t, dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}))
	require.NoError(t, dc.repo.SaveCommand(database.Command{ID: "cmd-1", SandboxID: "container-1"}))
	fixture.fail["DELETE /containers/container-1"] = http.StatusInternalServerError
	require.ErrorContains(t, dc.Remove(ctx, "container-1"), "fixture error")
	sandbox, err := dc.repo.FindByID("container-1")
	require.NoError(t, err)
	require.NotNil(t, sandbox, "failed deletion must leave the sandbox discoverable for retry")
	commands, err := dc.ListCommands(ctx, "container-1")
	require.NoError(t, err)
	require.Len(t, commands, 1)
	assert.Equal(t, "cmd-1", commands[0].ID)
	fixture.mu.Lock()
	fixture.fail["DELETE /containers/container-1"] = http.StatusNotFound
	fixture.mu.Unlock()
	require.NoError(t, dc.Remove(ctx, "container-1"))
	sandbox, err = dc.repo.FindByID("container-1")
	require.NoError(t, err)
	assert.Nil(t, sandbox)
	commands, err = dc.ListCommands(ctx, "container-1")
	require.NoError(t, err)
	assert.Empty(t, commands)
}

func TestFileOperationsSurfaceExecTransportFailures(t *testing.T) {
	for _, operation := range []string{"read", "write", "delete", "list"} {
		for _, stage := range []string{"create", "attach", "inspect", "malformed stream"} {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				dc, fixture := newDockerFixture(t)
				switch stage {
				case "create":
					fixture.fail["POST /containers/container-1/exec"] = http.StatusInternalServerError
				case "attach":
					fixture.fail["POST /exec/exec-1/start"] = http.StatusInternalServerError
				case "inspect":
					fixture.fail["GET /exec/exec-1/json"] = http.StatusInternalServerError
				case "malformed stream":
					// Docker multiplexing permits stdout/stderr stream IDs, not 9.
					fixture.attachBody = string([]byte{9, 0, 0, 0, 0, 0, 0, 0})
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				var err error
				var output string
				switch operation {
				case "read":
					output, err = dc.ReadFile(ctx, "container-1", "/work/file")
				case "write":
					err = dc.WriteFile(ctx, "container-1", "/work/file", "content")
				case "delete":
					err = dc.DeleteFile(ctx, "container-1", "/work/file")
				case "list":
					output, err = dc.ListDir(ctx, "container-1", "/work")
				}
				require.Error(t, err)
				assert.Empty(t, output, "failed file reads must not return partial output as successful content")
				switch stage {
				case "attach":
					assert.ErrorContains(t, err, "received 500")
				case "malformed stream":
					assert.ErrorContains(t, err, "unrecognized stream: 9")
				default:
					assert.ErrorContains(t, err, "fixture error")
				}
				fixture.mu.Lock()
				requests := append([]string(nil), fixture.requests...)
				fixture.mu.Unlock()
				expected := []string{"POST /containers/container-1/exec"}
				if stage != "create" {
					expected = append(expected, "POST /exec/exec-1/start")
				}
				if stage == "inspect" {
					expected = append(expected, "GET /exec/exec-1/json")
				}
				assert.Equal(t, expected, requests, "exec must stop at the failing stage")
			})
		}
	}
}

func TestFileOperationsRejectNonzeroGuestExit(t *testing.T) {
	for _, operation := range []string{"read", "write", "delete", "list"} {
		t.Run(operation, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			if operation == "write" {
				fixture.directExecExitCodes = []int{0, 23}
			} else {
				fixture.directExecExitCode = 23
			}
			fixture.directExecStdout = []byte("partial output")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			var output string
			var err error
			switch operation {
			case "read":
				output, err = dc.ReadFile(ctx, "container-1", "/work/file")
			case "write":
				err = dc.WriteFile(ctx, "container-1", "/work/file", "content")
			case "delete":
				err = dc.DeleteFile(ctx, "container-1", "/work/file")
			case "list":
				output, err = dc.ListDir(ctx, "container-1", "/work")
			}
			require.ErrorContains(t, err, "23", "a nonzero guest exit must not be treated as a successful file operation")
			assert.Empty(t, output, "failed file reads must not return partial output as successful content")
		})
	}
}

func TestWriteFileStopsWhenGuestDirectoryCreationFails(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	fixture.directExecExitCode = 23
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := dc.WriteFile(ctx, "container-1", "/work/file", "content that must not be sent")
	require.ErrorContains(t, err, "23")
	fixture.mu.Lock()
	created := append([]dockerExecCreate(nil), fixture.execCreates...)
	stdin := append([]byte(nil), fixture.stdin...)
	fixture.mu.Unlock()
	require.Len(t, created, 1, "a failed mkdir must prevent the subsequent file-writing exec")
	assert.Empty(t, stdin, "file contents must not be sent after directory creation failed")
}

func TestFileOperationErrorEscapesAndBoundsGuestDiagnostics(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	fixture.directExecExitCode = 19
	fixture.directExecStdout = []byte("private file content")
	fixture.directExecStderr = []byte("\x1b[31m\r\n" + strings.Repeat("d", 300))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := dc.ReadFile(ctx, "container-1", "/work/file")
	require.ErrorContains(t, err, "19")
	assert.NotContains(t, err.Error(), "private file content", "read output must not leak into an error diagnostic")
	assert.NotContains(t, err.Error(), "\x1b", "raw terminal escape bytes must not reach the returned error")
	assert.Contains(t, err.Error(), `\x1b`, "the escape byte should be visible as quoted text")
	assert.Contains(t, err.Error(), "...", "long guest diagnostics must be visibly truncated")
	assert.Less(t, len(err.Error()), 1024, "returned guest diagnostics must remain bounded after escaping")
}

func TestWriteFileKeepsGuestPathOutOfShellSource(t *testing.T) {
	for _, path := range []string{
		"/work/with spaces/file.txt",
		`/work/it's-a-file.txt`,
		"-leading-dash;$(touch /tmp/not-created)",
	} {
		t.Run(path, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := dc.WriteFile(ctx, "container-1", path, "safe content"); err != nil {
				t.Fatalf("WriteFile() error: %v", err)
			}

			fixture.mu.Lock()
			commands := append([]dockerExecCreate(nil), fixture.execCreates...)
			fixture.mu.Unlock()
			if len(commands) != 2 {
				t.Fatalf("WriteFile() created %d exec commands, want mkdir and write", len(commands))
			}
			for _, execution := range commands {
				foundAsArgument := false
				for _, arg := range execution.Cmd {
					if arg == path {
						foundAsArgument = true
					}
				}
				if len(execution.Cmd) > 2 && strings.Contains(execution.Cmd[2], path) {
					t.Errorf("guest path %q was interpolated into shell source %q", path, execution.Cmd[2])
				}
				if !foundAsArgument {
					t.Errorf("guest path %q was not passed as a literal exec argument: %#v", path, execution.Cmd)
				}
			}
		})
	}
}

func TestFileOperationsProtectOptionLikeGuestPaths(t *testing.T) {
	for _, tc := range []struct {
		operation string
		path      string
	}{
		{operation: "read", path: "-n"},
		{operation: "delete", path: "--no-preserve-root"},
		{operation: "list", path: "--help"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			switch tc.operation {
			case "read":
				_, _ = dc.ReadFile(ctx, "container-1", tc.path)
			case "delete":
				_ = dc.DeleteFile(ctx, "container-1", tc.path)
			case "list":
				_, _ = dc.ListDir(ctx, "container-1", tc.path)
			}

			fixture.mu.Lock()
			commands := append([]dockerExecCreate(nil), fixture.execCreates...)
			fixture.mu.Unlock()
			require.Len(t, commands, 1)
			cmd := commands[0].Cmd
			pathIndex := -1
			for i, arg := range cmd {
				if arg == tc.path {
					pathIndex = i
					break
				}
			}
			require.NotEqual(t, -1, pathIndex, "the guest path must remain a literal exec argument")
			safeOperand := pathIndex > 0 && cmd[pathIndex-1] == "--"
			if len(cmd) > 2 && strings.Contains(cmd[2], `case "$1"`) {
				safeOperand = true // The fixed script must make relative paths non-option operands.
			}
			assert.True(t, safeOperand, "option-like guest paths must be protected from option parsing: %#v", cmd)
		})
	}
}

func TestExecWithStdinBoundsCapturedOutput(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			largeOutput := bytes.Repeat([]byte("x"), (4<<20)+1)
			if stream == "stdout" {
				fixture.directExecStdout = largeOutput
			} else {
				fixture.directExecStderr = largeOutput
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			result, err := dc.execWithStdin(ctx, "container-1", []string{"cat"}, nil)
			require.Error(t, err, "synchronous exec output above the capture limit must fail")
			assert.LessOrEqual(t, len(result.stdout), 4<<20, "captured stdout must remain bounded")
			assert.LessOrEqual(t, len(result.stderr), 4<<20, "captured stderr must remain bounded")
		})
	}
}

func TestExecWithStdinReturnsWhenCallerCancelsBlockedAttach(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	fixture.directExecBlockEntered = make(chan struct{})
	fixture.directExecBlockRelease = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type execOutcome struct {
		err error
	}
	outcome := make(chan execOutcome, 1)
	go func() {
		_, err := dc.execWithStdin(ctx, "container-1", []string{"cat"}, nil)
		outcome <- execOutcome{err: err}
	}()
	select {
	case <-fixture.directExecBlockEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Docker fixture did not block the attached stream")
	}
	started := time.Now()
	cancel()
	select {
	case result := <-outcome:
		require.ErrorIs(t, result.err, context.Canceled)
		assert.Less(t, time.Since(started), time.Second, "a canceled hijacked Docker stream must be closed promptly")
	case <-time.After(time.Second):
		t.Fatal("execWithStdin did not return after its context was canceled")
	}
}

type failingInput struct{ err error }

func (r failingInput) Read([]byte) (int, error) { return 0, r.err }

func TestExecWithStdinPreservesInputReadFailure(t *testing.T) {
	dc, _ := newDockerFixture(t)
	inputErr := errors.New("input stream interrupted")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := dc.execWithStdin(ctx, "container-1", []string{"cat"}, failingInput{err: inputErr})
	require.ErrorIs(t, err, inputErr)
}
