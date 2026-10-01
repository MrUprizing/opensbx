package images

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func policyResponse(r *http.Request, headers http.Header) *http.Response {
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: headers, Body: io.NopCloser(strings.NewReader("challenge")), Request: r}
}

func TestRegistryPolicyAllowsOnlyExplicitOriginAndStripsCredentialsFromStorageCDNs(t *testing.T) {
	baseCalls := 0
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		baseCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})
	policy := &registryPolicy{primary: "registry.example.test", insecure: true, base: base}
	for _, raw := range []string{"https://registry.example.test/v2/", "http://registry.example.test/v2/"} {
		req, _ := http.NewRequest(http.MethodGet, raw, nil)
		if _, err := policy.RoundTrip(req); err != nil {
			t.Errorf("allowed explicit registry %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://unrelated.example.test/v2/", "https://user:secret@registry.example.test/v2/", "https://registry.example.test/v2/#fragment"} {
		req, _ := http.NewRequest(http.MethodGet, raw, nil)
		if _, err := policy.RoundTrip(req); err == nil {
			t.Errorf("untrusted registry URL %q accepted", raw)
		}
	}
	cdnPolicy := &registryPolicy{primary: "registry-1.docker.io", hub: true, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("registry credentials forwarded to CDN: %v", r.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("blob")), Request: r}, nil
	})}
	cdnReq, _ := http.NewRequest(http.MethodGet, "https://production.cloudflare.docker.com/blob", nil)
	cdnReq.Header.Set("Authorization", "Bearer synthetic-only")
	cdnReq.Header.Set("Cookie", "test=secret")
	cdnReq.Header.Set("Proxy-Authorization", "Basic synthetic")
	if _, err := cdnPolicy.RoundTrip(cdnReq); err != nil {
		t.Fatal(err)
	}
	post, _ := http.NewRequest(http.MethodPost, "https://production.cloudflare.docker.com/blob", nil)
	if _, err := cdnPolicy.RoundTrip(post); err == nil {
		t.Fatal("non-read CDN request accepted")
	}
	if baseCalls != 2 {
		t.Fatalf("policy forwarded %d requests to base transport, want only explicit registry requests", baseCalls)
	}
}

func TestDockerHubCloudFrontRedirectAllowsOnlySafeReadsAndStripsSensitiveHeaders(t *testing.T) {
	var originRequests, cdnRequests int
	policy := &registryPolicy{primary: "registry-1.docker.io", hub: true, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "registry-1.docker.io":
			originRequests++
			header := make(http.Header)
			header.Set("Location", "https://production.cloudfront.docker.com/blob")
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		case "production.cloudfront.docker.com":
			cdnRequests++
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
				t.Errorf("credentials reached approved Docker Hub CDN: %v", r.Header)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("blob")), Request: r}, nil
		default:
			t.Errorf("base transport received unexpected authority %q", r.URL.Host)
			return nil, errors.New("unexpected test authority")
		}
	})}
	client := &http.Client{Transport: policy}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request, err := http.NewRequest(method, "https://registry-1.docker.io/v2/library/node/blobs/sha256:test", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer synthetic-only")
		request.Header.Set("Cookie", "session=synthetic")
		request.Header.Set("Proxy-Authorization", "Basic synthetic")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("follow Docker Hub blob redirect for %s: %v", method, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("redirected %s response status=%d want 200", method, response.StatusCode)
		}
	}
	if originRequests != 2 || cdnRequests != 2 {
		t.Fatalf("redirect requests origin=%d CDN=%d want 2 each", originRequests, cdnRequests)
	}
}

func TestDockerHubCloudFrontPolicyRejectsNearbyAuthoritiesMethodsAndBearerRealms(t *testing.T) {
	baseCalls := 0
	policy := &registryPolicy{primary: "registry-1.docker.io", hub: true, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		baseCalls++
		if r.URL.Hostname() == "registry-1.docker.io" {
			header := make(http.Header)
			header.Set("WWW-Authenticate", `Bearer realm="https://production.cloudfront.docker.com/token"`)
			return policyResponse(r, header), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	for _, raw := range []string{
		"https://evil-production.cloudfront.docker.com/blob",
		"https://production.cloudfront.docker.com.evil.test/blob",
		"https://nested.production.cloudfront.docker.com/blob",
		"https://production.cloudfront.docker.com:444/blob",
		"https://production.cloudflare.docker.com:444/blob",
	} {
		request, _ := http.NewRequest(http.MethodGet, raw, nil)
		if _, err := policy.RoundTrip(request); err == nil {
			t.Errorf("unapproved nearby CDN authority accepted: %s", raw)
		}
	}
	post, _ := http.NewRequest(http.MethodPost, "https://production.cloudfront.docker.com/blob", nil)
	if _, err := policy.RoundTrip(post); err == nil {
		t.Fatal("POST to approved CloudFront hostname was accepted")
	}
	origin, _ := http.NewRequest(http.MethodGet, "https://registry-1.docker.io/v2/library/node/manifests/latest", nil)
	if _, err := policy.RoundTrip(origin); err == nil || !strings.Contains(err.Error(), "untrusted bearer realm") {
		t.Fatalf("Docker Hub bearer realm pointing to a storage CDN error=%v", err)
	}
	if baseCalls != 1 {
		t.Fatalf("rejected authorities/methods reached base transport %d times; only the origin challenge should", baseCalls)
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fd00::1"} {
		ip := net.ParseIP(raw)
		if ip == nil || publicRegistryIP(ip) {
			t.Errorf("private/reserved registry redirect address %q was considered public", raw)
		}
	}
}

func TestRegistryPolicyRejectsUntrustedBearerRealmsBeforeFollowingThem(t *testing.T) {
	closed := false
	policy := &registryPolicy{primary: "registry.example.test", base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Add("WWW-Authenticate", `Bearer realm="https://127.0.0.1:8080/token"`)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: h, Body: closeTrackingBody{Reader: strings.NewReader("challenge"), closed: &closed}, Request: r}, nil
	})}
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/team/app/manifests/latest", nil)
	if _, err := policy.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "untrusted bearer realm") {
		t.Fatalf("untrusted realm error=%v", err)
	}
	if !closed {
		t.Fatal("rejected bearer challenge body was not closed")
	}
	policy.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Add("WWW-Authenticate", `Bearer realm="https://registry.example.test/token"`)
		return policyResponse(r, h), nil
	})
	if _, err := policy.RoundTrip(req); err != nil {
		t.Fatalf("same-registry bearer realm rejected: %v", err)
	}
	policy.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Add("WWW-Authenticate", `Basic realm="https://attacker.example/token"`)
		return policyResponse(r, h), nil
	})
	if _, err := policy.RoundTrip(req); err != nil {
		t.Fatalf("non-bearer challenge should not redirect transport: %v", err)
	}
}

