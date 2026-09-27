package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"opensbx/internal/testsupport"
)

func TestVerificationCacheCancellationAndBoundedEviction(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive, platform := testsupport.OCIArchive(t)
	const ref = "example.test/team/verification-cache:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(context.Background(), ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.verified(context.Background(), v1.Descriptor{MediaType: types.OCIContentDescriptor}); err == nil {
		t.Fatal("non-image descriptor passed image graph verification")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.verified(canceled, artifact.Manifest); !errors.Is(err, context.Canceled) {
		t.Fatalf("warm cache verification ignored cancellation: %v", err)
	}
	key := fmt.Sprintf("%s/%d/%s", artifact.Manifest.Digest, artifact.Manifest.Size, artifact.Manifest.MediaType)
	store.verifiedCache.mu.Lock()
	store.verifiedCache.entries = make(map[string]verifiedGraph, 64)
	for i := 0; i < 64; i++ {
		store.verifiedCache.entries[fmt.Sprintf("padding-%d", i)] = verifiedGraph{}
	}
	store.verifiedCache.mu.Unlock()
	if _, err := store.verified(context.Background(), artifact.Manifest); err != nil {
		t.Fatalf("verify image while cache is saturated: %v", err)
	}
	store.verifiedCache.mu.Lock()
	defer store.verifiedCache.mu.Unlock()
	if len(store.verifiedCache.entries) != 1 {
		t.Fatalf("verification cache retained stale padding entries after eviction: %d entries", len(store.verifiedCache.entries))
	}
	if _, ok := store.verifiedCache.entries[key]; !ok {
		t.Fatalf("verified graph was not reinserted after cache eviction: keys=%v", store.verifiedCache.entries)
	}
}

func TestVerificationCacheInvalidatesSymlinkSubstitutedBlob(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/symlink-cache:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(context.Background(), ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := artifact.Image.Manifest()
	if err != nil || len(manifest.Layers) == 0 {
		t.Fatalf("fixture manifest layers=%+v err=%v", manifest, err)
	}
	layerPath, err := blobPath(store.dir, manifest.Layers[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "external-layer")
	if err := os.WriteFile(target, []byte("not the verified OCI layer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(layerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, layerPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), ref, platform); err == nil {
		t.Fatal("warm verification cache accepted a symlink substituted for a blob")
	}
}
