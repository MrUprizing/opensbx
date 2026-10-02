package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/klauspost/compress/zstd"
	"opensbx/internal/images"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

func TestDockerMaterializeLoadsAndReusesVerifiedDaemonNativeImageID(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(ctx, archive, "example.test/team/app:mutable", platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(ctx, "example.test/team/app:mutable", sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	dc, fixture := newDockerFixture(t)
	configureDockerCacheFixture(fixture, image)
	materialized, err := dc.Materialize(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	if materialized != fixture.cacheNativeID || materialized == image.ConfigDigest {
		t.Fatalf("Docker native handle=%q want daemon ID %q distinct from source config digest %q", materialized, fixture.cacheNativeID, image.ConfigDigest)
	}
	fixture.mu.Lock()
	loadedArchive := append([]byte(nil), fixture.loadedArchive...)
	requests := append([]string(nil), fixture.requests...)
	fixture.mu.Unlock()
	if len(loadedArchive) == 0 {
		t.Fatal("materializer did not send a Docker archive")
	}
	tr := tar.NewReader(bytes.NewReader(loadedArchive))
	var tags []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "manifest.json" {
			var manifest []struct {
				RepoTags []string `json:"RepoTags"`
			}
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest) > 0 {
				tags = manifest[0].RepoTags
			}
			break
		}
	}
	wantTag := "localhost/opensbx-cache:" + strings.TrimPrefix(image.ManifestDigest, "sha256:")
	if len(tags) != 1 || tags[0] != wantTag {
		t.Fatalf("Docker archive tags=%v want content-pinned %q", tags, wantTag)
	}
	loadCount, inspectCount, saveCount := 0, 0, 0
	wantInspect := "GET /images/" + fixture.cacheTag + "/json"
	for _, req := range requests {
		if req == "POST /images/load" {
			loadCount++
		}
		if req == wantInspect {
			inspectCount++
		}
		if req == "GET /images/get" {
			saveCount++
		}
	}
	if loadCount != 1 || inspectCount != 2 || saveCount != 1 {
		t.Fatalf("cache miss/load/verify requests=%v, want one load, two exact-tag inspections and one archive export", requests)
	}
	if got, err := dc.Materialize(ctx, image); err != nil || got != fixture.cacheNativeID {
		t.Fatalf("cache hit handle=%q err=%v", got, err)
	}
	fixture.mu.Lock()
	loadCount, inspectCount, saveCount = 0, 0, 0
	for _, req := range fixture.requests {
		if req == "POST /images/load" {
			loadCount++
		}
		if req == wantInspect {
			inspectCount++
		}
		if req == "GET /images/get" {
			saveCount++
		}
	}
	finalRequests := append([]string(nil), fixture.requests...)
	fixture.mu.Unlock()
	if loadCount != 1 || inspectCount != 3 || saveCount != 2 {
		t.Fatalf("verified cache hit must inspect/export actual daemon content without import: requests=%v", finalRequests)
	}
}

func configureDockerCacheFixture(fixture *dockerAPIFixture, image sandbox.Image) {
	fixture.cacheDigest = image.ConfigDigest
	fixture.cacheTag = "localhost/opensbx-cache:" + strings.TrimPrefix(image.ManifestDigest, "sha256:")
	fixture.cacheNativeID = "sha256:" + strings.Repeat("b", 64)
}

func configureDockerCacheFixtureWithID(fixture *dockerAPIFixture, image sandbox.Image, nativeID string) {
	configureDockerCacheFixture(fixture, image)
	fixture.cacheNativeID = nativeID
}

func TestValidateEndpointAcceptsOnlyLocalDockerTransports(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		valid    bool
	}{
		{"unix:///var/run/docker.sock", true}, {"tcp://127.0.0.1:2375", true}, {"http://[::1]:2375", true},
		{"tcp://0.0.0.0:2375", false}, {"tcp://192.0.2.1:2375", false}, {"tcp://user@localhost:2375", false},
		{"https://localhost:2375/path", false}, {"npipe:////./pipe/docker_engine", true}, {"tcp://[not-an-ip", false},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			wantValid := tc.valid
			if runtime.GOOS == "windows" && strings.HasPrefix(tc.endpoint, "unix://") {
				wantValid = false
			}
			err := ValidateEndpoint(tc.endpoint)
			if (err == nil) != wantValid {
				t.Fatalf("ValidateEndpoint(%q) err=%v want valid=%v", tc.endpoint, err, wantValid)
			}
		})
	}
}

func TestDockerCapabilitiesNormalizeDaemonPlatformAndVersion(t *testing.T) {
	dc, _ := newDockerFixture(t)
	caps, err := dc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Runtime != "docker" || caps.Version != "27.1.2/docker-archive-v2" || caps.Platform.OS != "linux" || caps.Platform.Architecture != "arm64" || !caps.Pause || !caps.FractionalCPU {
		t.Fatalf("Docker capabilities=%+v", caps)
	}
}

