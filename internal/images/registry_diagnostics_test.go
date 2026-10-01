package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	registrytransport "github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

const publicRegistryError = "registry request failed or was rejected by local URL policy"

// registryDiagnostic is the minimal internal, sanitized diagnostic contract.
// Implementations must not unwrap an upstream error or change Error().
type registryDiagnostic interface {
	RegistryDiagnostic() (category string, stage string, status int)
}

func TestPullRegistryStatusDiagnosticsPreserveSanitizedPublicError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		category string
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, category: "upstream_status"},
		{name: "temporarily unavailable", status: http.StatusServiceUnavailable, category: "upstream_status"},
		{name: "authentication rejected", status: http.StatusUnauthorized, category: "authentication"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
			const signedURL = "https://private.invalid/blob?X-Amz-Credential=synthetic-secret&X-Amz-Signature=synthetic-signature"
			var requestedPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestedPath = r.URL.Path
				w.Header().Set("X-Upstream-Debug", "synthetic-upstream-detail")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, "upstream failed for %s bearer synthetic-token", signedURL)
			}))
			t.Cleanup(server.Close)

			store, err := Open(t.TempDir(), WithTransport(server.Client().Transport))
			if err != nil {
				t.Fatal(err)
			}
			ref := strings.TrimPrefix(server.URL, "http://") + "/team/private:latest"
			err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
			if err == nil {
				t.Fatal("Pull() unexpectedly succeeded against failing registry fixture")
			}
			stage := expectedRegistryStage(requestedPath)
			if stage == "unknown" {
				t.Fatalf("status response came from unclassified registry endpoint %q", requestedPath)
			}
			assertRegistryDiagnostic(t, err, tc.category, stage, tc.status)
		})
	}
}

func TestRegistryTransportDiagnosticsClassifyTypedFailuresWithoutUnwrapping(t *testing.T) {
	dnsErr := &net.DNSError{
		Err:        "synthetic DNS detail for signed.invalid?token=synthetic-secret",
		Name:       "signed.invalid",
		IsNotFound: true,
	}
	timeoutErr := timeoutTransportError{"synthetic timeout detail for signed.invalid?token=synthetic-secret"}
	policy := &registryPolicy{
		primary: "registry.example",
		base:    http.DefaultTransport,
	}
	request, err := http.NewRequest(http.MethodGet, "https://signed.invalid/blob?token=synthetic-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, policyErr := policy.RoundTrip(request)
	if policyErr == nil {
		t.Fatal("registry policy unexpectedly accepted an untrusted external host")
	}
	for _, tc := range []struct {
		name     string
		err      error
		category string
		stage    string
	}{
		{name: "DNS failure", err: dnsErr, category: "dns", stage: "transport"},
		{name: "timeout", err: timeoutErr, category: "timeout", stage: "transport"},
		{name: "local URL policy rejection", err: policyErr, category: "policy", stage: "transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diagnostic := safeRegistryError(tc.err)
			assertRegistryDiagnostic(t, diagnostic, tc.category, tc.stage, 0)
			if errors.Is(diagnostic, tc.err) {
				t.Errorf("sanitized diagnostic unwraps or preserves raw failure %T", tc.err)
			}
		})
	}
}

func TestPullResponseBodyDiagnosticsUseTransportFailureSemantics(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	const rawFailure = "synthetic response body failure https://signed.invalid/blob?token=synthetic-secret"
	transport := responseBodyFailureTransport{err: errors.New(rawFailure)}
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Pull(context.Background(), "registry.example/team/private:latest", v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("Pull() unexpectedly succeeded when registry response body failed")
	}
	assertRegistryDiagnostic(t, err, "read", "manifest", http.StatusOK)
	if strings.Contains(err.Error(), rawFailure) || strings.Contains(err.Error(), "synthetic-secret") || errors.Is(err, transport.err) {
		t.Fatalf("response-body diagnostic leaked or unwrapped its raw error: %v", err)
	}
}

