package images

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// registryFailure retains only controlled metadata, never the upstream cause.
// It deliberately has no Unwrap method.
type registryFailure struct {
	category string
	stage    string
	status   int
}

func (registryFailure) Error() string {
	return "registry request failed or was rejected by local URL policy"
}

func (e registryFailure) RegistryDiagnostic() (string, string, int) {
	return e.category, e.stage, e.status
}

type responseBackedRegistryFailure struct{ registryFailure }

// The SDK's parallel ping errors implement As but not Unwrap. A selective
// projection lets As search past a no-response probe without exposing causes.
func (e registryFailure) As(target any) bool {
	if response, ok := target.(**responseBackedRegistryFailure); ok && e.status != 0 {
		*response = &responseBackedRegistryFailure{e}
		return true
	}
	return false
}

func boundedRegistryFailure(category, stage string, status int) registryFailure {
	switch category {
	case "upstream_status", "authentication", "dns", "timeout", "read", "policy":
	default:
		category = "unknown"
	}
	switch stage {
	case "registry-ping", "auth", "manifest", "blob", "transport":
	default:
		stage = "unknown"
	}
	if status < 100 || status > 599 {
		status = 0
	}
	return registryFailure{category: category, stage: stage, status: status}
}

type registryStageKey struct{}

func registryRequestStage(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "unknown"
	}
	if stage, ok := r.Context().Value(registryStageKey{}).(string); ok {
		return boundedRegistryFailure("unknown", stage, 0).stage
	}
	if r.URL.Path == "/v2/" {
		return "registry-ping"
	}
	if registryAuthority(r.URL) == "auth.docker.io" && r.URL.Path == "/token" {
		return "auth"
	}
	// Repository components may themselves be named manifests or blobs. Pull
	// endpoints end with /<endpoint>/<reference-or-digest> after the repository.
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) >= 5 && parts[0] == "" && parts[1] == "v2" && parts[len(parts)-1] != "" {
		switch parts[len(parts)-2] {
		case "manifests":
			return "manifest"
		case "blobs":
			return "blob"
		}
	}
	return "unknown"
}

func registryErrorDiagnostic(err error) registryFailure {
	// An insecure registry ping can join an HTTPS transport failure with an
	// HTTP response failure. Prefer the actual response over the failed probe.
	var response *responseBackedRegistryFailure
	if errors.As(err, &response) {
		return boundedRegistryFailure(response.RegistryDiagnostic())
	}
	var statusErr *transport.Error
	if errors.As(err, &statusErr) {
		category := "upstream_status"
		if statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden {
			category = "authentication"
		}
		stage := registryRequestStage(statusErr.Request)
		if stage == "unknown" && statusErr.Request != nil && statusErr.Request.URL != nil {
			cdnPolicy := registryPolicy{hub: true, primary: "ghcr.io"}
			if cdnPolicy.cdn(registryAuthority(statusErr.Request.URL)) {
				stage = "blob"
			}
		}
		return boundedRegistryFailure(category, stage, statusErr.StatusCode)
	}
	var diagnostic interface{ RegistryDiagnostic() (string, string, int) }
	if errors.As(err, &diagnostic) {
		return boundedRegistryFailure(diagnostic.RegistryDiagnostic())
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return boundedRegistryFailure("dns", "transport", 0)
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return boundedRegistryFailure("timeout", "transport", 0)
	}
	return boundedRegistryFailure("unknown", "unknown", 0)
}

func safeRegistryError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	d := registryErrorDiagnostic(err)
	return &d
}

// Each response owns immutable metadata, including successful HTTP status codes
// whose bodies fail later. Embedding preserves the original Close behavior.
type registryResponseBody struct {
	io.ReadCloser
	stage  string
	status int
}

func (b registryResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return n, safeRegistryError(err)
		}
		category := registryErrorDiagnostic(err).category
		if category == "unknown" {
			category = "read"
		}
		err = boundedRegistryFailure(category, b.stage, b.status)
	}
	return n, err
}
