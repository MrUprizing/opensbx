package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

func cliArgs(command, dataDir string, platform v1.Platform, rest ...string) []string {
	return append([]string{command, "--data-dir", dataDir, "--platform", platform.String()}, rest...)
}

func TestImageCLIHelpAndOfflineArchiveLifecycleWithoutRuntime(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	dataDir := t.TempDir()
	ref := "example.test/team/app:latest"
	var output bytes.Buffer
	if err := CLI(ctx, []string{"help"}, dataDir, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "no runtime is contacted") {
		t.Fatalf("image CLI help omitted runtime-independence contract: %q", output.String())
	}
	output.Reset()
	if err := CLI(ctx, cliArgs("import", dataDir, platform, "--reference", ref, archive), dataDir, &output); err != nil {
		t.Fatalf("image import: %v", err)
	}
	output.Reset()
	if err := CLI(ctx, cliArgs("list", dataDir, platform), dataDir, &output); err != nil {
		t.Fatalf("image list: %v", err)
	}
	var listed []sandbox.ImageSummary
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil || len(listed) != 1 || len(listed[0].Tags) != 1 || listed[0].Tags[0] != ref {
		t.Fatalf("image list=%+v decode=%v", listed, err)
	}
	output.Reset()
	if err := CLI(ctx, cliArgs("inspect", dataDir, platform, ref), dataDir, &output); err != nil {
		t.Fatalf("image inspect: %v", err)
	}
	var inspected sandbox.ImageDetail
	if err := json.Unmarshal(output.Bytes(), &inspected); err != nil || inspected.ID != listed[0].ID || inspected.OS != platform.OS || inspected.Architecture != platform.Architecture {
		t.Fatalf("image inspect=%+v decode=%v", inspected, err)
	}
	exported := filepath.Join(t.TempDir(), "roundtrip.oci.tar")
	output.Reset()
	if err := CLI(ctx, cliArgs("export", dataDir, platform, "--output", exported, ref), dataDir, &output); err != nil {
		t.Fatalf("image export: %v", err)
	}
	secondData := t.TempDir()
	if err := CLI(ctx, cliArgs("import", secondData, platform, "--reference", "example.test/team/app:copy", exported), dataDir, &output); err != nil {
		t.Fatalf("reimport exported OCI archive: %v", err)
	}
	if err := CLI(ctx, cliArgs("remove", dataDir, platform, ref), dataDir, &output); err != nil {
		t.Fatalf("image remove: %v", err)
	}
	output.Reset()
	if err := CLI(ctx, cliArgs("list", dataDir, platform), dataDir, &output); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != "[]" {
		t.Fatalf("images remained after remove: %s", output.String())
	}
}

func TestImageCLIRejectsInvalidCommandArgumentsAndPlatforms(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"unknown", "image"}, {"inspect", "--platform", "linux", "image"}, {"inspect", "--platform", "linux/amd64", "one", "two"},
		{"list", "extra"}, {"export", "--platform", "linux/amd64", "image"}, {"import", "--platform", "linux/amd64", "archive"},
		{"pull", "--platform", "linux/amd64", "not a valid reference"},
	} {
		var out bytes.Buffer
		if err := CLI(context.Background(), args, t.TempDir(), &out); err == nil {
			t.Errorf("CLI(%q) unexpectedly succeeded", args)
		}
	}
}

func TestImageCLINoArgumentsShowsHelpAndInvalidInspectionStaysOffline(t *testing.T) {
	var out bytes.Buffer
	if err := CLI(context.Background(), nil, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Usage: opensbx image") {
		t.Fatalf("empty CLI args did not display help: %q", out.String())
	}
	storeDir := t.TempDir()
	out.Reset()
	err := CLI(context.Background(), []string{"inspect", "--data-dir", storeDir, "--platform", "linux/amd64", "example.test/missing:tag"}, storeDir, &out)
	if err == nil || !strings.Contains(err.Error(), "image not found locally") {
		t.Fatalf("offline missing image inspect err=%v", err)
	}
}

type failingCLIWriter struct{ err error }

func (w failingCLIWriter) Write([]byte) (int, error) { return 0, w.err }

func TestImageCLIPropagatesHelpAndFlagOutputFailures(t *testing.T) {
	want := errors.New("synthetic terminal write failure")
	if err := CLI(context.Background(), nil, t.TempDir(), failingCLIWriter{err: want}); !errors.Is(err, want) {
		t.Fatalf("help output error=%v, want wrapped writer failure", err)
	}
	var output bytes.Buffer
	err := CLI(context.Background(), []string{"list", "--unknown-option"}, t.TempDir(), &output)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unknown option error=%v", err)
	}
}

func TestImageCLIPropagatesListEncodingAndDataDirectoryErrors(t *testing.T) {
	want := errors.New("synthetic terminal write failure")
	if err := CLI(context.Background(), []string{"list"}, t.TempDir(), failingCLIWriter{err: want}); !errors.Is(err, want) {
		t.Fatalf("list encoding error=%v, want writer failure", err)
	}
	archive, platform := testsupport.OCIArchive(t)
	imageDir := t.TempDir()
	store, err := Open(imageDir)
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/inspect-write-failure:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	args := cliArgs("inspect", imageDir, platform, ref)
	if err := CLI(context.Background(), args, imageDir, failingCLIWriter{err: want}); !errors.Is(err, want) {
		t.Fatalf("inspect encoding error=%v, want writer failure", err)
	}
	dataPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataPath, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CLI(context.Background(), []string{"list", "--data-dir", dataPath}, dataPath, &bytes.Buffer{}); err == nil {
		t.Fatal("CLI accepted a regular file as its data directory")
	}
}

func TestImageCLIPullsOnlyRequestedLocalRegistryPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	registryServer := httptest.NewServer(registry.New())
	t.Cleanup(registryServer.Close)
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/cli-multiarch:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := registryIndex(t, 101)
	if err := remote.WriteIndex(parsed, index, remote.WithTransport(registryServer.Client().Transport)); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	var output bytes.Buffer
	if err := CLI(context.Background(), cliArgs("pull", dataDir, platform, ref), dataDir, &output); err != nil {
		t.Fatalf("CLI pull of explicit local OCI variant: %v", err)
	}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), ref, platform); err != nil {
		t.Fatalf("requested CLI platform was not prepared: %v", err)
	}
	if _, err := store.Resolve(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatal("CLI pull implicitly prepared unrequested amd64 variant")
	}
}