func TestInsecureRegistryPingPrefersHTTPResponseBodyFailureOverHTTPSProbe(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	transport := &insecurePingFailureTransport{}
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("Pull() unexpectedly succeeded after both registry ping probes failed")
	}
	if got := transport.httpsRequests.Load(); got != 1 {
		t.Errorf("HTTPS probe count=%d, want 1", got)
	}
	if got := transport.httpRequests.Load(); got != 1 {
		t.Errorf("HTTP fallback probe count=%d, want 1", got)
	}
	assertRegistryDiagnostic(t, err, "read", "registry-ping", http.StatusServiceUnavailable)
}

func TestPullOpaqueSameRegistryManifestRedirectIsNotClassifiedAsAuth(t *testing.T) {
	var opaqueRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/team/app/manifests/latest":
			http.Redirect(w, r, "/v2/team/app/objects/opaque-item?signature=synthetic-secret", http.StatusTemporaryRedirect)
		case "/v2/team/app/objects/opaque-item":
			opaqueRequests.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, "opaque response failure")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	store, err := Open(t.TempDir(), WithTransport(server.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("Pull() unexpectedly succeeded after the redirected manifest failed")
	}
	if got := opaqueRequests.Load(); got != 1 {
		t.Fatalf("opaque same-registry redirect requests=%d, want 1", got)
	}
	assertRegistryDiagnostic(t, err, "upstream_status", "unknown", http.StatusServiceUnavailable)
}

