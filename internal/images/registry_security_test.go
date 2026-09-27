package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func isolateRegistryCredentials(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
}

type externalURLImage struct {
	v1.Image
	manifest v1.Manifest
	raw      []byte
}

func (i externalURLImage) Manifest() (*v1.Manifest, error) { m := i.manifest; return &m, nil }
func (i externalURLImage) RawManifest() ([]byte, error)    { return append([]byte(nil), i.raw...), nil }
func (i externalURLImage) Digest() (v1.Hash, error) {
	h, _, err := v1.SHA256(bytes.NewReader(i.raw))
	return h, err
}
func (i externalURLImage) Size() (int64, error)                { return int64(len(i.raw)), nil }
func (i externalURLImage) MediaType() (types.MediaType, error) { return i.manifest.MediaType, nil }

func TestPullNeverRequestsForeignLayerURLChosenByRegistry(t *testing.T) {
	isolateRegistryCredentials(t)
	var externalRequests atomic.Int32
	var externalAuthorization atomic.Value
	externalLayer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalRequests.Add(1)
		externalAuthorization.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "not a layer")
	}))
	t.Cleanup(externalLayer.Close)
	baseRegistry := registry.New()
	var hiddenBlobPath atomic.Value
	var hideBlob atomic.Bool
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hideBlob.Load() && r.URL.Path == hiddenBlobPath.Load().(string) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		baseRegistry.ServeHTTP(w, r)
	}))
	t.Cleanup(registryServer.Close)
	transport := registryServer.Client().Transport
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/foreign:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	baseImage, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := baseImage.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", "amd64"
	baseImage, err = mutate.ConfigFile(baseImage, config)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := baseImage.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	foreignBlobPath := "/v2/team/foreign/blobs/" + manifest.Layers[0].Digest.String()
	manifest.Layers[0].MediaType = types.DockerForeignLayer
	manifest.Layers[0].URLs = []string{externalLayer.URL + "/attacker-selected"}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	image := externalURLImage{Image: baseImage, manifest: *manifest, raw: rawManifest}
	if err := remote.Write(parsed, image, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	externalRequests.Store(0)
	hiddenBlobPath.Store(foreignBlobPath)
	hideBlob.Store(true)
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if got := externalRequests.Load(); got != 0 {
		t.Fatalf("foreign layer URL selected by registry received %d request(s), Authorization=%q", got, externalAuthorization.Load())
	}
}

func TestPullRejectsRegistryControlledExternalConfigURLWithoutRequest(t *testing.T) {
	isolateRegistryCredentials(t)
	var configRequests atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		configRequests.Add(1)
		_, _ = io.WriteString(w, "config")
	}))
	t.Cleanup(sink.Close)
	registryServer := httptest.NewServer(registry.New())
	t.Cleanup(registryServer.Close)
	transport := registryServer.Client().Transport
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/external-config:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OS, cfg.Architecture = "linux", "amd64"
	image, err = mutate.ConfigFile(image, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := image.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.Config.URLs = []string{sink.URL + "/external-config"}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	imageWithURL := externalURLImage{Image: image, manifest: *manifest, raw: raw}
	if err := remote.Write(parsed, imageWithURL, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "external config URLs") {
		t.Fatalf("external OCI config URL error=%v", err)
	}
	if got := configRequests.Load(); got != 0 {
		t.Fatalf("external config URL received %d request(s)", got)
	}
}

func TestPullRejectsUnknownRegistryArtifactMediaTypeWithoutPublishing(t *testing.T) {
	isolateRegistryCredentials(t)
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	transport := server.Client().Transport
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/unknown-artifact:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	image, err := random.Image(64, 1)
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
	manifest, err := image.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.MediaType = types.MediaType("application/vnd.example.sbom.v1+json")
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact := externalURLImage{Image: image, manifest: *manifest, raw: raw}
	if err := remote.Write(parsed, artifact, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "unsupported OCI artifact") {
		t.Fatalf("unknown artifact media type pull error=%v", err)
	}
	if refs, err := store.List(context.Background()); err != nil || len(refs) != 0 {
		t.Fatalf("unsupported artifact was published: refs=%+v err=%v", refs, err)
	}
}

func TestPullDoesNotFollowBearerRealmToUntrustedInternalService(t *testing.T) {
	isolateRegistryCredentials(t)
	var tokenRequests atomic.Int32
	var authorization atomic.Value
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		authorization.Store(r.Header.Get("Authorization"))
		_, _ = fmt.Fprint(w, `{"token":"synthetic-token"}`)
	}))
	t.Cleanup(sink.Close)
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service=%q,scope=%q`, sink.URL+"/token", "attacker-controlled", "repository:team/private:pull"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(registryServer.Close)
	transport := registryServer.Client().Transport
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/private:latest"
	if err := store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatal("challenge-only fake registry unexpectedly returned an image")
	}
	if got := tokenRequests.Load(); got != 0 {
		t.Fatalf("untrusted Bearer realm was contacted %d time(s), Authorization=%q", got, authorization.Load())
	}
}

func TestRegistrySafeErrorsPreserveCancellationButRedactTransportDetails(t *testing.T) {
	if err := safeRegistryError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled registry request error=%v", err)
	}
	if err := safeRegistryError(context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out registry request error=%v", err)
	}
	raw := errors.New("https://private.example/path?credential=secret")
	if err := safeRegistryError(raw); err == nil || strings.Contains(err.Error(), "private.example") || strings.Contains(err.Error(), "credential=secret") {
		t.Fatalf("unsafe registry diagnostic not redacted: %v", err)
	}
}
