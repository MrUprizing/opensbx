package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var sandboxHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.localhost$`)

// canonicalAuthority normalizes DNS spelling only, never forwarded headers.
// Strip one root dot; a second dot remains invalid rather than being collapsed.
func canonicalAuthority(authority string) (string, string, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		if strings.ContainsAny(authority, ":[]") {
			return "", "", fmt.Errorf("malformed authority")
		}
		host, port = authority, "80"
	}
	if host == "" || strings.ContainsAny(host, "/@\\ \t\r\n%") {
		return "", "", fmt.Errorf("malformed host")
	}
	if strings.HasPrefix(authority, "[") && (net.ParseIP(host) == nil || !strings.Contains(host, ":")) {
		return "", "", fmt.Errorf("brackets require an IPv6 literal")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", "", fmt.Errorf("invalid port")
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("invalid port")
	}
	if net.ParseIP(host) == nil {
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		if host == "" || strings.Contains(host, "..") || strings.HasSuffix(host, ".") {
			return "", "", fmt.Errorf("malformed DNS host")
		}
	}
	return host, port, nil
}

// LocalHandler dispatches by the original authority, never by forwarded headers
// or URL path. A sandbox host can never fall through to management endpoints.
func LocalHandler(addr net.Addr, control, sandboxes http.Handler) http.Handler {
	listenHost, listenPort, _ := net.SplitHostPort(addr.String())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := canonicalAuthority(r.Host)
		if err != nil || port != listenPort {
			http.Error(w, "invalid local authority", http.StatusBadRequest)
			return
		}
		if host == "localhost" || host == "127.0.0.1" || (host == "::1" && strings.Contains(listenHost, ":")) {
			if !controlOriginAllowed(r, host, port) {
				http.Error(w, "control-plane origin forbidden", http.StatusForbidden)
				return
			}
			control.ServeHTTP(w, r)
			return
		}
		if sandboxHost.MatchString(host) {
			sandboxes.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unknown local host", http.StatusMisdirectedRequest)
	})
}

func controlOriginAllowed(r *http.Request, host, port string) bool {
	origins := r.Header.Values("Origin")
	if len(origins) > 0 {
		if len(origins) != 1 || origins[0] == "null" {
			return false
		}
		u, err := url.Parse(origins[0])
		if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		h, p, err := canonicalAuthority(u.Host)
		if err != nil || h != host || p != port {
			return false
		}
	}
	// Origin-less native clients are allowed. A browser's cross-site or
	// same-site (but not same-origin) request is never trusted as a native client.
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "none" || site == "same-origin"
}