func TestDockerCapabilitiesPropagatesDaemonInfoFailures(t *testing.T) {
	client, fixture := newDockerFixture(t)
	fixture.fail["GET /info"] = http.StatusInternalServerError
	caps, err := client.Capabilities(context.Background())
	if err == nil || caps.Runtime != "" || caps.Platform.OS != "" {
		t.Fatalf("failed Docker info probe returned usable capabilities=%+v err=%v", caps, err)
	}
}

func TestDockerMaterializeReportsLoadStreamAndPostLoadVerificationFailures(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/cache-errors:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(context.Background(), ref, sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		loadBody     string
		loadStatus   int
		inspectID    string
		inspectCodes []int
		want         string
	}{
		{name: "daemon load error", loadBody: `{"error":"synthetic daemon rejection"}` + "\n", want: "synthetic daemon rejection"},
		{name: "Docker API load failure", loadStatus: http.StatusInternalServerError, want: "fixture error"},
		{name: "malformed load stream", loadBody: `{"error":`, want: "unexpected EOF"},
		{name: "post-load inspect failure", inspectCodes: []int{404, 500}, want: "fixture error"},
		{name: "post-load invalid native ID", inspectID: "sha256:wrong-config", want: "no immutable image ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fixture := newDockerFixture(t)
			configureDockerCacheFixture(fixture, image)
			fixture.cacheLoadBody = tc.loadBody
			fixture.cacheInspectID = tc.inspectID
			if tc.loadStatus != 0 {
				fixture.fail["POST /images/load"] = tc.loadStatus
			}
			if len(tc.inspectCodes) != 0 {
				fixture.responses = make(map[string][]int)
				fixture.responses["GET /images/"+fixture.cacheTag+"/json"] = append([]int(nil), tc.inspectCodes...)
			}
			if _, err := client.Materialize(context.Background(), image); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Materialize() error=%v want substring %q", err, tc.want)
			}
		})
	}
}

func TestDockerMaterializeSupportsClassicAndContainerdNativeIDs(t *testing.T) {
	for _, mode := range []string{"classic-config-id", "containerd-native-id"} {
		t.Run(mode, func(t *testing.T) {
			image := dockerCacheTestImage(t)
			nativeID := "sha256:" + strings.Repeat("c", 64)
			if mode == "classic-config-id" {
				nativeID = image.ConfigDigest
			}
			client, fixture := newDockerFixture(t)
			configureDockerCacheFixtureWithID(fixture, image, nativeID)
			got, err := client.Materialize(context.Background(), image)
			if err != nil || got != nativeID {
				t.Fatalf("Materialize() native ID=%q err=%v want actual daemon ID %q", got, err, nativeID)
			}
		})
	}
}

func TestDockerMaterializeRejectsUnverifiedExistingPrivateCacheWithoutChangingIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*testing.T, []byte, sandbox.Image) ([]byte, sandbox.Image, string)
		mode      string
		wantError string
	}{
		{name: "wrong config", mutate: func(t *testing.T, _ []byte, _ sandbox.Image) ([]byte, sandbox.Image, string) {
			return dockerArchiveForImage(t, dockerCacheTestImage(t)), sandbox.Image{}, "config digest mismatch"
		}},
		{name: "wrong platform", mutate: func(_ *testing.T, archive []byte, image sandbox.Image) ([]byte, sandbox.Image, string) {
			image.Platform.OS = "windows"
			return archive, image, "platform mismatch"
		}},
		{name: "changed layer bytes", mutate: func(t *testing.T, archive []byte, image sandbox.Image) ([]byte, sandbox.Image, string) {
			return mutateDockerLayerBytes(t, archive), image, "layer content mismatch"
		}},
		{name: "truncated archive", mutate: func(_ *testing.T, archive []byte, image sandbox.Image) ([]byte, sandbox.Image, string) {
			return archive[:len(archive)/2], image, ""
		}},
		{name: "manifest layer count differs from config", mutate: func(t *testing.T, archive []byte, image sandbox.Image) ([]byte, sandbox.Image, string) {
			return mutateDockerArchiveLayers(t, archive, nil), image, "layer count mismatch"
		}},
		{name: "oversized advertised export without allocating its declared body", mutate: func(_ *testing.T, archive []byte, image sandbox.Image) ([]byte, sandbox.Image, string) {
			return archive, image, ""
		}, mode: "oversized-advertised"},
		{name: "reader failure during export", mutate: func(_ *testing.T, archive []byte, image sandbox.Image) ([]byte, sandbox.Image, string) {
			return archive, image, ""
		}, mode: "truncated-reader"},
		{name: "Docker image-save API failure", mutate: func(_ *testing.T, archive []byte, image sandbox.Image) ([]byte, sandbox.Image, string) {
			return archive, image, ""
		}, mode: "status-error", wantError: "fixture image-save error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := dockerCacheTestImage(t)
			client, fixture, validArchive := seedDockerCacheFixture(t, image)
			badArchive, candidate, wantErr := tc.mutate(t, validArchive, image)
			if tc.wantError != "" {
				wantErr = tc.wantError
			}
			if tc.name == "wrong config" {
				badArchive = dockerArchiveForImage(t, dockerCacheTestImage(t))
				candidate = image
			}
			fixture.mu.Lock()
			fixture.cacheSavedArchive = badArchive
			fixture.cacheSaveMode = tc.mode
			originalNativeID := fixture.cacheNativeID
			originalArchive := append([]byte(nil), fixture.loadedArchive...)
			fixture.mu.Unlock()
			if candidate.ConfigDigest == "" {
				candidate = image
			}
			_, err := client.Materialize(context.Background(), candidate)
			if err == nil || wantErr != "" && !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("Materialize() error=%v want rejection containing %q", err, wantErr)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			loads := 0
			for _, request := range fixture.requests {
				if request == "POST /images/load" {
					loads++
				}
			}
			if loads != 1 || fixture.cacheNativeID != originalNativeID || !bytes.Equal(fixture.loadedArchive, originalArchive) {
				t.Fatalf("failed cache verification mutated the stale private tag: loads=%d id=%s archiveSame=%t", loads, fixture.cacheNativeID, bytes.Equal(fixture.loadedArchive, originalArchive))
			}
		})
	}
}