func TestPullActualBearerRealmFailureRetainsAuthStageForOpaqueTokenPath(t *testing.T) {
	var exchangeRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service=%q,scope=%q`, server.URL+"/credentials/exchange", "fixture", "repository:team/app:pull"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/credentials/exchange" {
			exchangeRequests.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, "credential service unavailable")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	store, err := Open(t.TempDir(), WithTransport(server.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("Pull() unexpectedly succeeded after the challenged credential exchange failed")
	}
	if got := exchangeRequests.Load(); got == 0 {
		t.Fatal("registry client did not call the challenge's nonstandard credential endpoint")
	}
	assertRegistryDiagnostic(t, err, "upstream_status", "auth", http.StatusServiceUnavailable)
}

func TestPullDiagnosticStageUsesFinalRegistryEndpointWithReservedRepoNames(t *testing.T) {
	for _, tc := range []struct {
		name       string
		repository string
		failedPath string
		stage      string
	}{
		{name: "blob endpoint after manifests repository component", repository: "team/manifests/app", failedPath: "/blobs/", stage: "blob"},
		{name: "manifest endpoint after blobs repository component", repository: "team/blobs/app", failedPath: "/manifests/latest", stage: "manifest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registryServer := httptest.NewServer(registry.New())
			t.Cleanup(registryServer.Close)
			base := registryServer.Client().Transport
			ref := strings.TrimPrefix(registryServer.URL, "http://") + "/" + tc.repository + ":latest"
			parsed, err := name.ParseReference(ref, name.Insecure)
			if err != nil {
				t.Fatal(err)
			}
			image := diagnosticTestImage(t)
			if err := remote.Write(parsed, image, remote.WithTransport(base)); err != nil {
				t.Fatalf("seed OCI image in hermetic registry: %v", err)
			}

			transport := failingRegistryPathTransport{base: base, repository: tc.repository, failedPath: tc.failedPath}
			store, err := Open(t.TempDir(), WithTransport(&transport))
			if err != nil {
				t.Fatal(err)
			}
			err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
			if err == nil {
				t.Fatal("Pull() unexpectedly succeeded despite the selected endpoint's 503 response")
			}
			if got := transport.failures.Load(); got != 1 {
				t.Fatalf("injected terminal endpoint failures=%d, want 1", got)
			}
			assertRegistryDiagnostic(t, err, "upstream_status", tc.stage, http.StatusServiceUnavailable)
		})
	}
}

func TestRegistryDiagnosticClassifiesTerminalAuthBlobAndCDNResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string
		status   int
		category string
		stage    string
	}{
		{name: "terminal token service auth", url: "https://auth.docker.io/token", status: http.StatusUnauthorized, category: "authentication", stage: "auth"},
		{name: "terminal registry blob", url: "https://registry-1.docker.io/v2/library/node/blobs/sha256:deadbeef", status: http.StatusNotFound, category: "upstream_status", stage: "blob"},
		{name: "terminal Docker CDN blob", url: "https://production.cloudflare.docker.com/signed/blob?token=synthetic-secret", status: http.StatusServiceUnavailable, category: "upstream_status", stage: "blob"},
		{name: "terminal GHCR CDN blob", url: "https://pkg-containers.githubusercontent.com/ghcr/signed?token=synthetic-secret", status: http.StatusBadGateway, category: "upstream_status", stage: "blob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			upstream := &registrytransport.Error{StatusCode: tc.status, Request: request}
			diagnostic := safeRegistryError(upstream)
			assertRegistryDiagnostic(t, diagnostic, tc.category, tc.stage, tc.status)
			if strings.Contains(diagnostic.Error(), "synthetic-secret") || errors.Is(diagnostic, upstream) {
				t.Fatalf("terminal registry diagnostic leaked or unwrapped upstream error: %v", diagnostic)
			}
		})
	}
}

func TestPullEmitsOneRedactedDiagnosticLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, "https://private.invalid/blob?X-Amz-Credential=synthetic-secret bearer synthetic-token")
	}))
	t.Cleanup(server.Close)
	store, err := Open(t.TempDir(), WithTransport(server.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}

	logs := captureRegistryPullLogs(t, func() {
		if err := store.Pull(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/team/private:latest", v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
			t.Fatal("Pull() unexpectedly succeeded against unavailable registry")
		}
	})
	if count := strings.Count(logs, "registry_pull_failed"); count != 1 {
		t.Fatalf("registry diagnostic log count=%d, want exactly one; logs=%q", count, logs)
	}
	for _, field := range []string{"category=upstream_status", "stage=registry-ping", "status=503"} {
		if !strings.Contains(logs, field) {
			t.Errorf("registry log %q does not include %q", logs, field)
		}
	}
	for _, secret := range []string{"private.invalid", "X-Amz-Credential", "synthetic-secret", "synthetic-token", "team/private"} {
		if strings.Contains(logs, secret) {
			t.Errorf("registry log leaked %q: %q", secret, logs)
		}
	}
}

func TestPullCancellationLogHasNoHiddenCauseOrHTTPStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("synthetic hidden cause https://private.invalid/?token=synthetic-secret: %w", context.Canceled)
	})
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}

	var pullErr error
	logs := captureRegistryPullLogs(t, func() {
		pullErr = store.Pull(context.Background(), "registry.example/team/canceled:latest", v1.Platform{OS: "linux", Architecture: "amd64"})
	})
	if !errors.Is(pullErr, context.Canceled) || pullErr.Error() != context.Canceled.Error() {
		t.Fatalf("Pull cancellation=%v; want context.Canceled without hidden cause", pullErr)
	}
	if count := strings.Count(logs, "registry_pull_failed"); count != 1 {
		t.Fatalf("canceled pull log count=%d, want exactly one; logs=%q", count, logs)
	}
	for _, field := range []string{"category=unknown", "stage=unknown", "status=0"} {
		if !strings.Contains(logs, field) {
			t.Errorf("cancellation log %q does not include %q", logs, field)
		}
	}
	if strings.Contains(logs, "private.invalid") || strings.Contains(logs, "synthetic-secret") {
		t.Fatalf("cancellation log leaked hidden cause: %q", logs)
	}
}

func captureRegistryPullLogs(t *testing.T, action func()) string {
	t.Helper()
	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	action()
	return logs.String()
}

func TestRegistryDiagnosticsDoNotInferCategoriesFromArbitraryErrorText(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{name: "policy-like wording", message: "registry URL policy rejected a host; token=synthetic-secret"},
		{name: "DNS-like wording", message: "DNS lookup failed for signed.invalid?token=synthetic-secret"},
		{name: "HTTP-status-like wording", message: "upstream HTTP 503 at https://signed.invalid/blob?token=synthetic-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := errors.New(tc.message)
			diagnostic := safeRegistryError(err)
			assertRegistryDiagnostic(t, diagnostic, "unknown", "unknown", 0)
			if errors.Is(diagnostic, err) {
				t.Fatal("sanitized diagnostic unwraps arbitrary upstream error")
			}
		})
	}
}

func TestRegistryDiagnosticPreservesContextCancellationSemantics(t *testing.T) {
	if err := safeRegistryError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled registry request error=%v", err)
	}
	if err := safeRegistryError(context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out registry request error=%v", err)
	}
}

func assertRegistryDiagnostic(t *testing.T, err error, wantCategory, wantStage string, wantStatus int) {
	t.Helper()
	if got := err.Error(); got != publicRegistryError {
		t.Errorf("public error=%q; want unchanged sanitized message %q", got, publicRegistryError)
	}
	var diagnostic registryDiagnostic
	if !errors.As(err, &diagnostic) {
		t.Errorf("error %T does not expose the internal RegistryDiagnostic() interface", err)
		return
	}
	category, stage, status := diagnostic.RegistryDiagnostic()
	if category != wantCategory || stage != wantStage || status != wantStatus {
		t.Errorf("RegistryDiagnostic()=(%q, %q, %d), want (%q, %q, %d)", category, stage, status, wantCategory, wantStage, wantStatus)
	}
}

func expectedRegistryStage(path string) string {
	switch {
	case path == "/v2/":
		return "registry-ping"
	case strings.Contains(path, "/manifests/"):
		return "manifest"
	case strings.Contains(path, "/blobs/"):
		return "blob"
	default:
		return "unknown"
	}
}

type responseBodyFailureTransport struct{ err error }

func (r responseBodyFailureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/v2/" {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/vnd.oci.image.manifest.v1+json"}},
		Body:       failingReadCloser(r),
		Request:    request,
	}, nil
}

type insecurePingFailureTransport struct {
	httpsRequests atomic.Int32
	httpRequests  atomic.Int32
}

func (r *insecurePingFailureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch request.URL.Scheme {
	case "https":
		r.httpsRequests.Add(1)
		return nil, errors.New("synthetic HTTPS probe connection failure")
	case "http":
		r.httpRequests.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       failingReadCloser{err: errors.New("synthetic HTTP probe body failure")},
			Request:    request,
		}, nil
	default:
		return nil, fmt.Errorf("unexpected registry probe scheme %q", request.URL.Scheme)
	}
}

type failingRegistryPathTransport struct {
	base       http.RoundTripper
	repository string
	failedPath string
	failures   atomic.Int32
}

func (r *failingRegistryPathTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	prefix := "/v2/" + r.repository
	if strings.HasPrefix(request.URL.Path, prefix) && strings.Contains(request.URL.Path, r.failedPath) {
		r.failures.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("synthetic endpoint unavailable")),
			Request:    request,
		}, nil
	}
	return r.base.RoundTrip(request)
}

func diagnosticTestImage(t *testing.T) v1.Image {
	t.Helper()
	image, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", "amd64"
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

type failingReadCloser struct{ err error }

func (r failingReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (failingReadCloser) Close() error               { return nil }

type timeoutTransportError struct{ message string }

func (e timeoutTransportError) Error() string   { return e.message }
func (e timeoutTransportError) Timeout() bool   { return true }
func (e timeoutTransportError) Temporary() bool { return true }

var _ net.Error = timeoutTransportError{}
