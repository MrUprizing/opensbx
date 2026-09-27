package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"opensbx/internal/database"
)

// Server is a reverse proxy that routes HTTP requests based on subdomain.
type Server struct {
	repo        *database.Repository
	cache       *routeCache
	resolveLive func(context.Context, string) (string, error)
}

// SetResolver is configured before serving. Live resolution prevents stale
// published ports from routing traffic to a different process after sandbox stop.
func (s *Server) SetResolver(resolve func(context.Context, string) (string, error)) {
	s.resolveLive = resolve
}

// New creates a proxy Server.
func New(repo *database.Repository) *Server {
	return &Server{
		repo:  repo,
		cache: newRouteCache(30 * time.Second),
	}
}

// Handler returns the http.Handler for the proxy server.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleRequest)
}

// InvalidateCache removes a sandbox entry from the route cache.
func (s *Server) InvalidateCache(name string) {
	s.cache.Invalidate(name)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	name := s.extractSubdomain(r.Host)
	if name == "" {
		http.Error(w, "no subdomain in request", http.StatusBadGateway)
		return
	}

	var target *url.URL
	var err error
	if s.resolveLive != nil {
		var port string
		port, err = s.resolveLive(r.Context(), name)
		if err == nil {
			port, err = checkedHostPort(port)
		}
		if err == nil {
			target = &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port)}
		}
	} else {
		target, err = s.resolve(name)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %q: %v", name, err), http.StatusBadGateway)
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = r.Host
		},
		FlushInterval: -1, // stream immediately (SSE, WebSocket, HMR)
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy error for %s: %v", name, err)
			http.Error(w, "sandbox unavailable", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

// extractSubdomain extracts the sandbox name from the Host header.
// "mi-app.localhost:3000" with baseDomain "localhost" → "mi-app"
func (s *Server) extractSubdomain(host string) string {
	h, _, err := canonicalAuthority(host)
	if err != nil {
		return ""
	}
	if !sandboxHost.MatchString(h) {
		return ""
	}
	return strings.TrimSuffix(h, ".localhost")
}
