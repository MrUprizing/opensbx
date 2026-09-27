package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

type requestRecorder struct {
	http.RoundTripper
	mu    sync.Mutex
	paths []string
}

func (r *requestRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	return r.RoundTripper.RoundTrip(req)
}
func (r *requestRecorder) reset() { r.mu.Lock(); r.paths = nil; r.mu.Unlock() }
func (r *requestRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func registryIndex(t *testing.T, version int) (v1.ImageIndex, map[string]v1.Hash) {
	t.Helper()
	images := map[string]v1.Image{}
	layers := map[string]v1.Hash{}
	for _, arch := range []string{"amd64", "arm64"} {
		img, err := random.Image(int64(128+version), 1)
		if err != nil {
			t.Fatal(err)
		}
		config, err := img.ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		config.OS, config.Architecture = "linux", arch
		config.Config.Env = []string{fmt.Sprintf("FIXTURE_VERSION=%d", version)}
		img, err = mutate.ConfigFile(img, config)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := img.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		images[arch] = img
		layers[arch] = manifest.Layers[0].Digest
	}
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: images["amd64"], Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: images["arm64"], Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	return index, layers
}

func TestPullPreservesRootAndDownloadsOnlyExplicitPlatformUntilPrepared(t *testing.T) {
	ctx := context.Background()
	registryServer := httptest.NewServer(registry.New())
	t.Cleanup(registryServer.Close)
	recorder := &requestRecorder{RoundTripper: http.DefaultTransport}
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/app:stable"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	index, layerDigests := registryIndex(t, 1)
	if err := remote.WriteIndex(parsed, index, remote.WithTransport(recorder)); err != nil {
		t.Fatal(err)
	}
	rootDigest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	amdImage, err := index.Image(indexIndexChild(t, index, "amd64"))
	if err != nil {
		t.Fatal(err)
	}
	amdDigest, err := amdImage.Digest()
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir(), WithTransport(recorder))
	if err != nil {
		t.Fatal(err)
	}
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm64"}
	recorder.reset()
	if err := store.Pull(ctx, ref, amd); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(ctx, ref, amd)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Root.Digest != rootDigest || artifact.Manifest.Digest != amdDigest {
		t.Fatalf("root=%s selected=%s; want root=%s amd=%s", artifact.Root.Digest, artifact.Manifest.Digest, rootDigest, amdDigest)
	}
	if paths := recorder.snapshot(); pathHasDigest(paths, layerDigests["arm64"]) {
		t.Fatalf("amd64 pull fetched arm64 layer: %v", paths)
	}
	fullArchive := t.TempDir() + "/sparse-full.tar"
	if err := store.Export(ctx, ref, fullArchive, amd, true); err == nil {
		t.Fatal("full-index export succeeded while arm64 graph is sparse")
	}
	variantArchive := t.TempDir() + "/amd64.tar"
	if err := store.Export(ctx, ref, variantArchive, amd, false); err != nil {
		t.Fatalf("prepared variant export: %v", err)
	}
	if _, err := store.Resolve(ctx, ref, arm); err == nil {
		t.Fatal("unprepared arm64 variant resolved without explicit preparation")
	}
	recorder.reset()
	unsupported := v1.Platform{OS: "linux", Architecture: "s390x"}
	if err := store.Pull(ctx, ref, unsupported); err == nil {
		t.Fatal("unsupported platform pull succeeded")
	}
	if paths := recorder.snapshot(); pathHasDigest(paths, layerDigests["amd64"]) || pathHasDigest(paths, layerDigests["arm64"]) {
		t.Fatalf("unsupported platform pull fetched variant blobs: %v", paths)
	}
	recorder.reset()
	if err := store.Pull(ctx, ref, arm); err != nil {
		t.Fatal(err)
	}
	armArtifact, err := store.Resolve(ctx, ref, arm)
	if err != nil {
		t.Fatal(err)
	}
	if armArtifact.Root.Digest != rootDigest || armArtifact.Manifest.Digest == artifact.Manifest.Digest {
		t.Fatalf("prepared variants lost root or share manifest: amd=%+v arm=%+v", artifact, armArtifact)
	}
	if paths := recorder.snapshot(); !pathHasDigest(paths, layerDigests["arm64"]) {
		t.Fatalf("explicit arm64 preparation did not fetch selected layer: %v", paths)
	}
	fullArchive = t.TempDir() + "/complete-full.tar"
	if err := store.Export(ctx, ref, fullArchive, amd, true); err != nil {
		t.Fatalf("complete index export after preparing both variants: %v", err)
	}
}

