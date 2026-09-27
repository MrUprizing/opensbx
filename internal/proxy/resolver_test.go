package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"opensbx/internal/database"
)

func TestProxyDatabaseFailureReturnsBadGatewayWithoutCaching(t *testing.T) {
	db := database.New(":memory:")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	s := New(database.NewRepository(db))
	require.NoError(t, sqlDB.Close())
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	_, cached := s.cache.get("demo")
	assert.False(t, cached, "failed lookups must not produce a cached route")
}

func TestLiveResolverBypassesStalePortCacheAndStopsRoutingWhenOwnershipEnds(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("first-owner")) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("reused-port-owner")) }))
	defer second.Close()
	firstPort := strings.TrimPrefix(first.URL, "http://127.0.0.1:")
	secondPort := strings.TrimPrefix(second.URL, "http://127.0.0.1:")
	var selected atomic.Value
	selected.Store(firstPort)
	var stopped atomic.Bool
	server := New(nil)
	server.SetResolver(func(_ context.Context, name string) (string, error) {
		if name != "demo" {
			return "", errors.New("not owned")
		}
		if stopped.Load() {
			return "", errors.New("sandbox stopped")
		}
		return selected.Load().(string), nil
	})
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "http://demo.localhost:8080/a/stream", nil)
		server.Handler().ServeHTTP(w, r)
		return w
	}
	if got := request(); got.Code != http.StatusOK || got.Body.String() != "first-owner" {
		t.Fatalf("initial live route status=%d body=%q", got.Code, got.Body.String())
	}
	selected.Store(secondPort)
	if got := request(); got.Code != http.StatusOK || got.Body.String() != "reused-port-owner" {
		t.Fatalf("updated live route used stale published port: status=%d body=%q", got.Code, got.Body.String())
	}
	stopped.Store(true)
	if got := request(); got.Code != http.StatusBadGateway {
		t.Fatalf("stopped sandbox routed to reused port: status=%d body=%q", got.Code, got.Body.String())
	}
}

func TestProxyInvalidPortMappingRecoversAfterRepair(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("recovered backend"))
	}))
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	db := database.New(":memory:")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	require.NoError(t, repo.Save(database.Sandbox{
		ID: "sb-1", Name: "demo", Port: "3000/tcp", Ports: database.JSONMap{"4000/tcp": target.Port()},
	}))
	s := New(repo)
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil))
		return w
	}
	assert.Equal(t, http.StatusBadGateway, request().Code)
	_, cached := s.cache.get("demo")
	assert.False(t, cached)
	require.NoError(t, repo.UpdatePorts("sb-1", database.JSONMap{"3000/tcp": target.Port()}))
	w := request()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "recovered backend", w.Body.String(), "repaired routing must work without manual cache invalidation")
}
