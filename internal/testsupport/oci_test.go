package testsupport

import (
	"archive/tar"
	"encoding/json"
	"io"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestOCIArchivePlatformKeepsIndexAndConfigVariantConsistent(t *testing.T) {
	want := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	archive, returned := OCIArchivePlatform(t, want)
	if !reflect.DeepEqual(returned, want) {
		t.Fatalf("returned platform=%+v want %+v", returned, want)
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	blobs := map[string][]byte{}
	var index v1.IndexManifest
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(header.Name, `\`) {
			t.Fatalf("OCI archive path %q contains a platform-specific separator", header.Name)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "index.json" {
			if err := json.Unmarshal(body, &index); err != nil {
				t.Fatal(err)
			}
		} else if len(header.Name) > len("blobs/sha256/") && header.Name[:len("blobs/sha256/")] == "blobs/sha256/" {
			blobs[header.Name] = body
		}
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Platform == nil || !reflect.DeepEqual(*index.Manifests[0].Platform, want) {
		t.Fatalf("archive index platform=%+v want %+v", index.Manifests, want)
	}
	manifestRaw := blobs[path.Join("blobs", "sha256", index.Manifests[0].Digest.Hex)]
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("decode fixture manifest: %v", err)
	}
	configRaw := blobs[path.Join("blobs", "sha256", manifest.Config.Digest.Hex)]
	var config v1.ConfigFile
	if err := json.Unmarshal(configRaw, &config); err != nil {
		t.Fatalf("decode fixture image config: %v", err)
	}
	if config.OS != want.OS || config.Architecture != want.Architecture || config.Variant != want.Variant {
		t.Fatalf("config platform=%s/%s/%s want %+v", config.OS, config.Architecture, config.Variant, want)
	}
}
