package images

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

func TestResolvedImageArchiveRemainsPinnedAfterLastTagRemovalOrMutableTagRepull(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, tc := range []struct {
		name    string
		replace bool
	}{
		{name: "last alias removed"},
		{name: "mutable tag repointed", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalArchive, originalPlatform := testsupport.OCIArchive(t)
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const ref = "example.test/team/pinned:latest"
			if err := store.Import(ctx, originalArchive, ref, originalPlatform); err != nil {
				t.Fatal(err)
			}
			pinned, err := store.ResolveImage(ctx, ref, sandboxPlatform(platform))
			if err != nil {
				t.Fatal(err)
			}
			if tc.replace {
				replacementArchive, replacementPlatform := testsupport.OCIArchive(t)
				if err := store.Import(ctx, replacementArchive, ref, replacementPlatform); err != nil {
					t.Fatal(err)
				}
			} else if err := store.Remove(ctx, ref, false); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "pinned.oci.tar")
			cacheRef := "opensbx.invalid/cache@" + pinned.ManifestDigest
			if err := pinned.Archive(destination, cacheRef); err != nil {
				t.Fatalf("resolved immutable artifact lost its archive after catalog change: %v", err)
			}
			if err := pinned.Archive(filepath.Join(t.TempDir(), "invalid-ref.tar"), "not a digest/tag reference"); err == nil {
				t.Fatal("archive accepted invalid cache reference")
			}
			if err := pinned.Archive(destination, cacheRef); err == nil {
				t.Fatal("archive overwrote an existing export target")
			}
			check, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := check.Import(ctx, destination, cacheRef, platform); err != nil {
				t.Fatalf("validate archive pinned to original resolution: %v", err)
			}
			result, err := check.Resolve(ctx, cacheRef, platform)
			if err != nil || result.Manifest.Digest.String() != pinned.ManifestDigest {
				t.Fatalf("exported resolved manifest=%v want %s err=%v", result.Manifest.Digest, pinned.ManifestDigest, err)
			}
		})
	}
}

func sandboxPlatform(platform v1.Platform) sandbox.Platform {
	return sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant}
}
