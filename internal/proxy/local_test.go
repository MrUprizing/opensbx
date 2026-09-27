package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestLocalHandlerDispatchesOriginalHostAndPreservesEveryPath(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "control:"+r.URL.Path) })
	sandbox := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "sandbox:"+r.URL.Path) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: LocalHandler(listener.Addr(), control, sandbox)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	endpoint := "http://" + listener.Addr().String()
	for _, tc := range []struct{ host, path, want string }{
		{net.JoinHostPort("localhost", strconv.Itoa(port)), "/v1/mcp", "control:/v1/mcp"},
		{net.JoinHostPort("LOCALHOST", strconv.Itoa(port)), "/v1/health", "control:/v1/health"},
		{net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "/swagger/index.html", "control:/swagger/index.html"},
		{net.JoinHostPort("demo.localhost", strconv.Itoa(port)), "/v1/mcp", "sandbox:/v1/mcp"},
		{net.JoinHostPort("demo.localhost.", strconv.Itoa(port)), "/v1/mcp", "sandbox:/v1/mcp"},
		{net.JoinHostPort("demo.localhost", strconv.Itoa(port)), "/assets/app.js", "sandbox:/assets/app.js"},
	} {
		req, _ := http.NewRequest(http.MethodGet, endpoint+tc.path, nil)
		req.Host = tc.host
		req.Header.Set("X-Forwarded-Host", "localhost:8123")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || string(body) != tc.want {
			t.Errorf("Host %q path %q => %d %q, want %q", tc.host, tc.path, response.StatusCode, body, tc.want)
		}
	}
}

func TestLocalHandlerRejectsUnknownMalformedAndWrongPortAuthorities(t *testing.T) {
	h := LocalHandler(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}, http.NotFoundHandler(), http.NotFoundHandler())
	for _, host := range []string{"evil.test:8080", "nested.demo.localhost:8080", "bad_.localhost:8080", "demo.localhost:8081", "demo.localhost:abc", "demo.localhost:8080@localhost", "[localhost]:8080", "demo.localhost:0"} {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/v1/sandboxes", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q status=%d body=%q", host, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

func TestLocalHandlerAllowsIPv6ControlOnlyWhenListenerIsIPv6(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		listener net.Addr
		host     string
		want     int
	}{
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 9090}, "[::1]:9090", http.StatusNoContent},
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9090}, "[::1]:9090", http.StatusMisdirectedRequest},
	} {
		h := LocalHandler(tc.listener, control, http.NotFoundHandler())
		r := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		r.Host = tc.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("listener %v Host %q status=%d want %d", tc.listener, tc.host, w.Code, tc.want)
		}
	}
}