func TestPullingMutableTagRepinsItToNewOCIIndex(t *testing.T) {
	ctx := context.Background()
	registryServer := httptest.NewServer(registry.New())
	t.Cleanup(registryServer.Close)
	recorder := &requestRecorder{RoundTripper: http.DefaultTransport}
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/mutable:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := registryIndex(t, 1)
	if err := remote.WriteIndex(parsed, first, remote.WithTransport(recorder)); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir(), WithTransport(recorder))
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	if err := store.Pull(ctx, ref, platform); err != nil {
		t.Fatal(err)
	}
	firstDigest, _ := first.Digest()
	second, _ := registryIndex(t, 2)
	if err := remote.WriteIndex(parsed, second, remote.WithTransport(recorder)); err != nil {
		t.Fatal(err)
	}
	if err := store.Pull(ctx, ref, platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(ctx, ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, _ := second.Digest()
	if artifact.Root.Digest != secondDigest || artifact.Root.Digest == firstDigest {
		t.Fatalf("mutable tag retained stale root: got=%s old=%s new=%s", artifact.Root.Digest, firstDigest, secondDigest)
	}
}

func TestPullPreparesNestedSparseIndexesOnlyForRequestedPlatform(t *testing.T) {
	ctx := context.Background()
	registryServer := httptest.NewServer(registry.New())
	t.Cleanup(registryServer.Close)
	recorder := &requestRecorder{RoundTripper: http.DefaultTransport}
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/nested/app:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	inner, layers := registryIndex(t, 3)
	outer := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: inner})
	if err := remote.WriteIndex(parsed, outer, remote.WithTransport(recorder)); err != nil {
		t.Fatal(err)
	}
	recorder.reset()
	store, err := Open(t.TempDir(), WithTransport(recorder))
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	if err := store.Pull(ctx, ref, platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(ctx, ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := outer.Digest()
	wantArm, _ := inner.Image(indexIndexChild(t, inner, "arm64"))
	wantManifest, _ := wantArm.Digest()
	if artifact.Root.Digest != wantRoot || artifact.Manifest.Digest != wantManifest {
		t.Fatalf("nested identities root=%s manifest=%s want root=%s manifest=%s", artifact.Root.Digest, artifact.Manifest.Digest, wantRoot, wantManifest)
	}
	paths := recorder.snapshot()
	if pathHasDigest(paths, layers["amd64"]) || !pathHasDigest(paths, layers["arm64"]) {
		t.Fatalf("nested platform pull fetched incorrect leaves: %v", paths)
	}
	if err := store.Export(ctx, ref, t.TempDir()+"/all-platforms.tar", platform, true); err == nil {
		t.Fatal("sparse nested graph exported as complete index")
	}
}

func TestPullPropagatesRegistryUnavailableWithoutPublishingImage(t *testing.T) {
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fixture registry unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(registryServer.Close)
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/offline:latest"
	store, err := Open(t.TempDir(), WithTransport(registryServer.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || err.Error() != "registry request failed or was rejected by local URL policy" || strings.Contains(err.Error(), "fixture registry unavailable") {
		t.Fatalf("unavailable registry diagnostics were not safely redacted: %v", err)
	}
	items, err := store.List(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("failed registry pull published refs: items=%+v err=%v", items, err)
	}
}

type cancelPullTransport struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (r *cancelPullTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	r.cancel()
	return nil, context.Canceled
}

func TestPullPropagatesTransportCancellationWithoutPublishingReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	transport := &cancelPullTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	transport.cancel = cancel
	defer cancel()
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Pull(ctx, "registry.example/team/canceled:latest", v1.Platform{OS: "linux", Architecture: "amd64"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Pull canceled by transport error=%v, want context.Canceled", err)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("canceled pull reached transport %d times, want one", got)
	}
	if refs, err := store.List(context.Background()); err != nil || len(refs) != 0 {
		t.Fatalf("canceled pull published references=%+v err=%v", refs, err)
	}
}

func TestPullRejectsRegistryPlatformMismatchAndExternalChildURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
		urls []string
		want string
	}{
		{name: "config disagrees with selected descriptor", want: "image platform does not match linux/arm64"},
		{name: "registry-selected child URL", urls: []string{"https://127.0.0.1/private/blob"}, want: "registry-controlled external descriptor URLs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
			server := httptest.NewServer(registry.New())
			t.Cleanup(server.Close)
			ref := strings.TrimPrefix(server.URL, "http://") + "/team/untrusted-index:latest"
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
			claimed := v1.Platform{OS: "linux", Architecture: "arm64"}
			index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
				Add:        image,
				Descriptor: v1.Descriptor{Platform: &claimed, URLs: tc.urls},
			})
			transport := server.Client().Transport
			if err := remote.WriteIndex(parsed, index, remote.WithTransport(transport)); err != nil {
				t.Fatal(err)
			}
			store, err := Open(t.TempDir(), WithTransport(transport))
			if err != nil {
				t.Fatal(err)
			}
			err = store.Pull(context.Background(), ref, claimed)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Pull() error=%v want substring %q", err, tc.want)
			}
			if refs, err := store.List(context.Background()); err != nil || len(refs) != 0 {
				t.Fatalf("rejected registry graph was published: refs=%+v err=%v", refs, err)
			}
		})
	}
}

