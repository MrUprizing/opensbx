package docker

import (
	"context"
	"errors"
	"net/http"
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
				_, err = dc.Start(ctx, "container-1")
			case "restart":
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
