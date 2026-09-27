package images

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"opensbx/internal/testsupport"
)

func TestLocalOCIContentAndLayerExposeVerifiedRawAndCompressedBytes(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := "example.test/team/content:latest"
	if err := store.Import(ctx, archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(ctx, ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	graphSize, err := validateGraph(store.dir, artifact.Manifest, 0, map[string]bool{})
	if err != nil || graphSize <= 0 {
		t.Fatalf("whole graph size=%d err=%v", graphSize, err)
	}
	if err := validateLayers(store.dir, artifact.Manifest); err != nil {
		t.Fatalf("selected layer validation: %v", err)
	}
	image, err := contentAt(store.dir, artifact.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Digest()
	if err != nil || digest != artifact.Manifest.Digest {
		t.Fatalf("local image digest=%s err=%v", digest, err)
	}
	mediaType, err := image.MediaType()
	if err != nil || mediaType != artifact.Manifest.MediaType {
		t.Fatalf("local image media=%s err=%v", mediaType, err)
	}
	size, err := image.Size()
	if err != nil || size != artifact.Manifest.Size {
		t.Fatalf("local image size=%d want %d err=%v", size, artifact.Manifest.Size, err)
	}
	manifest, err := image.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	rawManifest, err := image.RawManifest()
	if err != nil || len(rawManifest) == 0 {
		t.Fatalf("raw manifest len=%d err=%v", len(rawManifest), err)
	}
	rawConfig, err := image.RawConfigFile()
	if err != nil || len(rawConfig) == 0 {
		t.Fatalf("raw config len=%d err=%v", len(rawConfig), err)
	}
	config, err := image.ConfigFile()
	if err != nil || config.Architecture != platform.Architecture || config.OS != platform.OS {
		t.Fatalf("local config=%+v err=%v", config, err)
	}
	layers, err := image.Layers()
	if err != nil || len(layers) != len(manifest.Layers) || len(layers) == 0 {
		t.Fatalf("local layers=%d manifest=%d err=%v", len(layers), len(manifest.Layers), err)
	}
	layer := layers[0]
	layerDigest, err := layer.Digest()
	if err != nil || layerDigest != manifest.Layers[0].Digest {
		t.Fatalf("layer digest=%s err=%v", layerDigest, err)
	}
	layerSize, err := layer.Size()
	if err != nil || layerSize != manifest.Layers[0].Size {
		t.Fatalf("layer size=%d err=%v", layerSize, err)
	}
	layerMedia, err := layer.MediaType()
	if err != nil || layerMedia != manifest.Layers[0].MediaType {
		t.Fatalf("layer media=%s err=%v", layerMedia, err)
	}
	compressed, err := layer.Compressed()
	if err != nil {
		t.Fatal(err)
	}
	compressedBytes, err := io.ReadAll(compressed)
	_ = compressed.Close()
	if err != nil || int64(len(compressedBytes)) != layerSize {
		t.Fatalf("compressed layer len=%d err=%v", len(compressedBytes), err)
	}
	if _, err := image.LayerByDigest(v1.Hash{Algorithm: "sha256", Hex: "deadbeef"}); err == nil {
		t.Fatal("unknown layer digest resolved")
	}
	layerPath, err := blobPath(store.dir, manifest.Layers[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(layerPath); err != nil {
		t.Fatal(err)
	}
	if _, err := layer.Compressed(); err == nil {
		t.Fatal("missing local layer opened successfully")
	}
	if _, err := contentAt(store.dir, v1.Descriptor{Digest: v1.Hash{Algorithm: "sha256", Hex: "deadbeef"}, Size: 1, MediaType: artifact.Manifest.MediaType}); err == nil {
		t.Fatal("missing manifest blob opened as local image")
	}
}

func TestLocalContentRejectsMalformedManifestAndMissingConfigBlob(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		manifest []byte
	}{
		{name: "malformed manifest", manifest: []byte("{")},
		{name: "missing config blob", manifest: func() []byte {
			body, err := json.Marshal(v1.Manifest{SchemaVersion: 2, Config: v1.Descriptor{Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}, Size: 1}})
			if err != nil {
				t.Fatal(err)
			}
			return body
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digest, size, err := v1.SHA256(bytes.NewReader(tc.manifest))
			if err != nil {
				t.Fatal(err)
			}
			descriptor := v1.Descriptor{Digest: digest, Size: size, MediaType: types.OCIManifestSchema1}
			if err := putBlob(dir, descriptor, bytes.NewReader(tc.manifest)); err != nil {
				t.Fatal(err)
			}
			if _, err := contentAt(dir, descriptor); err == nil {
				t.Fatal("invalid local content graph was accepted")
			}
		})
	}
}

func TestLocalCompressedLayerRejectsUnsupportedDigestAlgorithm(t *testing.T) {
	layer := localLayer{dir: t.TempDir(), d: v1.Descriptor{Digest: v1.Hash{Algorithm: "sha512", Hex: strings.Repeat("0", 128)}}}
	if _, err := layer.Compressed(); err == nil {
		t.Fatal("compressed local layer opened with unsupported digest algorithm")
	}
}