func TestPullRejectsMalformedReferenceBeforeRegistryAccess(t *testing.T) {
	transport := &requestRecorder{RoundTripper: http.DefaultTransport}
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pull(context.Background(), "https://", v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatal("malformed image reference accepted")
	}
	if got := transport.snapshot(); len(got) != 0 {
		t.Fatalf("malformed reference reached registry transport: %v", got)
	}
}

func TestDefaultRegistryPolicyAllowsExplicitLoopbackRegistryAndVerifiesImage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/local:latest"
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
	if err := remote.Write(parsed, image, remote.WithTransport(server.Client().Transport)); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir()) // exercise the production DNS-pinned policy transport
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pull(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"}); err != nil {
		t.Fatalf("explicit loopback registry pull: %v", err)
	}
	if _, err := store.Resolve(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"}); err != nil {
		t.Fatalf("verify pulled loopback image: %v", err)
	}
}

func TestBlockedRegistryPullDoesNotBlockPreparedImageReads(t *testing.T) {
	baseRegistry := registry.New()
	var block atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/slow") {
			once.Do(func() { close(entered) })
			<-release
		}
		baseRegistry.ServeHTTP(w, r)
	}))
	t.Cleanup(registryServer.Close)
	transport := registryServer.Client().Transport
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/slow:slow"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := registryIndex(t, 42)
	if err := remote.WriteIndex(parsed, index, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir(), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	existingArchive, platform := testsupport.OCIArchive(t)
	const existingRef = "example.test/team/prepared:offline"
	if err := store.Import(context.Background(), existingArchive, existingRef, platform); err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	pullDone := make(chan error, 1)
	go func() { pullDone <- store.Pull(context.Background(), ref, platform) }()
	pullFinished := false
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if !pullFinished {
			<-pullDone
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("registry fixture did not reach its deterministic blocked manifest response")
	}
	type readResult struct {
		list                []sandbox.ImageSummary
		artifact            Artifact
		listErr, resolveErr error
	}
	readsDone := make(chan readResult, 1)
	go func() {
		list, listErr := store.List(context.Background())
		artifact, resolveErr := store.Resolve(context.Background(), existingRef, platform)
		readsDone <- readResult{list: list, artifact: artifact, listErr: listErr, resolveErr: resolveErr}
	}()
	var reads readResult
	select {
	case reads = <-readsDone:
	case <-time.After(1500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		pullErr := <-pullDone
		pullFinished = true
		<-readsDone
		t.Fatalf("slow registry pull held catalog read access; prepared List/Resolve blocked until remote response (pull err=%v)", pullErr)
	}
	if reads.listErr != nil || len(reads.list) != 1 {
		t.Fatalf("prepared image list while pull blocked=%+v err=%v", reads.list, reads.listErr)
	}
	if reads.resolveErr != nil || reads.artifact.Root.Digest.Hex == "" {
		t.Fatalf("prepared image resolution while pull blocked=%+v err=%v", reads.artifact, reads.resolveErr)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-pullDone; err != nil {
		pullFinished = true
		t.Fatalf("unblocked registry pull: %v", err)
	}
	pullFinished = true
}

func indexIndexChild(t *testing.T, index v1.ImageIndex, arch string) v1.Hash {
	t.Helper()
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range manifest.Manifests {
		if child.Platform != nil && child.Platform.Architecture == arch {
			return child.Digest
		}
	}
	t.Fatalf("no %s descriptor in fixture index", arch)
	return v1.Hash{}
}
func pathHasDigest(paths []string, digest v1.Hash) bool {
	for _, path := range paths {
		if strings.Contains(path, digest.Hex) {
			return true
		}
	}
	return false
}
