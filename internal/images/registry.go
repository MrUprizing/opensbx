package images

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Option configures an internal seam without permitting callers to disable the
// registry policy. Injected transports are still wrapped by the same policy.
type Option func(*Store)

func WithTransport(rt http.RoundTripper) Option { return func(s *Store) { s.registryTransport = rt } }

type registryPolicy struct {
	primary  string
	hub      bool
	insecure bool
	base     http.RoundTripper
}

func (p *registryPolicy) registry(host string) bool {
	return host == p.primary || (p.hub && (host == "registry-1.docker.io" || host == "index.docker.io"))
}
func (p *registryPolicy) token(host string) bool {
	return p.registry(host) || (p.hub && host == "auth.docker.io")
}
func (p *registryPolicy) cdn(host string) bool {
	return p.hub && (host == "production.cloudflare.docker.com" || strings.HasSuffix(host, ".r2.cloudflarestorage.com")) || p.primary == "ghcr.io" && (host == "pkg-containers.githubusercontent.com" || strings.HasSuffix(host, ".pkg-containers.githubusercontent.com"))
}

var realmPattern = regexp.MustCompile(`(?i)\brealm\s*=\s*(?:"([^"]+)"|([^,\s]+))`)

func registryAuthority(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
func publicRegistryIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 || v4[0] >= 224 || v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 || v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return false
		}
	}
	return true
}

func (p *registryPolicy) RoundTrip(r *http.Request) (*http.Response, error) {
	host := registryAuthority(r.URL)
	if r.URL.User != nil || r.URL.Fragment != "" || r.URL.Scheme != "https" && !(p.insecure && p.registry(host) && r.URL.Scheme == "http") {
		return nil, errors.New("registry URL policy rejected scheme or authority")
	}
	if !p.token(host) && !p.cdn(host) {
		return nil, errors.New("registry URL policy rejected external host")
	}
	if !p.token(host) {
		if r.Method != "GET" && r.Method != "HEAD" {
			return nil, errors.New("registry URL policy rejected external method")
		}
		r = r.Clone(r.Context())
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		r.Header.Del("Proxy-Authorization")
	}
	resp, err := p.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	for _, challenge := range resp.Header.Values("WWW-Authenticate") {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(challenge)), "bearer ") {
			continue
		}
		m := realmPattern.FindStringSubmatch(challenge)
		valid := false
		if len(m) == 3 {
			raw := m[1]
			if raw == "" {
				raw = m[2]
			}
			u, e := url.Parse(raw)
			valid = e == nil && u.User == nil && u.Fragment == "" && p.token(registryAuthority(u)) && (u.Scheme == "https" || p.insecure && u.Scheme == "http" && p.registry(registryAuthority(u)))
		}
		if !valid {
			resp.Body.Close()
			return nil, errors.New("registry URL policy rejected untrusted bearer realm")
		}
	}
	return resp, nil
}

func (s *Store) registryOptionsFor(ctx context.Context, r name.Reference) ([]remote.Option, func(), error) {
	primary := registryAuthority(&url.URL{Scheme: r.Context().Registry.Scheme(), Host: r.Context().RegistryStr()})
	p := &registryPolicy{primary: primary, hub: primary == "index.docker.io" || primary == "registry-1.docker.io" || primary == "docker.io", insecure: r.Context().Registry.Scheme() == "http"}
	close := func() {}
	if s.registryTransport != nil {
		p.base = s.registryTransport
	} else {
		t := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 10, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second}
		t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			allowPrivate := p.registry(registryAuthority(&url.URL{Scheme: r.Context().Registry.Scheme(), Host: address}))
			if p.hub || p.primary == "ghcr.io" {
				allowPrivate = false
			}
			for _, ip := range ips {
				if !allowPrivate && !publicRegistryIP(ip.IP) {
					return nil, errors.New("registry URL policy rejected non-public address")
				}
			}
			// Pin the checked addresses, with bounded fallback instead of another
			// hostname lookup. Prefer IPv4 on dual-stack hosts without IPv6 routes.
			sort.SliceStable(ips, func(i, j int) bool { return ips[i].IP.To4() != nil && ips[j].IP.To4() == nil })
			var last error
			for _, ip := range ips {
				conn, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if e == nil {
					return conn, nil
				}
				last = e
			}
			if last == nil {
				last = errors.New("registry host has no addresses")
			}
			return nil, last
		}
		p.base = t
		close = t.CloseIdleConnections
	}
	// Resolve credentials once, for the explicitly requested repository only.
	auth, err := authn.DefaultKeychain.Resolve(r.Context())
	if err != nil {
		close()
		return nil, func() {}, errors.New("registry credential resolution failed")
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuth(auth), remote.WithTransport(p), remote.WithRetryPredicate(func(error) bool { return false })}, close, nil
}
