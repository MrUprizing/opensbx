package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"opensbx/internal/database"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractSubdomain_Localhost(t *testing.T) {
	s := &Server{baseDomain: "localhost"}

	tests := []struct {
		host string
		want string
	}{
		{"mi-app.localhost:3000", "mi-app"},
		{"mi-app.localhost", "mi-app"},
		{"localhost:3000", ""},
		{"localhost", ""},
		{"nested.sub.localhost", ""},
		{"mi-app.other.com", ""},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := s.extractSubdomain(tt.host)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractSubdomain_Production(t *testing.T) {
	s := &Server{baseDomain: "sandbox.example.com"}

	tests := []struct {
		host string
		want string
	}{
		{"mi-app.sandbox.example.com", "mi-app"},
		{"mi-app.sandbox.example.com:80", "mi-app"},
		{"sandbox.example.com", ""},
		{"mi-app.localhost", ""},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := s.extractSubdomain(tt.host)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveHostPort(t *testing.T) {
	// port set and present in port map
	sb := &database.Sandbox{
		Port:  "3000/tcp",
		Ports: database.JSONMap{"3000/tcp": "32768"},
	}
	hp, err := resolveHostPort(sb)
	require.NoError(t, err)
	assert.Equal(t, "32768", hp)

	// port set but not in port map → error
	sb2 := &database.Sandbox{
		Port:  "9999/tcp",
		Ports: database.JSONMap{"3000/tcp": "32768"},
	}
	_, err = resolveHostPort(sb2)
	assert.Error(t, err)

	// no port configured, single port in map → auto-resolve
	sb3 := &database.Sandbox{
		Ports: database.JSONMap{"3000/tcp": "32768"},
	}
	hp3, err := resolveHostPort(sb3)
	require.NoError(t, err)
	assert.Equal(t, "32768", hp3)

	// no port configured, multiple ports → error
	sb4 := &database.Sandbox{
		Ports: database.JSONMap{"80/tcp": "32000", "443/tcp": "32001"},
	}
	_, err = resolveHostPort(sb4)
	assert.Error(t, err)

	// no port configured, no ports at all → error
	sb5 := &database.Sandbox{
		Ports: database.JSONMap{},
	}
	_, err = resolveHostPort(sb5)
	assert.Error(t, err)
}

func TestRouteCache(t *testing.T) {
	c := newRouteCache(100 * time.Millisecond)

	target, _ := url.Parse("http://127.0.0.1:32768")
	c.set("mi-app", target)

	// Hit
	got, ok := c.get("mi-app")
	assert.True(t, ok)
	assert.Equal(t, target, got)

	// Miss
	_, ok = c.get("other")
	assert.False(t, ok)

	// Invalidate
	c.set("mi-app", target)
	c.Invalidate("mi-app")
	_, ok = c.get("mi-app")
	assert.False(t, ok)

	// Expire
	c.set("mi-app", target)
	time.Sleep(150 * time.Millisecond)
	_, ok = c.get("mi-app")
	assert.False(t, ok)
}

func TestProxy_NoSubdomain(t *testing.T) {
	s := New("localhost", nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "localhost:3000"

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestProxy_SandboxNotFound(t *testing.T) {
	db := database.New(":memory:")
	repo := database.NewRepository(db)

	s := New("localhost", repo)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "unknown.localhost:3000"

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestProxy_EndToEnd(t *testing.T) {
	// Start a backend server simulating a sandbox container.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from sandbox"))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	port := u.Port()

	// Set up in-memory DB with a sandbox entry.
	db := database.New(":memory:")
	repo := database.NewRepository(db)
	repo.Save(database.Sandbox{
		ID:    "test123",
		Name:  "mi-app",
		Image: "node:22",
		Ports: database.JSONMap{"3000/tcp": port},
		Port:  "3000/tcp",
	})

	// Create proxy server.
	s := New("localhost", repo)
	proxySrv := httptest.NewServer(s.Handler())
	defer proxySrv.Close()

	// Make request with subdomain Host header.
	req, _ := http.NewRequest("GET", proxySrv.URL+"/", nil)
	req.Host = "mi-app.localhost:3000"

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "hello from sandbox", string(body))
}

func TestProxy_CacheInvalidation(t *testing.T) {
	// First backend
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend-1"))
	}))
	defer backend1.Close()
	u1, _ := url.Parse(backend1.URL)

	// Second backend (simulating port change after restart)
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend-2"))
	}))
	defer backend2.Close()
	u2, _ := url.Parse(backend2.URL)

	db := database.New(":memory:")
	repo := database.NewRepository(db)
	repo.Save(database.Sandbox{
		ID:    "test123",
		Name:  "mi-app",
		Image: "node:22",
		Ports: database.JSONMap{"3000/tcp": u1.Port()},
		Port:  "3000/tcp",
	})

	s := New("localhost", repo)
	proxySrv := httptest.NewServer(s.Handler())
	defer proxySrv.Close()

	doReq := func() string {
		req, _ := http.NewRequest("GET", proxySrv.URL+"/", nil)
		req.Host = "mi-app.localhost:3000"
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	// First request → backend-1, gets cached
	assert.Equal(t, "backend-1", doReq())

	// Simulate restart: update port in DB
	repo.UpdatePorts("test123", database.JSONMap{"3000/tcp": u2.Port()})

	// Still cached → backend-1
	assert.Equal(t, "backend-1", doReq())

	// Invalidate cache
	s.InvalidateCache("mi-app")

	// Now resolves to backend-2
	assert.Equal(t, "backend-2", doReq())
}

func TestProxy_UpgradeHeaderForwarding(t *testing.T) {
	// Verify the proxy forwards Upgrade/Connection headers to the backend.
	// A real WebSocket handshake requires a full WS server; here we just verify
	// the proxy correctly relays the upgrade-related headers.
	var receivedUpgrade, receivedConnection string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUpgrade = r.Header.Get("Upgrade")
		receivedConnection = r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upgrade headers received"))
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	db := database.New(":memory:")
	repo := database.NewRepository(db)
	repo.Save(database.Sandbox{
		ID:    "ws-test",
		Name:  "ws-app",
		Image: "node:22",
		Ports: database.JSONMap{"3000/tcp": u.Port()},
		Port:  "3000/tcp",
	})

	s := New("localhost", repo)
	proxySrv := httptest.NewServer(s.Handler())
	defer proxySrv.Close()

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/_next/webpack-hmr", proxySrv.URL), nil)
	req.Host = "ws-app.localhost:3000"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "websocket", receivedUpgrade)
	assert.Contains(t, receivedConnection, "Upgrade")
}
