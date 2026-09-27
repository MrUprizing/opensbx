package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"opensbx/internal/images"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

func TestDockerMaterializeLoadsSelectedManifestAndReusesVerifiedConfigDigest(t *testing.T) {
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
	fixture.cacheDigest = image.ConfigDigest
	materialized, err := dc.Materialize(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	if materialized != image.ConfigDigest {
		t.Fatalf("Docker native handle=%q want config digest %q (not OCI root/manifest)", materialized, image.ConfigDigest)
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
	loadCount, inspectCount := 0, 0
	for _, req := range requests {
		if req == "POST /images/load" {
			loadCount++
		}
		if req == "GET /images/"+image.ConfigDigest+"/json" {
			inspectCount++
		}
	}
	if loadCount != 1 || inspectCount != 2 {
		t.Fatalf("cache miss/load/verify requests=%v, want one load and two digest inspections", requests)
	}
	if got, err := dc.Materialize(ctx, image); err != nil || got != image.ConfigDigest {
		t.Fatalf("cache hit handle=%q err=%v", got, err)
	}
	fixture.mu.Lock()
	loadCount = 0
	for _, req := range fixture.requests {
		if req == "POST /images/load" {
			loadCount++
		}
	}
	fixture.mu.Unlock()
	if loadCount != 1 {
		t.Fatalf("verified cache hit triggered another import: requests=%v", fixture.requests)
	}
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
			err := ValidateEndpoint(tc.endpoint)
			if (err == nil) != tc.valid {
				t.Fatalf("ValidateEndpoint(%q) err=%v want valid=%v", tc.endpoint, err, tc.valid)
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
	if caps.Runtime != "docker" || caps.Version != "27.1.2/docker-archive-v1" || caps.Platform.OS != "linux" || caps.Platform.Architecture != "arm64" || !caps.Pause || !caps.FractionalCPU {
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
		{name: "post-load identity mismatch", inspectID: "sha256:wrong-config", want: "config digest mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fixture := newDockerFixture(t)
			fixture.cacheDigest = image.ConfigDigest
			fixture.cacheLoadBody = tc.loadBody
			fixture.cacheInspectID = tc.inspectID
			if tc.loadStatus != 0 {
				fixture.fail["POST /images/load"] = tc.loadStatus
			}
			if len(tc.inspectCodes) != 0 {
				fixture.responses = make(map[string][]int)
				fixture.responses["GET /images/"+image.ConfigDigest+"/json"] = append([]int(nil), tc.inspectCodes...)
			}
			if _, err := client.Materialize(context.Background(), image); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Materialize() error=%v want substring %q", err, tc.want)
			}
		})
	}
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