type closeTrackingBody struct {
	io.Reader
	closed *bool
}

func (b closeTrackingBody) Close() error { *b.closed = true; return nil }

func TestRegistryPolicyRejectsPrivateAddressesAndCanonicalizesAuthorities(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.4", "169.254.1.2", "100.64.0.1", "198.18.0.1", "224.0.0.1", "::1", "fd00::1", "fe80::1"} {
		ip := net.ParseIP(raw)
		if ip == nil || publicRegistryIP(ip) {
			t.Errorf("private/reserved address %q considered public", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		ip := net.ParseIP(raw)
		if ip == nil || !publicRegistryIP(ip) {
			t.Errorf("public address %q considered nonpublic", raw)
		}
	}
	for _, tc := range []struct{ raw, want string }{{"https://REGISTRY.example:443/v2", "registry.example"}, {"http://registry.example:80/v2", "registry.example"}, {"https://registry.example:5000/v2", "registry.example:5000"}, {"https://[2001:db8::1]:443/v2", "[2001:db8::1]"}} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := registryAuthority(u); got != tc.want {
			t.Errorf("registryAuthority(%q)=%q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestRegistryPolicyPropagatesTransportErrorsAndRejectsMalformedBearerRealms(t *testing.T) {
	baseErr := errors.New("synthetic low-level connection failure")
	policy := &registryPolicy{primary: "registry.example.test", base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, baseErr
	})}
	request, err := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.RoundTrip(request); !errors.Is(err, baseErr) {
		t.Fatalf("base transport error=%v, want original transport failure", err)
	}
	for _, realm := range []string{
		"%invalid",
		"https://user:secret@registry.example.test/token",
		"https://registry.example.test/token#fragment",
		"http://registry.example.test/token",
	} {
		t.Run(realm, func(t *testing.T) {
			closed := false
			policy.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: h, Body: closeTrackingBody{Reader: strings.NewReader("challenge"), closed: &closed}, Request: r}, nil
			})
			if _, err := policy.RoundTrip(request); err == nil || !strings.Contains(err.Error(), "untrusted bearer realm") {
				t.Fatalf("malformed bearer realm %q error=%v", realm, err)
			}
			if !closed {
				t.Fatalf("rejected bearer challenge %q body was not closed", realm)
			}
		})
	}
}

func TestRegistryOptionsPinCredentialsAndTransportToExplicitRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", home)
	store, err := Open(t.TempDir(), WithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("not invoked in option construction test")
	})))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference("ghcr.io/team/app:tag")
	if err != nil {
		t.Fatal(err)
	}
	options, closeTransport, err := store.registryOptionsFor(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	if len(options) != 4 {
		t.Fatalf("policy options=%d want context/auth/transport/retry policy", len(options))
	}
	ref, err = name.ParseReference("registry-1.docker.io/library/node:latest")
	if err != nil {
		t.Fatal(err)
	}
	if _, closeTransport, err = store.registryOptionsFor(context.Background(), ref); err != nil {
		t.Fatalf("explicit Docker Hub options: %v", err)
	} else {
		closeTransport()
	}
}

func TestRegistryOptionsCreateIndependentDNSPinnedTransportsForPublicRegistries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", home)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"ghcr.io/team/app:latest", "registry-1.docker.io/library/node:latest"} {
		ref, err := name.ParseReference(raw)
		if err != nil {
			t.Fatal(err)
		}
		options, closeTransport, err := store.registryOptionsFor(context.Background(), ref)
		if err != nil {
			t.Fatalf("registry policy options for %s: %v", raw, err)
		}
		if len(options) != 4 {
			t.Errorf("%s policy options=%d want scoped context/auth/transport/retry", raw, len(options))
		}
		closeTransport()
	}
}

type failingRegistryReader struct{}

func (failingRegistryReader) Read([]byte) (int, error) {
	return 0, errors.New("sensitive registry path /private/credential")
}

func TestSafeRegistryReaderRedactsTransportErrorsAndPreservesEOF(t *testing.T) {
	if _, err := (safeRegistryReader{Reader: failingRegistryReader{}}).Read(make([]byte, 8)); err == nil || strings.Contains(err.Error(), "/private/credential") || err.Error() != "registry request failed or was rejected by local URL policy" {
		t.Fatalf("unsafe registry read error=%v", err)
	}
	n, err := (safeRegistryReader{Reader: strings.NewReader("")}).Read(make([]byte, 8))
	if n != 0 || err != io.EOF {
		t.Fatalf("registry EOF converted unexpectedly: n=%d err=%v", n, err)
	}
}
