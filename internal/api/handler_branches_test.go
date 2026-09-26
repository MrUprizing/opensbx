package api_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"opensbx/internal/api"
	"opensbx/models"
)

func TestListAndDeleteHandlersCoverEmptyAndFailureResponses(t *testing.T) {
	t.Run("empty sandbox list", func(t *testing.T) {
		r := newRouter(&stub{list: func() ([]models.SandboxSummary, error) { return nil, nil }})
		w := do(r, http.MethodGet, "/v1/sandboxes", nil)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "no sandboxes found")
	})
	t.Run("remove failure", func(t *testing.T) {
		r := newRouter(&stub{remove: func(string) error { return errors.New("remove failed") }})
		w := do(r, http.MethodDelete, "/v1/sandboxes/sb", nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
	t.Run("list failure", func(t *testing.T) {
		r := newRouter(&stub{list: func() ([]models.SandboxSummary, error) { return nil, errors.New("list failed") }})
		w := do(r, http.MethodGet, "/v1/sandboxes", nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
	t.Run("empty image list", func(t *testing.T) {
		r := newRouter(&stub{listImages: func() ([]models.ImageSummary, error) { return nil, nil }})
		w := do(r, http.MethodGet, "/v1/images", nil)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "no images found")
	})
	t.Run("non-empty image list", func(t *testing.T) {
		r := newRouter(&stub{listImages: func() ([]models.ImageSummary, error) { return []models.ImageSummary{{ID: "sha256:1"}}, nil }})
		w := do(r, http.MethodGet, "/v1/images", nil)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "sha256:1")
	})
	t.Run("image list failure", func(t *testing.T) {
		r := newRouter(&stub{listImages: func() ([]models.ImageSummary, error) { return nil, errors.New("daemon failed") }})
		w := do(r, http.MethodGet, "/v1/images", nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

func TestWaitCommandStreamsInitialAndFinalStatus(t *testing.T) {
	r := newRouter(&stub{
		getCommand: func(sandboxID, cmdID string) (models.CommandDetail, error) {
			return models.CommandDetail{ID: cmdID, SandboxID: sandboxID, Name: "job", StartedAt: 10}, nil
		},
		waitCommand: func(sandboxID, cmdID string) (models.CommandDetail, error) {
			exitCode := 0
			finishedAt := int64(20)
			return models.CommandDetail{ID: cmdID, SandboxID: sandboxID, Name: "job", ExitCode: &exitCode, StartedAt: 10, FinishedAt: &finishedAt}, nil
		},
	})
	w := do(r, http.MethodGet, "/v1/sandboxes/sb/cmd/cmd-1?wait=true", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/x-ndjson")
	assert.Equal(t, 2, strings.Count(w.Body.String(), "\n"))
	assert.Contains(t, w.Body.String(), `"exit_code":0`)
}

func TestMCPHandlerCanBeConstructedWithoutDockerIO(t *testing.T) {
	h := api.NewMCPHandler(&stub{}, "localhost", ":3000", false)
	if h == nil {
		t.Fatal("NewMCPHandler() returned nil")
	}
}
