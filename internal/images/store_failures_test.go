package images

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"opensbx/internal/testsupport"
)

func TestStoreSurfacesMissingArchiveDestinationAndBlobFilesystemErrors(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(ctx, filepath.Join(t.TempDir(), "missing.oci.tar"), "example.test/team/missing:latest", platform); err == nil {
		t.Fatal("missing archive path imported")
	}
	const ref = "example.test/team/export-parent:latest"
	if err := store.Import(ctx, archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "absent-parent", "image.tar")
	if err := store.Export(ctx, ref, destination, platform, false); err == nil {
		t.Fatal("export created a missing parent directory")
	}
	digest, _, err := v1.SHA256(strings.NewReader("small blob"))
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlob(t.TempDir(), v1.Descriptor{Digest: digest, Size: int64(len("small blob"))}, strings.NewReader("small blob")); err == nil {
		t.Fatal("blob publication succeeded without an OCI blob directory")
	}
}

func TestPublishCancellationAndBlobRenameFailureDoNotCommitCatalog(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stage, "blobs", "sha256"), 0700); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.publish(canceled, stage, map[string]Entry{}); err == nil {
		t.Fatal("canceled publication committed")
	}

	body := []byte("target collision")
	digest, _, err := v1.SHA256(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	filename := digest.Hex
	if err := os.Mkdir(filepath.Join(store.dir, "blobs", "sha256", filename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "blobs", "sha256", filename), body, 0600); err != nil {
		t.Fatal(err)
	}
	ref := "example.test/team/rename-failure:latest"
	entry := Entry{Root: v1.Descriptor{Digest: digest, Size: int64(len(body))}, Prepared: map[string]v1.Descriptor{}}
	if err := store.publish(context.Background(), stage, map[string]Entry{ref: entry}); err == nil {
		t.Fatal("publication overwrote a non-regular target blob")
	}
	if refs, err := store.List(context.Background()); err != nil || len(refs) != 0 {
		t.Fatalf("failed blob publication committed catalog entry: refs=%+v err=%v", refs, err)
	}
}

func TestResolveAndGraphSizeRejectMissingRootsAndCountSharedGraphOnce(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/missing-root:latest"
	if err := store.Import(ctx, archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(ctx, ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	first, err := graphSize(store.dir, artifact.Manifest, seen)
	if err != nil || first <= 0 {
		t.Fatalf("first graph size=%d err=%v", first, err)
	}
	second, err := graphSize(store.dir, artifact.Manifest, seen)
	if err != nil || second != 0 {
		t.Fatalf("shared graph re-counted size=%d err=%v", second, err)
	}
	rootPath, err := blobPath(store.dir, artifact.Root.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rootPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ctx, ref, platform); err == nil {
		t.Fatal("Resolve accepted a catalog root whose immutable blob is missing")
	}
}

func TestImageReferenceParserRejectsMalformedNames(t *testing.T) {
	for _, raw := range []string{"", "not a reference", "example.test/team/image:bad tag"} {
		if _, err := reference(raw); err == nil {
			t.Errorf("malformed image reference %q parsed successfully", raw)
		}
	}
}