func TestDockerCacheVerifierBindsExpectedSourceManifestAndConfig(t *testing.T) {
	image := dockerCacheTestImage(t)
	client, fixture, _ := seedDockerCacheFixture(t, image)
	for _, tc := range []struct {
		name string
		edit func(*sandbox.Image)
		want string
	}{
		{name: "manifest pin", edit: func(image *sandbox.Image) { image.ManifestDigest = "sha256:" + strings.Repeat("0", 64) }, want: "source manifest/config mismatch"},
		{name: "config pin", edit: func(image *sandbox.Image) { image.ConfigDigest = "sha256:" + strings.Repeat("0", 64) }, want: "source manifest/config mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := image
			tc.edit(&candidate)
			_, err := client.verifyCache(context.Background(), candidate, fixture.cacheNativeID, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verifyCache() error=%v want %q", err, tc.want)
			}
		})
	}
}

func TestDockerVerifyCacheLayersRejectsDuplicateNonregularAndMissingEntries(t *testing.T) {
	image := dockerCacheTestImage(t)
	_, _, archive := seedDockerCacheFixture(t, image)
	manifest := dockerArchiveManifest(t, archive)
	if len(manifest) != 1 || len(manifest[0].Layers) == 0 {
		t.Fatalf("expected one archive manifest with a layer: %+v", manifest)
	}
	layerPath := manifest[0].Layers[0]
	config, err := image.Content.ConfigFile()
	if err != nil || len(config.RootFS.DiffIDs) == 0 {
		t.Fatalf("read source rootfs diff IDs: %+v err=%v", config, err)
	}
	diffID := config.RootFS.DiffIDs[0]
	for _, tc := range []struct {
		name    string
		archive []byte
		paths   []string
		diffIDs []v1.Hash
		want    string
	}{
		{name: "duplicate archive entry", archive: duplicateDockerLayerEntry(t, archive, layerPath), paths: []string{layerPath}, diffIDs: []v1.Hash{diffID}, want: "unique regular files"},
		{name: "nonregular archive entry", archive: changeDockerLayerToDirectory(t, archive, layerPath), paths: []string{layerPath}, diffIDs: []v1.Hash{diffID}, want: "unique regular files"},
		{name: "referenced layer absent", archive: archive, paths: []string{"missing/layer.tar"}, diffIDs: []v1.Hash{diffID}, want: "layer missing"},
		{name: "conflicting identities for duplicate path", archive: archive, paths: []string{layerPath, layerPath}, diffIDs: []v1.Hash{diffID, v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}}, want: "conflicting layer identities"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "verified.tar")
			if err := os.WriteFile(path, tc.archive, 0600); err != nil {
				t.Fatal(err)
			}
			err := verifyCacheLayers(context.Background(), path, tc.paths, tc.diffIDs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verifyCacheLayers() error=%v want %q", err, tc.want)
			}
		})
	}
}

func TestDockerCacheLayerHashAcceptsPlainGzipAndZstdAndRejectsCorruption(t *testing.T) {
	plain := []byte("small synthetic uncompressed layer")
	want, _, err := v1.SHA256(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	var gzipBytes bytes.Buffer
	gz := gzip.NewWriter(&gzipBytes)
	if _, err := gz.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	zstdBytes := encoder.EncodeAll(plain, nil)
	encoder.Close()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "plain", data: plain},
		{name: "gzip", data: gzipBytes.Bytes()},
		{name: "zstd", data: zstdBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cacheLayerHash(context.Background(), bytes.NewReader(tc.data))
			if err != nil || got != want {
				t.Fatalf("cacheLayerHash()=%s err=%v want %s", got, err, want)
			}
		})
	}
	if _, err := cacheLayerHash(context.Background(), bytes.NewReader([]byte{0x1f, 0x8b, 0, 0})); err == nil {
		t.Fatal("corrupt gzip layer accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cacheLayerHash(canceled, bytes.NewReader(plain)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cacheLayerHash() canceled read error=%v want context.Canceled", err)
	}
}

