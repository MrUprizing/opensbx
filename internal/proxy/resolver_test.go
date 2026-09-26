package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"opensbx/internal/database"
)

func TestProxyDatabaseFailureReturnsBadGatewayWithoutCaching(t *testing.T) {
	db := database.New(":memory:")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	s := New("localhost", database.NewRepository(db))
	require.NoError(t, sqlDB.Close())
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	_, cached := s.cache.get("demo")
	assert.False(t, cached, "failed lookups must not produce a cached route")
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
	s := New("localhost", repo)
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
