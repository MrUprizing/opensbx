package proxy

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"opensbx/internal/database"
)

// Server is a reverse proxy that routes HTTP requests based on subdomain.
type Server struct {
	baseDomain string
	repo       *database.Repository
	cache      *routeCache
}

// New creates a proxy Server.
func New(baseDomain string, repo *database.Repository) *Server {
	return &Server{
		baseDomain: baseDomain,
		repo:       repo,
		cache:      newRouteCache(30 * time.Second),
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

	target, err := s.resolve(name)
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
	// Strip port if present.
	h := host
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		h = h[:idx]
	}

	suffix := "." + s.baseDomain
	if !strings.HasSuffix(h, suffix) {
		return ""
	}

	sub := strings.TrimSuffix(h, suffix)
	if sub == "" || strings.Contains(sub, ".") {
		return "" // no nested subdomains
	}

	return sub
}