type imageDigestError struct {
	v1.Image
	err error
}

func (i imageDigestError) Digest() (v1.Hash, error) { return v1.Hash{}, i.err }

var errSyntheticCacheBodyRead = errors.New("synthetic Docker response-body transport error")

type cacheBodyReadGate struct {
	source      io.ReadCloser
	entered     chan struct{}
	closed      chan struct{}
	release     chan struct{}
	readFailure chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
	readErr     error
	closeErr    error
}

func (b *cacheBodyReadGate) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-b.closed:
	}
	close(b.readFailure)
	return 0, b.readErr
}

func (b *cacheBodyReadGate) Close() error {
	b.closeOnce.Do(func() {
		close(b.closed)
		b.closeErr = b.source.Close()
	})
	return b.closeErr
}

type cacheBodyReadGateTransport struct {
	base http.RoundTripper
	body *cacheBodyReadGate
}

func (t *cacheBodyReadGateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/images/get") {
		t.body.source = response.Body
		response.Body = t.body
	}
	return response, err
}

func newCacheBodyReadGateClient(t *testing.T, image sandbox.Image) (*Client, *dockerAPIFixture, *cacheBodyReadGate) {
	t.Helper()
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	t.Cleanup(base.CloseIdleConnections)
	body := &cacheBodyReadGate{
		entered:     make(chan struct{}),
		closed:      make(chan struct{}),
		release:     make(chan struct{}),
		readFailure: make(chan struct{}),
		readErr:     errSyntheticCacheBodyRead,
	}
	client, fixture := newDockerFixtureWithRoundTripper(t, &cacheBodyReadGateTransport{base: base, body: body})
	configureDockerCacheFixture(fixture, image)
	archive := dockerArchiveForImage(t, image)
	fixture.mu.Lock()
	fixture.cacheLoaded = true
	fixture.loadedArchive = archive
	fixture.cacheSavedArchive = archive
	fixture.mu.Unlock()
	return client, fixture, body
}

func TestDockerVerifyCachePropagatesDigestAndExportFileErrors(t *testing.T) {
	image := dockerCacheTestImage(t)
	client, fixture, _ := seedDockerCacheFixture(t, image)

	digestFailure := image
	digestFailure.Content = imageDigestError{Image: image.Content, err: errors.New("synthetic source digest read failure")}
	if _, err := client.verifyCache(context.Background(), digestFailure, fixture.cacheNativeID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "synthetic source digest read failure") {
		t.Fatalf("verifyCache() digest error=%v want source read error", err)
	}
	missingParent := filepath.Join(t.TempDir(), "missing", "nested")
	if _, err := client.verifyCache(context.Background(), image, fixture.cacheNativeID, missingParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verifyCache() export path error=%v want os.ErrNotExist", err)
	}
}

func TestDockerMaterializeHonorsCancellationDuringCacheExport(t *testing.T) {
	image := dockerCacheTestImage(t)
	client, _, body := newCacheBodyReadGateClient(t, image)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Materialize(ctx, image)
		done <- err
	}()
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Docker image-save body Read did not reach its synchronization barrier")
	}
	cancel()
	select {
	case <-body.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("canceling the caller did not close the image-save response body")
	}
	select {
	case <-body.readFailure:
	case <-time.After(5 * time.Second):
		t.Fatal("closed response-body Read did not return the synthetic transport error")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Materialize() after response-body transport error=%v, want context.Canceled", err)
		}
		if errors.Is(err, errSyntheticCacheBodyRead) {
			t.Fatalf("Materialize() leaked the transport error instead of caller cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Materialize() did not return after its response body was closed")
	}
}

func TestDockerVerifyCachePreservesTransportReadErrorWithActiveContext(t *testing.T) {
	image := dockerCacheTestImage(t)
	client, fixture, body := newCacheBodyReadGateClient(t, image)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	dir := t.TempDir()
	go func() {
		_, err := client.verifyCache(ctx, image, fixture.cacheNativeID, dir)
		done <- err
	}()
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Docker image-save response body was not read")
	}
	close(body.release)
	select {
	case <-body.readFailure:
	case <-time.After(5 * time.Second):
		t.Fatal("active-context read did not return the synthetic transport error")
	}
	select {
	case err := <-done:
		if !errors.Is(err, errSyntheticCacheBodyRead) {
			t.Fatalf("verifyCache() active-context transport error=%v, want original error %v", err, errSyntheticCacheBodyRead)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verifyCache() did not return the active-context transport error")
	}
	if ctx.Err() != nil {
		t.Fatalf("fixture unexpectedly canceled caller context: %v", ctx.Err())
	}
}

