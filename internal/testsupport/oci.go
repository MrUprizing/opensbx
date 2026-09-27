// Package testsupport contains hermetic fixtures shared by package tests.
package testsupport

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// OCIArchive returns a complete OCI archive and its image config platform.
func OCIArchive(t testing.TB) (string, v1.Platform) {
	return OCIArchivePlatform(t, v1.Platform{OS: "linux", Architecture: "amd64"})
}

// OCIArchivePlatform creates a fully valid one-variant index for platform.
func OCIArchivePlatform(t testing.TB, platform v1.Platform) (string, v1.Platform) {
	t.Helper()
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if platform.OS == "" || platform.Architecture == "" {
		t.Fatal("OCI fixture platform must include OS and architecture")
	}
	config.OS, config.Architecture, config.Variant = platform.OS, platform.Architecture, platform.Variant
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
	layoutDir := filepath.Join(t.TempDir(), "layout")
	if _, err := layout.Write(layoutDir, index); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "oci.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	err = filepath.Walk(layoutDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == layoutDir {
			return nil
		}
		name, err := filepath.Rel(layoutDir, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return writer.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0700})
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err = writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(body))}); err != nil {
			return err
		}
		_, err = writer.Write(body)
		return err
	})
	if err == nil {
		err = writer.Close()
	}
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return archive, platform
}
