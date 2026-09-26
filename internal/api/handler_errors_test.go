package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"opensbx/internal/api"
	"opensbx/internal/docker"
	"opensbx/models"
)

func TestFileHandlersPropagateBackendErrors(t *testing.T) {
	for _, operation := range []struct {
		name, method, path string
		body               any
	}{
		{"read", http.MethodGet, "/files?path=/work/file", nil},
		{"write", http.MethodPut, "/files?path=/work/file", map[string]string{"content": "new content"}},
		{"delete", http.MethodDelete, "/files?path=/work/file", nil},
		{"list", http.MethodGet, "/files/list?path=/work/file", nil},
	} {
		for _, failure := range []struct {
			name, code, message string
			err                 error
			status              int
		}{
			{"daemon", "INTERNAL_ERROR", "daemon unavailable", errors.New("daemon unavailable"), http.StatusInternalServerError},
			{"missing sandbox", "NOT_FOUND", "sandbox not found", fmt.Errorf("lookup: %w", docker.ErrNotFound), http.StatusNotFound},
			{"deadline", "TIMEOUT", "operation timed out", fmt.Errorf("execution: %w", context.DeadlineExceeded), http.StatusRequestTimeout},
		} {
			t.Run(operation.name+"/"+failure.name, func(t *testing.T) {
				calls := 0
				check := func(id, path string) error {
					calls++
					assert.Equal(t, "sandbox-7", id)
					assert.Equal(t, "/work/file", path)
					return failure.err
				}
				d := &stub{}
				switch operation.name {
				case "read":
					d.readFile = func(id, path string) (string, error) { return "", check(id, path) }
				case "write":
					d.writeFile = func(id, path, content string) error {
						assert.Equal(t, "new content", content)
						return check(id, path)
					}
				case "delete":
					d.deleteFile = check
				case "list":
					d.listDir = func(id, path string) (string, error) { return "", check(id, path) }
				}
				w := do(newRouter(d), operation.method, "/v1/sandboxes/sandbox-7"+operation.path, operation.body)
				assert.Equal(t, 1, calls)
				assert.Equal(t, failure.status, w.Code)
				assert.JSONEq(t, fmt.Sprintf(`{"code":%q,"message":%q}`, failure.code, failure.message), w.Body.String())
			})
		}
	}
}

func TestWriteFileRejectsMalformedBodyBeforeCallingBackend(t *testing.T) {
	called := false
	r := newRouter(&stub{writeFile: func(string, string, string) error { called = true; return nil }})
	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/sb/files?path=/work/file", strings.NewReader(`{"content":`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.False(t, called)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var response api.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "BAD_REQUEST", response.Code)
	assert.NotEmpty(t, response.Message)
}

func TestNetworkHandlerPropagatesDaemonFailure(t *testing.T) {
	w := do(newRouter(&stub{getNetwork: func(id string) (models.SandboxNetwork, error) {
		assert.Equal(t, "sandbox-7", id)
		return models.SandboxNetwork{}, errors.New("inspect failed")
	}}), http.MethodGet, "/v1/sandboxes/sandbox-7/network", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"code":"INTERNAL_ERROR","message":"inspect failed"}`, w.Body.String())
}

func TestLogStreamReportsOpenFailureAsJSON(t *testing.T) {
	w := do(newRouter(&stub{streamCommandLogs: func(sandboxID, commandID string) (io.ReadCloser, io.ReadCloser, error) {
		assert.Equal(t, "sandbox-7", sandboxID)
		assert.Equal(t, "command-9", commandID)
		return nil, nil, errors.New("attach failed")
	}}), http.MethodGet, "/v1/sandboxes/sandbox-7/cmd/command-9/logs?stream=true", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.JSONEq(t, `{"code":"INTERNAL_ERROR","message":"attach failed"}`, w.Body.String())
}

// Override only the context-sensitive operation; the general stub intentionally
// omits contexts so most handler tests can focus on request argument mapping.
type cancelableWaitStub struct {
	*stub
	wait func(context.Context, string, string) (models.CommandDetail, error)
}

func (s *cancelableWaitStub) WaitCommand(ctx context.Context, sandboxID, commandID string) (models.CommandDetail, error) {
	return s.wait(ctx, sandboxID, commandID)
}

func TestWaitStreamCancellationStopsWaitingWithoutEmittingCompletion(t *testing.T) {
	waiting := make(chan struct{})
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancelableWaitStub{
		stub: &stub{getCommand: func(sandboxID, commandID string) (models.CommandDetail, error) {
			return models.CommandDetail{ID: commandID, SandboxID: sandboxID, Name: "sleep", StartedAt: 10}, nil
		}},
		wait: func(ctx context.Context, _, _ string) (models.CommandDetail, error) {
			close(waiting)
			<-ctx.Done()
			return models.CommandDetail{}, ctx.Err()
		},
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb/cmd/cmd-1?wait=true", nil).WithContext(ctx)
	r := newRouter(d)
	go func() { defer close(finished); r.ServeHTTP(w, req) }()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach WaitCommand")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not terminate after cancellation")
	}
	assert.Contains(t, w.Header().Get("Content-Type"), "application/x-ndjson")
	decoder := json.NewDecoder(w.Body)
	var initial models.CommandResponse
	require.NoError(t, decoder.Decode(&initial))
	assert.Equal(t, "cmd-1", initial.Command.ID)
	assert.Equal(t, "sb", initial.Command.SandboxID)
	assert.Nil(t, initial.Command.ExitCode)
	assert.Nil(t, initial.Command.FinishedAt)
	var extra models.CommandResponse
	assert.ErrorIs(t, decoder.Decode(&extra), io.EOF, "cancellation must not fabricate a completed command")
}