func TestDockerMaterializeDoesNotSerializeDifferentImageCacheExports(t *testing.T) {
	imageA := dockerCacheTestImage(t)
	imageB := dockerCacheTestImage(t)
	if imageA.ManifestDigest == imageB.ManifestDigest {
		t.Fatal("independent cache-lock fixture images unexpectedly share a manifest")
	}
	clientA, fixtureA, archiveA := seedDockerCacheFixture(t, imageA)
	clientB, _, _ := seedDockerCacheFixture(t, imageB)

	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	fixtureA.mu.Lock()
	fixtureA.cacheSaveMode = "block-reader"
	fixtureA.cacheSaveEntered = entered
	fixtureA.cacheSaveRelease = release
	fixtureA.cacheSavedArchive = archiveA
	fixtureA.mu.Unlock()

	ctxA, cancelA := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelA()
	doneA := make(chan error, 1)
	go func() {
		_, err := clientA.Materialize(ctxA, imageA)
		doneA <- err
	}()
	ownerFinished := false
	defer func() {
		unblock()
		if !ownerFinished {
			select {
			case err := <-doneA:
				if err != nil {
					t.Errorf("blocked image A cleanup: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("blocked image A materialization did not exit during cleanup")
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("image A did not reach the deterministic blocked archive export")
	}

	ctxB, cancelB := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelB()
	doneB := make(chan error, 1)
	go func() {
		_, err := clientB.Materialize(ctxB, imageB)
		doneB <- err
	}()
	var imageBErr error
	imageBFinishedWhileAWasBlocked := false
	select {
	case imageBErr = <-doneB:
		imageBFinishedWhileAWasBlocked = true
	case <-time.After(2 * time.Second):
	}
	unblock()
	imageAErr := <-doneA
	ownerFinished = true
	if !imageBFinishedWhileAWasBlocked {
		select {
		case imageBErr = <-doneB:
		case <-time.After(5 * time.Second):
			t.Fatal("independent image B did not exit after releasing image A")
		}
	}
	if imageAErr != nil {
		t.Fatalf("image A materialization failed after release: %v", imageAErr)
	}
	if imageBErr != nil {
		t.Fatalf("independent image B materialization failed: %v", imageBErr)
	}
	if !imageBFinishedWhileAWasBlocked {
		t.Fatal("image B cache materialization serialized behind the unrelated blocked image A export")
	}
}

func TestDockerMaterializeCancelsSameImageWaiterWithoutNativeRequests(t *testing.T) {
	image := dockerCacheTestImage(t)
	client, fixture, archive := seedDockerCacheFixture(t, image)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	fixture.mu.Lock()
	fixture.cacheSaveMode = "block-reader"
	fixture.cacheSaveEntered = entered
	fixture.cacheSaveRelease = release
	fixture.cacheSavedArchive = archive
	fixture.mu.Unlock()

	ownerCtx, cancelOwner := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelOwner()
	ownerDone := make(chan error, 1)
	go func() {
		_, err := client.Materialize(ownerCtx, image)
		ownerDone <- err
	}()
	ownerFinished := false
	defer func() {
		unblock()
		if !ownerFinished {
			select {
			case err := <-ownerDone:
				if err != nil {
					t.Errorf("blocked owner cleanup: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("blocked same-image owner did not exit during cleanup")
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not reach the deterministic blocked archive export")
	}

	fixture.mu.Lock()
	requestsBeforeWaiter := len(fixture.requests)
	fixture.mu.Unlock()
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterStarted := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		close(waiterStarted)
		_, err := client.Materialize(waiterCtx, image)
		waiterDone <- err
	}()
	<-waiterStarted
	cancelWaiter()

	var waiterErr error
	waiterReturnedPromptly := false
	select {
	case waiterErr = <-waiterDone:
		waiterReturnedPromptly = true
	case <-time.After(time.Second):
	}
	fixture.mu.Lock()
	requestsWhileOwnerHeld := len(fixture.requests)
	fixture.mu.Unlock()

	unblock()
	ownerErr := <-ownerDone
	ownerFinished = true
	if !waiterReturnedPromptly {
		select {
		case waiterErr = <-waiterDone:
		case <-time.After(5 * time.Second):
			t.Fatal("canceled same-image waiter remained stuck after releasing the owner")
		}
	}
	if ownerErr != nil {
		t.Fatalf("owner materialization failed after release: %v", ownerErr)
	}
	if requestsWhileOwnerHeld != requestsBeforeWaiter {
		t.Fatalf("same-image waiter issued native requests while the owner held its cache lock: before=%d while-held=%d", requestsBeforeWaiter, requestsWhileOwnerHeld)
	}
	if !waiterReturnedPromptly || !errors.Is(waiterErr, context.Canceled) {
		t.Fatalf("same-image canceled waiter returned promptly=%t error=%v; want prompt context.Canceled while owner stays blocked", waiterReturnedPromptly, waiterErr)
	}
}

func TestDockerMaterializeSerializesSameCacheAcrossClientCalls(t *testing.T) {
	image := dockerCacheTestImage(t)
	clientA, fixture, archive := seedDockerCacheFixture(t, image)
	clientB := &Client{cli: clientA.cli, repo: clientA.repo}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	fixture.mu.Lock()
	fixture.cacheSaveMode = "block-reader"
	fixture.cacheSaveEntered = entered
	fixture.cacheSaveRelease = release
	fixture.cacheSavedArchive = archive
	fixture.mu.Unlock()

	ownerCtx, cancelOwner := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelOwner()
	ownerDone := make(chan struct {
		handle string
		err    error
	}, 1)
	go func() {
		handle, err := clientA.Materialize(ownerCtx, image)
		ownerDone <- struct {
			handle string
			err    error
		}{handle, err}
	}()
	ownerFinished := false
	defer func() {
		unblock()
		if !ownerFinished {
			select {
			case result := <-ownerDone:
				if result.err != nil {
					t.Errorf("same-cache owner cleanup: %v", result.err)
				}
			case <-time.After(5 * time.Second):
				t.Error("same-cache owner did not exit during cleanup")
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("same-cache owner did not reach the deterministic blocked export")
	}
	fixture.mu.Lock()
	fixture.cacheSaveMode = ""
	fixture.mu.Unlock()

	fixture.mu.Lock()
	requestsBeforeWaiter := len(fixture.requests)
	fixture.mu.Unlock()
	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWaiter()
	waiterDone := make(chan struct {
		handle string
		err    error
	}, 1)
	go func() {
		handle, err := clientB.Materialize(waiterCtx, image)
		waiterDone <- struct {
			handle string
			err    error
		}{handle, err}
	}()
	waitCacheLockRefs(t, &materializeLocks, "localhost/opensbx-cache:"+strings.TrimPrefix(image.ManifestDigest, "sha256:"), 2)
	fixture.mu.Lock()
	requestsWhileOwnerBlocked := len(fixture.requests)
	fixture.mu.Unlock()
	if requestsWhileOwnerBlocked != requestsBeforeWaiter {
		t.Fatalf("same-key client waiter issued native calls before owner release: before=%d while-blocked=%d", requestsBeforeWaiter, requestsWhileOwnerBlocked)
	}

	unblock()
	owner := <-ownerDone
	ownerFinished = true
	waiter := <-waiterDone
	if owner.err != nil || waiter.err != nil {
		t.Fatalf("same-key materializations owner=%q/%v waiter=%q/%v", owner.handle, owner.err, waiter.handle, waiter.err)
	}
	if owner.handle != fixture.cacheNativeID || waiter.handle != owner.handle {
		t.Fatalf("same-key materializations returned different handles: owner=%q waiter=%q want=%q", owner.handle, waiter.handle, fixture.cacheNativeID)
	}
	fixture.mu.Lock()
	loads := 0
	for _, request := range fixture.requests {
		if request == "POST /images/load" {
			loads++
		}
	}
	fixture.mu.Unlock()
	if loads != 1 {
		t.Fatalf("same-key cache hits unexpectedly reimported image: image-load count=%d want only the initial seed import", loads)
	}
}

func TestCacheLocksRejectsPrecanceledContextWithoutRegisteringReference(t *testing.T) {
	var locks cacheLocks
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := locks.acquire(ctx, "cache:precanceled")
	if release != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("precanceled acquire returned release=%t err=%v; want nil and context.Canceled", release != nil, err)
	}
	assertCacheLockEntries(t, &locks, 0)
}

func TestCacheLocksDeadlineWhileWaitingReturnsOriginalContextError(t *testing.T) {
	var locks cacheLocks
	ownerRelease, err := locks.acquire(context.Background(), "cache:deadline")
	if err != nil {
		t.Fatal(err)
	}
	ownerReleased := false
	defer func() {
		if !ownerReleased {
			ownerRelease()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	type result struct {
		release func()
		err     error
	}
	done := make(chan result, 1)
	go func() {
		release, err := locks.acquire(ctx, "cache:deadline")
		done <- result{release, err}
	}()
	waitCacheLockRefs(t, &locks, "cache:deadline", 2)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("waiter context deadline did not fire")
	}
	select {
	case got := <-done:
		if got.release != nil || !errors.Is(got.err, context.DeadlineExceeded) || !errors.Is(got.err, ctx.Err()) {
			t.Fatalf("deadline waiter returned release=%t err=%v; want original deadline error", got.release != nil, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline waiter did not promptly leave the cache lock")
	}
	assertCacheLockRefs(t, &locks, "cache:deadline", 1)
	ownerRelease()
	ownerReleased = true
	assertCacheLockEntries(t, &locks, 0)
}

func TestCacheLocksSerializeSameKeyAndAllowDifferentKeysInParallel(t *testing.T) {
	var locks cacheLocks
	ownerRelease, err := locks.acquire(context.Background(), "cache:same")
	if err != nil {
		t.Fatal(err)
	}
	ownerReleased := false
	defer func() {
		if !ownerReleased {
			ownerRelease()
		}
	}()
	otherRelease, err := locks.acquire(context.Background(), "cache:other")
	if err != nil {
		t.Fatalf("independent cache key blocked behind owner: %v", err)
	}
	otherRelease()

	type result struct {
		release func()
		err     error
	}
	sameDone := make(chan result, 1)
	go func() {
		release, err := locks.acquire(context.Background(), "cache:same")
		sameDone <- result{release, err}
	}()
	waitCacheLockRefs(t, &locks, "cache:same", 2)
	select {
	case got := <-sameDone:
		if got.release != nil {
			got.release()
		}
		t.Fatal("same-key waiter acquired while its owner still held the reference")
	default:
	}
	ownerRelease()
	ownerReleased = true
	select {
	case got := <-sameDone:
		if got.err != nil || got.release == nil {
			t.Fatalf("same-key waiter after owner release: release=%t err=%v", got.release != nil, got.err)
		}
		got.release()
	case <-time.After(time.Second):
		t.Fatal("same-key waiter did not acquire after owner release")
	}
	assertCacheLockEntries(t, &locks, 0)
}

func TestCacheLocksCancellationReleaseRaceCleansAllEntries(t *testing.T) {
	var locks cacheLocks
	for iteration := 0; iteration < 100; iteration++ {
		t.Run(fmt.Sprintf("%d", iteration), func(t *testing.T) {
			key := fmt.Sprintf("cache:race-%d", iteration)
			ownerRelease, err := locks.acquire(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			releaseOwner := func() { releaseOnce.Do(ownerRelease) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				release func()
				err     error
			}
			waiterDone := make(chan result, 1)
			go func() {
				release, err := locks.acquire(ctx, key)
				waiterDone <- result{release, err}
			}()
			waiterCollected := false
			defer func() {
				cancel()
				releaseOwner()
				if !waiterCollected {
					select {
					case got := <-waiterDone:
						if got.release != nil {
							got.release()
						}
					case <-time.After(time.Second):
						t.Error("race waiter did not exit during cleanup")
					}
				}
			}()
			waitCacheLockRefs(t, &locks, key, 2)
			startRace := make(chan struct{})
			ownerReleased := make(chan struct{})
			cancelDone := make(chan struct{})
			go func() { <-startRace; cancel(); close(cancelDone) }()
			go func() { <-startRace; releaseOwner(); close(ownerReleased) }()
			close(startRace)
			select {
			case <-cancelDone:
			case <-time.After(time.Second):
				t.Fatal("cancellation race participant did not finish")
			}
			select {
			case <-ownerReleased:
			case <-time.After(time.Second):
				t.Fatal("owner release race participant did not finish")
			}
			cancel()
			select {
			case got := <-waiterDone:
				waiterCollected = true
				if got.err != nil {
					if got.release != nil || !errors.Is(got.err, context.Canceled) {
						t.Fatalf("race iteration %d returned release=%t err=%v", iteration, got.release != nil, got.err)
					}
				} else if got.release == nil {
					t.Fatalf("race iteration %d succeeded without a release function", iteration)
				} else {
					got.release()
				}
			case <-time.After(time.Second):
				t.Fatalf("race iteration %d waiter did not exit", iteration)
			}
			assertCacheLockEntries(t, &locks, 0)
		})
	}
}

func waitCacheLockRefs(t *testing.T, locks *cacheLocks, ref string, want int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		locks.mu.Lock()
		entry := locks.entries[ref]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		locks.mu.Unlock()
		if refs == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("cache lock %q refs=%d want %d", ref, refs, want)
		default:
			runtime.Gosched()
		}
	}
}

func assertCacheLockRefs(t *testing.T, locks *cacheLocks, ref string, want int) {
	t.Helper()
	locks.mu.Lock()
	defer locks.mu.Unlock()
	entry := locks.entries[ref]
	got := 0
	if entry != nil {
		got = entry.refs
	}
	if got != want {
		t.Fatalf("cache lock %q refs=%d want %d", ref, got, want)
	}
}

func assertCacheLockEntries(t *testing.T, locks *cacheLocks, want int) {
	t.Helper()
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.entries) != want {
		t.Fatalf("cache lock map entries=%d want %d: %#v", len(locks.entries), want, locks.entries)
	}
}

func dockerCacheTestImage(t *testing.T) sandbox.Image {
	t.Helper()
	archive, platform := testsupport.OCIArchive(t)
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/cache-verification:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(context.Background(), ref, sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func seedDockerCacheFixture(t *testing.T, image sandbox.Image) (*Client, *dockerAPIFixture, []byte) {
	t.Helper()
	client, fixture := newDockerFixture(t)
	configureDockerCacheFixture(fixture, image)
	if _, err := client.Materialize(context.Background(), image); err != nil {
		t.Fatalf("seed valid verified Docker cache: %v", err)
	}
	fixture.mu.Lock()
	archive := append([]byte(nil), fixture.loadedArchive...)
	fixture.mu.Unlock()
	if len(archive) == 0 {
		t.Fatal("Docker fixture did not retain its imported archive")
	}
	return client, fixture, archive
}

func dockerArchiveForImage(t *testing.T, image sandbox.Image) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.tar")
	tag, err := name.NewTag("localhost/cache-fixture:verification")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tarball.Write(tag, image.Content, f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func mutateDockerLayerBytes(t *testing.T, archive []byte) []byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(archive))
	var entries []struct {
		header *tar.Header
		body   []byte
	}
	var layerPaths []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, struct {
			header *tar.Header
			body   []byte
		}{header: header, body: body})
		if header.Name == "manifest.json" {
			var manifest []struct {
				Layers []string `json:"Layers"`
			}
			if err := json.Unmarshal(body, &manifest); err != nil || len(manifest) != 1 || len(manifest[0].Layers) == 0 {
				t.Fatalf("read Docker archive layer paths: manifest=%+v err=%v", manifest, err)
			}
			layerPaths = manifest[0].Layers
		}
	}
	if len(layerPaths) == 0 {
		t.Fatal("Docker archive has no layer entries")
	}
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	mutated := false
	for _, entry := range entries {
		body := append([]byte(nil), entry.body...)
		if entry.header.Name == layerPaths[0] {
			compressed, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			layerBytes, err := io.ReadAll(compressed)
			_ = compressed.Close()
			if err != nil || len(layerBytes) == 0 {
				t.Fatalf("read first compressed Docker layer: len=%d err=%v", len(layerBytes), err)
			}
			layerBytes[len(layerBytes)/2] ^= 0xff
			var recompressed bytes.Buffer
			writer := gzip.NewWriter(&recompressed)
			if _, err := writer.Write(layerBytes); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			body = recompressed.Bytes()
			mutated = true
		}
		header := *entry.header
		header.Size = int64(len(body))
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Fatal("did not find a Docker layer to mutate")
	}
	return out.Bytes()
}

type dockerArchiveEntry struct {
	header tar.Header
	body   []byte
}

func dockerArchiveEntries(t *testing.T, archive []byte) []dockerArchiveEntry {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(archive))
	var entries []dockerArchiveEntry
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, dockerArchiveEntry{header: *header, body: body})
	}
}

func writeDockerArchive(t *testing.T, entries []dockerArchiveEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, entry := range entries {
		header := entry.header
		header.Size = int64(len(entry.body))
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func dockerArchiveManifest(t *testing.T, archive []byte) []struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
} {
	t.Helper()
	for _, entry := range dockerArchiveEntries(t, archive) {
		if entry.header.Name == "manifest.json" {
			var manifest []struct {
				Config   string   `json:"Config"`
				RepoTags []string `json:"RepoTags"`
				Layers   []string `json:"Layers"`
			}
			if err := json.Unmarshal(entry.body, &manifest); err != nil {
				t.Fatal(err)
			}
			return manifest
		}
	}
	t.Fatal("Docker archive has no manifest.json")
	return nil
}

func mutateDockerArchiveLayers(t *testing.T, archive []byte, layers []string) []byte {
	t.Helper()
	entries := dockerArchiveEntries(t, archive)
	for i := range entries {
		if entries[i].header.Name != "manifest.json" {
			continue
		}
		manifest := dockerArchiveManifest(t, archive)
		if len(manifest) != 1 {
			t.Fatalf("want one archive image manifest, got %d", len(manifest))
		}
		manifest[0].Layers = append([]string(nil), layers...)
		entries[i].body, _ = json.Marshal(manifest)
		return writeDockerArchive(t, entries)
	}
	t.Fatal("Docker archive has no manifest.json to mutate")
	return nil
}

func duplicateDockerLayerEntry(t *testing.T, archive []byte, layerPath string) []byte {
	t.Helper()
	entries := dockerArchiveEntries(t, archive)
	for _, entry := range entries {
		if entry.header.Name == layerPath {
			entries = append(entries, entry)
			return writeDockerArchive(t, entries)
		}
	}
	t.Fatalf("Docker archive has no layer entry %q", layerPath)
	return nil
}

func changeDockerLayerToDirectory(t *testing.T, archive []byte, layerPath string) []byte {
	t.Helper()
	entries := dockerArchiveEntries(t, archive)
	for i := range entries {
		if entries[i].header.Name == layerPath {
			entries[i].header.Typeflag = tar.TypeDir
			entries[i].header.Size = 0
			entries[i].body = nil
			return writeDockerArchive(t, entries)
		}
	}
	t.Fatalf("Docker archive has no layer entry %q", layerPath)
	return nil
}

type imageConfigError struct {
	v1.Image
	err error
}

func (i imageConfigError) ConfigName() (v1.Hash, error) { return v1.Hash{}, i.err }

func TestDockerMaterializeStopsBeforeLoadForInvalidPinnedTagOrUnreadableOCIConfig(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/materialize-boundaries:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(context.Background(), ref, sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*sandbox.Image)
		want string
	}{
		{name: "invalid content pin", edit: func(image *sandbox.Image) { image.ManifestDigest = "not a valid digest" }, want: "tag can only contain"},
		{name: "unreadable OCI config", edit: func(image *sandbox.Image) {
			image.Content = imageConfigError{Image: image.Content, err: errors.New("synthetic config read failure")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fixture := newDockerFixture(t)
			candidate := image
			tc.edit(&candidate)
			_, err := client.Materialize(context.Background(), candidate)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Materialize() error=%v want substring %q", err, tc.want)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			for _, request := range fixture.requests {
				if request == "POST /images/load" {
					t.Fatalf("invalid local content reached Docker image load: %v", fixture.requests)
				}
			}
		})
	}
}
