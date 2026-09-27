package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

func writeLayoutArchive(t *testing.T, destination string, image v1.ImageIndex) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "layout")
	if _, err := layout.Write(dir, image); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	err = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		name, err := filepath.Rel(dir, path)
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
}

type archiveEntry struct {
	header tar.Header
	body   []byte
}

func readArchiveEntries(t *testing.T, path string) map[string]archiveEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries := map[string]archiveEntry{}
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		entries[h.Name] = archiveEntry{header: *h, body: body}
	}
	return entries
}
func writeArchiveEntries(t *testing.T, path string, entries map[string]archiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(f)
	for name, entry := range entries {
		h := entry.header
		h.Name = name
		h.Size = int64(len(entry.body))
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

type layerMediaOverride struct {
	v1.Layer
	mediaType types.MediaType
}

func (l layerMediaOverride) MediaType() (types.MediaType, error) { return l.mediaType, nil }

type packedLayer struct {
	compressed, plain []byte
	digest, diffID    v1.Hash
	mediaType         types.MediaType
}

func (l packedLayer) Digest() (v1.Hash, error) { return l.digest, nil }
func (l packedLayer) DiffID() (v1.Hash, error) { return l.diffID, nil }
func (l packedLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.compressed)), nil
}
func (l packedLayer) Uncompressed() (io.ReadCloser, error) {
	if l.mediaType == types.OCILayerZStd {
		decoder, err := zstd.NewReader(bytes.NewReader(l.compressed))
		if err != nil {
			return nil, err
		}
		return decoder.IOReadCloser(), nil
	}
	return io.NopCloser(bytes.NewReader(l.plain)), nil
}
func (l packedLayer) Size() (int64, error)                { return int64(len(l.compressed)), nil }
func (l packedLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }

func zstdFixtureLayer(t *testing.T) v1.Layer {
	t.Helper()
	var tarData bytes.Buffer
	writer := tar.NewWriter(&tarData)
	body := []byte("oci zstd fixture")
	if err := writer.WriteHeader(&tar.Header{Name: "fixture.txt", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(tarData.Bytes(), nil)
	encoder.Close()
	digest, _, err := v1.SHA256(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	diffID, _, err := v1.SHA256(bytes.NewReader(tarData.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return packedLayer{compressed: compressed, plain: tarData.Bytes(), digest: digest, diffID: diffID, mediaType: types.OCILayerZStd}
}

func rewriteArchiveDiffID(t *testing.T, source string) string {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	entries := map[string][]byte{}
	headers := map[string]tar.Header{}
	reader := tar.NewReader(input)
	for {
		h, err := reader.Next()
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
		entries[h.Name] = body
		headers[h.Name] = *h
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(entries["index.json"], &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("source index=%+v err=%v", index, err)
	}
	root := index.Manifests[0]
	var manifest v1.Manifest
	if err := json.Unmarshal(entries["blobs/sha256/"+root.Digest.Hex], &manifest); err != nil {
		t.Fatal(err)
	}
	var config v1.ConfigFile
	if err := json.Unmarshal(entries["blobs/sha256/"+manifest.Config.Digest.Hex], &config); err != nil {
		t.Fatal(err)
	}
	if len(config.RootFS.DiffIDs) == 0 {
		t.Fatal("fixture image has no layer diffIDs")
	}
	config.RootFS.DiffIDs[0] = v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}
	configRaw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configDigest, configSize, err := v1.SHA256(bytes.NewReader(configRaw))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Config.Digest, manifest.Config.Size = configDigest, configSize
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest, manifestSize, err := v1.SHA256(bytes.NewReader(manifestRaw))
	if err != nil {
		t.Fatal(err)
	}
	index.Manifests[0].Digest, index.Manifests[0].Size = manifestDigest, manifestSize
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	entries["blobs/sha256/"+configDigest.Hex] = configRaw
	entries["blobs/sha256/"+manifestDigest.Hex] = manifestRaw
	entries["index.json"] = indexRaw
	outputPath := filepath.Join(t.TempDir(), "bad-diff-id.tar")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(output)
	for name, body := range entries {
		h, exists := headers[name]
		if !exists {
			h = tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600}
		}
		h.Size = int64(len(body))
		if err := writer.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	return outputPath
}

func TestConcurrentImportsPreserveEveryTagForSharedContent(t *testing.T) {
	ctx := context.Background()
	image, err := random.Index(128, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := image.IndexManifest()
	if err != nil || len(manifest.Manifests) != 1 {
		t.Fatalf("fixture index=%+v err=%v", manifest, err)
	}
	selected, err := image.Image(manifest.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	config, err := selected.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: config.OS, Architecture: config.Architecture, Variant: config.Variant}
	archive := filepath.Join(t.TempDir(), "shared.tar")
	writeLayoutArchive(t, archive, image)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.Import(ctx, archive, fmt.Sprintf("example.test/team/app:tag-%d", i), platform)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent import failed: %v", err)
		}
	}
	items, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Tags) != workers {
		t.Fatalf("shared immutable image catalog=%+v, want one image and %d tags", items, workers)
	}
	if err := store.Remove(ctx, "example.test/team/app:tag-0", false); err != nil {
		t.Fatal(err)
	}
	items, err = store.List(ctx)
	if err != nil || len(items) != 1 || len(items[0].Tags) != workers-1 {
		t.Fatalf("remove one alias changed shared content unexpectedly: %+v err=%v", items, err)
	}
	if err := store.Remove(ctx, string(items[0].ID), false); err != nil {
		t.Fatal(err)
	}
	items, err = store.List(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("digest removal left aliases: %+v err=%v", items, err)
	}
}

func TestIndependentStoreHandlesSerializeCatalogUpdatesWithoutLostTags(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	dataDir := t.TempDir()
	first, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := first
			if i%2 != 0 {
				store = second
			}
			errs <- store.Import(ctx, archive, fmt.Sprintf("example.test/team/independent:tag-%d", i), platform)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent handle import: %v", err)
		}
	}
	items, err := first.List(ctx)
	if err != nil || len(items) != 1 || len(items[0].Tags) != workers {
		t.Fatalf("catalog lost refs across independent handles: items=%+v err=%v", items, err)
	}
}

func TestReadonlyListAndResolveDoNotRepublishUnchangedOCILayoutIndex(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/readonly:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(store.dir, "index.json")
	assertSameLayoutIndex := func(operation string, before os.FileInfo) os.FileInfo {
		t.Helper()
		after, err := os.Stat(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) {
			t.Fatalf("read-only %s atomically republished unchanged index.json", operation)
		}
		return after
	}
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	before = assertSameLayoutIndex("List", before)
	if _, err := store.Resolve(context.Background(), ref, platform); err != nil {
		t.Fatal(err)
	}
	assertSameLayoutIndex("Resolve", before)
}

func TestStoreImportProcessHelper(t *testing.T) {
	if os.Getenv("OPENSBX_IMAGE_STORE_PROCESS_HELPER") != "1" {
		return
	}
	store, err := Open(os.Getenv("OPENSBX_IMAGE_STORE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), os.Getenv("OPENSBX_IMAGE_STORE_ARCHIVE"), os.Getenv("OPENSBX_IMAGE_STORE_REF"), v1.Platform{OS: "linux", Architecture: "amd64"}); err != nil {
		t.Fatal(err)
	}
}

func TestSeparateProcessesDoNotLoseConcurrentCatalogReferences(t *testing.T) {
	archive, _ := testsupport.OCIArchive(t)
	dataDir := t.TempDir()
	const count = 2
	commands := make([]*exec.Cmd, count)
	outputs := make([]*bytes.Buffer, count)
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestStoreImportProcessHelper$")
		commands[i].Env = append(os.Environ(), "OPENSBX_IMAGE_STORE_PROCESS_HELPER=1", "OPENSBX_IMAGE_STORE_DIR="+dataDir, "OPENSBX_IMAGE_STORE_ARCHIVE="+archive, fmt.Sprintf("OPENSBX_IMAGE_STORE_REF=example.test/team/process:tag-%d", i))
		outputs[i] = &bytes.Buffer{}
		commands[i].Stdout, commands[i].Stderr = outputs[i], outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("store writer process %d failed: %v output=%s", i, err, outputs[i].String())
		}
	}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List(context.Background())
	if err != nil || len(items) != 1 || len(items[0].Tags) != count {
		t.Fatalf("cross-process commits lost references: items=%+v err=%v", items, err)
	}
}

func TestImportResolveExportRoundTripPreservesOCIManifestAndConfig(t *testing.T) {
	ctx := context.Background()
	image, err := random.Index(1024, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	index, err := image.IndexManifest()
	if err != nil || len(index.Manifests) != 1 {
		t.Fatalf("fixture index=%+v err=%v", index, err)
	}
	selected, err := image.Image(index.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	configFile, err := selected.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: configFile.OS, Architecture: configFile.Architecture, Variant: configFile.Variant}
	archive := filepath.Join(t.TempDir(), "image.tar")
	writeLayoutArchive(t, archive, image)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(ctx, archive, "example.test/team/app:stable", platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(ctx, "example.test/team/app:stable", platform)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest, err := selected.Digest()
	wantSelectedDigest, selectedErr := selected.Digest()
	if err != nil || selectedErr != nil || artifact.Root.Digest != manifestDigest || artifact.Manifest.Digest != wantSelectedDigest {
		t.Fatalf("root/selected identity root=%s wantRoot=%s selected=%s wantSelected=%s errs=%v/%v", artifact.Root.Digest, manifestDigest, artifact.Manifest.Digest, wantSelectedDigest, err, selectedErr)
	}
	configDigest, err := selected.ConfigName()
	gotConfig, gotConfigErr := artifact.Image.ConfigName()
	if err != nil || gotConfigErr != nil || artifact.Root.Digest != manifestDigest || gotConfig != configDigest {
		t.Fatalf("OCI identities root=%s manifest=%s config=%s source=%s errs=%v/%v", artifact.Root.Digest, artifact.Manifest.Digest, gotConfig, configDigest, err, gotConfigErr)
	}
	output := filepath.Join(t.TempDir(), "roundtrip.tar")
	if err := store.Export(ctx, "example.test/team/app:stable", output, platform, false); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Import(ctx, output, "example.test/team/app:roundtrip", platform); err != nil {
		t.Fatal(err)
	}
	got, err := second.Resolve(ctx, "example.test/team/app:roundtrip", platform)
	if err != nil || got.Manifest.Digest != artifact.Manifest.Digest {
		t.Fatalf("roundtrip manifest=%v, err=%v; want %s", got.Manifest.Digest, err, artifact.Manifest.Digest)
	}
}

func TestEmptyRootfsOCIArchiveImportsExportsAndResolves(t *testing.T) {
	image, err := random.Image(0, 0)
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
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
	source := filepath.Join(t.TempDir(), "empty-rootfs.tar")
	writeLayoutArchive(t, source, index)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), source, "example.test/team/empty:latest", platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(context.Background(), "example.test/team/empty:latest", platform)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := artifact.Image.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Layers) != 0 {
		t.Fatalf("empty rootfs has %d layers", len(manifest.Layers))
	}
	output := filepath.Join(t.TempDir(), "empty-export.tar")
	if err := store.Export(context.Background(), "example.test/team/empty:latest", output, platform, false); err != nil {
		t.Fatal(err)
	}
	copyStore, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := copyStore.Import(context.Background(), output, "example.test/team/empty:copy", platform); err != nil {
		t.Fatal(err)
	}
	copyArtifact, err := copyStore.Resolve(context.Background(), "example.test/team/empty:copy", platform)
	if err != nil || copyArtifact.Manifest.Digest != artifact.Manifest.Digest {
		t.Fatalf("empty OCI roundtrip artifact=%+v err=%v", copyArtifact, err)
	}
}

func TestOCIStoreRejectsDockerSaveTarAndAcceptsStrictOCIArchive(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = platform.OS, platform.Architecture
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag("example.test/team/docker-cache:latest")
	if err != nil {
		t.Fatal(err)
	}
	dockerTar := filepath.Join(t.TempDir(), "docker-save.tar")
	f, err := os.Create(dockerTar)
	if err != nil {
		t.Fatal(err)
	}
	if err := tarball.Write(tag, image, f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), dockerTar, "example.test/team/docker-cache:latest", platform); err == nil {
		t.Fatal("Docker-save archive was silently adopted as an OpenSBX OCI archive")
	}
	if list, err := store.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("rejected native archive created catalog entries: %+v err=%v", list, err)
	}
	ociTar, ociPlatform := testsupport.OCIArchive(t)
	if err := store.Import(context.Background(), ociTar, "example.test/team/oci:latest", ociPlatform); err != nil {
		t.Fatalf("valid OCI-layout archive import failed: %v", err)
	}
	if list, err := store.List(context.Background()); err != nil || len(list) != 1 {
		t.Fatalf("OCI manager catalog after strict import=%+v err=%v", list, err)
	}
}

func TestResolveImageProvidesVerifiedContentAndVariantArchive(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/app:latest", platform); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveImage(context.Background(), "example.test/team/app:latest", sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := resolved.Content.Digest()
	if err != nil || manifest.String() != resolved.ManifestDigest {
		t.Fatalf("cache content manifest=%s declared=%s err=%v", manifest, resolved.ManifestDigest, err)
	}
	config, err := resolved.Content.ConfigName()
	if err != nil || config.String() != resolved.ConfigDigest {
		t.Fatalf("cache config digest=%s declared=%s err=%v", config, resolved.ConfigDigest, err)
	}
	variantArchive := filepath.Join(t.TempDir(), "variant.tar")
	if err := resolved.Archive(variantArchive, "opensbx.invalid/cache@"+resolved.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	cacheStore, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cacheStore.Import(context.Background(), variantArchive, "opensbx.invalid/cache@"+resolved.ManifestDigest, platform); err != nil {
		t.Fatal(err)
	}
	variant, err := cacheStore.Resolve(context.Background(), "opensbx.invalid/cache@"+resolved.ManifestDigest, platform)
	if err != nil || variant.Manifest.Digest.String() != resolved.ManifestDigest {
		t.Fatalf("variant cache manifest=%v err=%v", variant.Manifest.Digest, err)
	}
}

func TestDescriptorAndCanonicalPlatformNormalizeKnownVariants(t *testing.T) {
	body := []byte("manifest")
	d := descriptor(body, types.OCIManifestSchema1)
	wantDigest, _, err := v1.SHA256(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if d.Digest != wantDigest || d.Size != int64(len(body)) || d.MediaType != types.OCIManifestSchema1 {
		t.Fatalf("descriptor=%+v", d)
	}
	for _, tc := range []struct{ in, want string }{
		{"arm64/v8", "arm64"}, {"amd64/v1", "amd64"}, {"arm/v7", "arm/v7"},
	} {
		parts := strings.Split(tc.in, "/")
		p := v1.Platform{OS: "linux", Architecture: parts[0]}
		if len(parts) > 1 {
			p.Variant = parts[1]
		}
		got := canonicalPlatform(p)
		actual := got.Architecture
		if got.Variant != "" {
			actual += "/" + got.Variant
		}
		if actual != tc.want {
			t.Errorf("canonicalPlatform(%q)=%q want %q", tc.in, actual, tc.want)
		}
	}
}

func TestImportRejectsUnsafeArchiveEntryWithoutPublishingReference(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unsafe.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(f)
	if err := w.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0600, Size: 0}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), path, "example.test/team/unsafe:latest", v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatal("unsafe entry unexpectedly imported")
	}
	items, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("failed import published catalog entries: %+v", items)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "escape")); !os.IsNotExist(err) {
		t.Fatalf("unsafe archive wrote outside staging, stat error=%v", err)
	}
}

func TestImportRejectsMissingGraphBlobAndDescriptorSizeMismatch(t *testing.T) {
	source, _ := testsupport.OCIArchive(t)
	for _, tc := range []struct {
		name   string
		mutate func(map[string]archiveEntry)
	}{
		{name: "missing layer or config blob", mutate: func(entries map[string]archiveEntry) {
			var index v1.IndexManifest
			if err := json.Unmarshal(entries["index.json"].body, &index); err != nil || len(index.Manifests) != 1 {
				t.Fatalf("source index=%+v err=%v", index, err)
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(entries["blobs/sha256/"+index.Manifests[0].Digest.Hex].body, &manifest); err != nil || len(manifest.Layers) == 0 {
				t.Fatalf("source manifest=%+v err=%v", manifest, err)
			}
			delete(entries, "blobs/sha256/"+manifest.Layers[0].Digest.Hex)
		}},
		{name: "root descriptor size", mutate: func(entries map[string]archiveEntry) {
			var index v1.IndexManifest
			if err := json.Unmarshal(entries["index.json"].body, &index); err != nil {
				t.Fatal(err)
			}
			index.Manifests[0].Size++
			body, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			entry := entries["index.json"]
			entry.body = body
			entries["index.json"] = entry
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := readArchiveEntries(t, source)
			tc.mutate(entries)
			bad := filepath.Join(t.TempDir(), "bad-graph.tar")
			writeArchiveEntries(t, bad, entries)
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Import(context.Background(), bad, "example.test/team/bad-graph:latest", v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
				t.Fatal("invalid OCI graph imported")
			}
			list, err := store.List(context.Background())
			if err != nil || len(list) != 0 {
				t.Fatalf("invalid graph created catalog references: %+v err=%v", list, err)
			}
		})
	}
}

func TestImportRejectsIndexGraphBeyondNestingLimit(t *testing.T) {
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
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
	for i := 0; i < 17; i++ {
		index = mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: index})
	}
	archive := filepath.Join(t.TempDir(), "deep-index.tar")
	writeLayoutArchive(t, archive, index)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = store.Import(context.Background(), archive, "example.test/team/deep:latest", platform)
	if err == nil || !strings.Contains(err.Error(), "OCI graph exceeds limits") {
		t.Fatalf("deep OCI graph error=%v", err)
	}
	list, err := store.List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("deep index was cataloged: list=%+v err=%v", list, err)
	}
}

func TestImportValidatesSingleRootReferencesAndDigestAliases(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	first, err := random.Image(32, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := random.Image(48, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []*v1.Image{&first, &second} {
		config, err := (*image).ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		config.OS, config.Architecture = platform.OS, platform.Architecture
		*image, err = mutate.ConfigFile(*image, config)
		if err != nil {
			t.Fatal(err)
		}
	}
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: first, Descriptor: v1.Descriptor{Platform: &platform}},
		mutate.IndexAddendum{Add: second, Descriptor: v1.Descriptor{Platform: &platform}},
	)
	archive := filepath.Join(t.TempDir(), "multi-root.tar")
	writeLayoutArchive(t, archive, index)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/multi:tag", platform); err == nil || !strings.Contains(err.Error(), "single archive root") {
		t.Fatalf("multi-root reference import error=%v", err)
	}
	if err := store.Import(context.Background(), archive, "", platform); err == nil || !strings.Contains(err.Error(), "needs a reference") {
		t.Fatalf("unannotated multi-root import error=%v", err)
	}
	items, err := store.List(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("rejected multi-root archive was cataloged: %+v err=%v", items, err)
	}
	single, _ := testsupport.OCIArchive(t)
	if err := store.Import(context.Background(), single, "example.test/team/single@sha256:"+strings.Repeat("0", 64), platform); err == nil || !strings.Contains(err.Error(), "digest reference does not match") {
		t.Fatalf("mismatched digest alias import error=%v", err)
	}
}

func TestOpenRejectsCorruptCatalogAndCanceledTransactionsDoNotCommit(t *testing.T) {
	dataDir := t.TempDir()
	imageDir := filepath.Join(dataDir, "images")
	if err := os.MkdirAll(imageDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "catalog.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataDir); err == nil {
		t.Fatal("corrupt catalog JSON opened successfully")
	}
	cleanDir := t.TempDir()
	store, err := Open(cleanDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err = store.transaction(ctx, func(*catalog) error { called = true; return nil }, true)
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled transaction err=%v callbackCalled=%v", err, called)
	}
}

func TestAtomicFileFailureDoesNotReplaceExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(target, []byte("replacement")); err == nil {
		t.Fatal("atomicFile replaced a directory")
	}
	if got, err := os.ReadFile(filepath.Join(target, "keep")); err != nil || string(got) != "unchanged" {
		t.Fatalf("existing state changed: %q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state" {
		t.Fatalf("failed atomic publication leaked staging files: %v err=%v", entries, err)
	}
}

func TestRemoveMissingReferenceRequiresForceAndForceIsIdempotent(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(context.Background(), "example.test/team/absent:latest", false); !errors.Is(err, sandbox.ErrImageNotFound) {
		t.Fatalf("non-forced missing image removal error=%v", err)
	}
	if err := store.Remove(context.Background(), "example.test/team/absent:latest", true); err != nil {
		t.Fatalf("forced missing image removal: %v", err)
	}
	if err := store.Remove(context.Background(), "example.test/team/absent:latest", true); err != nil {
		t.Fatalf("repeated forced removal: %v", err)
	}
}

func TestBlobReadAndWriteRejectUnreasonableDescriptorSizesBeforeIOLimitAllocation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := v1.SHA256(strings.NewReader("one byte"))
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlob(store.dir, v1.Descriptor{Digest: digest, Size: -1}, strings.NewReader("one byte")); err == nil {
		t.Fatal("negative blob size accepted")
	}
	if err := putBlob(store.dir, v1.Descriptor{Digest: digest, Size: MaxArchiveBytes + 1}, strings.NewReader("one byte")); err == nil {
		t.Fatal("oversized blob descriptor accepted")
	}
	if _, err := readBlob(store.dir, v1.Descriptor{Digest: digest, Size: maxMetadataBytes + 1}); err == nil {
		t.Fatal("oversized metadata descriptor opened")
	}
}

func TestGraphChildrenValidateIndexAndManifestSchemaBeforeTraversal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "layout")
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		body  []byte
		media types.MediaType
		want  string
	}{
		{name: "index schema", body: []byte(`{"schemaVersion":1,"manifests":[]}`), media: types.OCIImageIndex, want: "invalid OCI index schema"},
		{name: "malformed index JSON", body: []byte("{"), media: types.OCIImageIndex, want: "unexpected end"},
		{name: "manifest schema", body: []byte(`{"schemaVersion":1,"config":{},"layers":[]}`), media: types.OCIManifestSchema1, want: "invalid OCI manifest schema"},
		{name: "malformed manifest JSON", body: []byte("{"), media: types.OCIManifestSchema1, want: "unexpected end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := descriptor(tc.body, tc.media)
			path, err := blobPath(dir, d.Digest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := children(dir, d); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("children() error=%v want %q", err, tc.want)
			}
		})
	}
	if got, err := children(dir, v1.Descriptor{Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("a", 64)}, MediaType: types.MediaType("application/octet-stream")}); err != nil || len(got) != 0 {
		t.Fatalf("non-image artifact traversal=%v err=%v", got, err)
	}
}

func TestGraphValidatorRejectsUntrustedDescriptorsBeforeOpeningBlobs(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		desc v1.Descriptor
		want string
	}{
		{name: "missing media type", ctx: context.Background(), desc: v1.Descriptor{}, want: "media type is required"},
		{name: "external URL", ctx: context.Background(), desc: v1.Descriptor{MediaType: types.OCIManifestSchema1, URLs: []string{"http://127.0.0.1/private"}}, want: "external OCI descriptor URLs"},
		{name: "embedded metadata mismatch", ctx: context.Background(), desc: v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}, Size: 1, Data: []byte("x")}, want: "embedded OCI descriptor data mismatch"},
		{name: "canceled context", ctx: func() context.Context { c, cancel := context.WithCancel(context.Background()); cancel(); return c }(), desc: v1.Descriptor{MediaType: types.OCIManifestSchema1}, want: context.Canceled.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateGraphContext(tc.ctx, dir, tc.desc, 0, map[string]bool{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("descriptor validation error=%v want %q", err, tc.want)
			}
		})
	}
}

func TestGraphValidatorRejectsSymlinkBlobAndSelectManifestEnforcesDepth(t *testing.T) {
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte("regular blob contents")
	digest, size, err := v1.SHA256(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, body, 0600); err != nil {
		t.Fatal(err)
	}
	link, err := blobPath(dir, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := validateGraphContext(context.Background(), dir, v1.Descriptor{Digest: digest, Size: size, MediaType: types.OCIManifestSchema1}, 0, map[string]bool{}); err == nil {
		t.Fatal("symlink-backed OCI blob accepted")
	}
	if _, err := selectManifest(dir, v1.Descriptor{MediaType: types.OCIImageIndex}, v1.Platform{OS: "linux", Architecture: "amd64"}, 17); err == nil || !strings.Contains(err.Error(), "nesting limit") {
		t.Fatalf("deep selector error=%v", err)
	}
}

func TestImageInspectionAndCacheResolutionReturnNotFoundForUnknownReference(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	if _, err := store.Resolve(context.Background(), "example.test/team/missing:latest", platform); !errors.Is(err, sandbox.ErrImageNotFound) {
		t.Fatalf("Resolve() missing ref error=%v", err)
	}
	if _, err := store.Inspect(context.Background(), "example.test/team/missing:latest", platform); !errors.Is(err, sandbox.ErrImageNotFound) {
		t.Fatalf("Inspect() missing ref error=%v", err)
	}
	if _, err := store.ResolveImage(context.Background(), "example.test/team/missing:latest", sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture}); !errors.Is(err, sandbox.ErrImageNotFound) {
		t.Fatalf("ResolveImage() missing ref error=%v", err)
	}
}

func TestExportHonorsPreparedVariantAndNeverOverwritesDestination(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := "example.test/team/export:latest"
	if err := store.Import(ctx, archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	missingPlatform := v1.Platform{OS: "linux", Architecture: "arm64"}
	if err := store.Export(ctx, ref, filepath.Join(t.TempDir(), "missing.tar"), missingPlatform, false); err == nil || !strings.Contains(err.Error(), "platform is not prepared") {
		t.Fatalf("unprepared variant export error=%v", err)
	}
	destination := filepath.Join(t.TempDir(), "existing.tar")
	if err := os.WriteFile(destination, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Export(ctx, ref, destination, platform, false); err == nil {
		t.Fatal("Export replaced an existing destination")
	}
	if body, err := os.ReadFile(destination); err != nil || string(body) != "keep" {
		t.Fatalf("existing export target changed: %q err=%v", body, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Export(canceled, ref, filepath.Join(t.TempDir(), "canceled.tar"), platform, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export error=%v", err)
	}
}

func TestOpenRejectsInvalidExistingOCILayoutAndPreservesValidLayout(t *testing.T) {
	dataDir := t.TempDir()
	imageDir := filepath.Join(dataDir, "images")
	if err := os.MkdirAll(imageDir, 0700); err != nil {
		t.Fatal(err)
	}
	layoutPath := filepath.Join(imageDir, "oci-layout")
	if err := os.WriteFile(layoutPath, []byte(`{"imageLayoutVersion":"0.9.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataDir); err == nil || !strings.Contains(err.Error(), "invalid owned OCI layout version") {
		t.Fatalf("foreign layout version error=%v", err)
	}
	if err := os.WriteFile(layoutPath, []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataDir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("opening a valid existing OCI layout rewrote its identity file")
	}
}

func TestResolveRejectsCorruptManifestBlobAndInvalidDigestAlgorithm(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/corrupt:latest", platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(context.Background(), "example.test/team/corrupt:latest", platform)
	if err != nil {
		t.Fatal(err)
	}
	path, err := blobPath(store.dir, artifact.Manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob[0] ^= 0xff
	if err := os.WriteFile(path, blob, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), "example.test/team/corrupt:latest", platform); err == nil {
		t.Fatal("corrupt manifest blob resolved")
	}
	if _, err := blobPath(store.dir, v1.Hash{Algorithm: "sha512", Hex: strings.Repeat("0", 128)}); err == nil {
		t.Fatal("non-sha256 OCI blob digest accepted")
	}
	if _, err := blobPath(store.dir, v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("z", 64)}); err == nil {
		t.Fatal("non-hex OCI blob digest accepted")
	}
}

func TestVerifiedLayerReuseStillDetectsCorruptImmutableBlob(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/verified:latest"
	if err := store.Import(ctx, archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	first, err := store.Resolve(ctx, ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("%s/%d/%s", first.Manifest.Digest, first.Manifest.Size, first.Manifest.MediaType)
	if _, ok := store.verifiedCache.entries[key]; !ok {
		t.Fatal("successful selected graph was not cached after integrity verification")
	}
	if _, err := store.Resolve(ctx, ref, platform); err != nil {
		t.Fatalf("warm immutable image resolution: %v", err)
	}
	manifest, err := first.Image.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Layers) == 0 {
		t.Fatal("fixture has no rootfs layer")
	}
	layerPath, err := blobPath(store.dir, manifest.Layers[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatal(err)
	}
	blob[0] ^= 0xff
	if err := os.WriteFile(layerPath, blob, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ctx, ref, platform); err == nil {
		t.Fatal("warm verification cache hid mutation of an immutable OCI layer")
	}
}

func TestGraphAndLayerValidationHonorContextCancellation(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/cancel:latest", platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(context.Background(), "example.test/team/cancel:latest", platform)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validateGraphContext(ctx, store.dir, artifact.Manifest, 0, map[string]bool{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("graph validation ignored cancellation: %v", err)
	}
	if err := validateLayersContext(ctx, store.dir, artifact.Manifest); !errors.Is(err, context.Canceled) {
		t.Fatalf("layer validation ignored cancellation: %v", err)
	}
}

func TestImportRejectsTamperedBlobAndDoesNotPublishReference(t *testing.T) {
	archive, _ := testsupport.OCIArchive(t)
	input, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(t.TempDir(), "damaged.tar")
	output, err := os.Create(damaged)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := tar.NewReader(input), tar.NewWriter(output)
	mutated := false
	for {
		h, err := reader.Next()
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
		if strings.HasPrefix(h.Name, "blobs/sha256/") && len(body) > 0 && !mutated {
			body[0] ^= 0xff
			mutated = true
		}
		if err := writer.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Fatal("fixture archive had no blob to corrupt")
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = store.Import(context.Background(), damaged, "example.test/team/tampered:latest", v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "digest or size mismatch") {
		t.Fatalf("tampered blob import error=%v", err)
	}
	items, err := store.List(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("failed import published content: items=%+v err=%v", items, err)
	}
}

func TestImportRejectsDuplicateAndSymlinkArchiveEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []tar.Header
	}{
		{name: "duplicate metadata", entries: []tar.Header{{Name: "oci-layout", Typeflag: tar.TypeReg, Size: int64(len(`{"imageLayoutVersion":"1.0.0"}`))}, {Name: "oci-layout", Typeflag: tar.TypeReg, Size: int64(len(`{"imageLayoutVersion":"1.0.0"}`))}}},
		{name: "symlink metadata", entries: []tar.Header{{Name: "oci-layout", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.tar")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			w := tar.NewWriter(f)
			for _, h := range tc.entries {
				if err := w.WriteHeader(&h); err != nil {
					t.Fatal(err)
				}
				if h.Typeflag == tar.TypeReg {
					if _, err := io.WriteString(w, `{"imageLayoutVersion":"1.0.0"}`); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Import(context.Background(), path, "example.test/team/invalid:latest", v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestImportRejectsBadDiffIDAndConfigPlatformMismatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		badDiffID bool
	}{
		{name: "diffID mismatch", badDiffID: true},
		{name: "config platform mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image, err := random.Image(128, 1)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := image.ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			cfg.OS, cfg.Architecture = "linux", "amd64"
			if tc.name == "config platform mismatch" {
				cfg.OS = "windows"
			}
			image, err = mutate.ConfigFile(image, cfg)
			if err != nil {
				t.Fatal(err)
			}
			platform := v1.Platform{OS: "linux", Architecture: "amd64"}
			index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
			archive := filepath.Join(t.TempDir(), "invalid-image.tar")
			writeLayoutArchive(t, archive, index)
			if tc.badDiffID {
				archive = rewriteArchiveDiffID(t, archive)
			}
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			err = store.Import(context.Background(), archive, "example.test/team/bad:latest", platform)
			if err == nil {
				t.Fatal("invalid image configuration imported")
			}
			if tc.badDiffID && !strings.Contains(err.Error(), "diffID mismatch") {
				t.Fatalf("bad diffID error=%v", err)
			}
			items, err := store.List(context.Background())
			if err != nil || len(items) != 0 {
				t.Fatalf("failed validation published image: items=%+v err=%v", items, err)
			}
		})
	}
}

func TestImportRejectsNonExecutableArtifactMediaType(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	entries := readArchiveEntries(t, archive)
	var index v1.IndexManifest
	if err := json.Unmarshal(entries["index.json"].body, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("fixture index=%+v err=%v", index, err)
	}
	index.Manifests[0].MediaType = types.MediaType("application/vnd.example.sbom.v1+json")
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	entry := entries["index.json"]
	entry.body = indexRaw
	entries["index.json"] = entry
	archive = filepath.Join(t.TempDir(), "artifact.tar")
	writeArchiveEntries(t, archive, entries)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = store.Import(context.Background(), archive, "example.test/team/artifact:latest", platform)
	if err == nil || !strings.Contains(err.Error(), "artifact is not an executable OCI image") {
		t.Fatalf("non-image artifact import error=%v", err)
	}
}

func TestLayerValidationAcceptsZstdAndRejectsUnknownCompression(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, tc := range []struct {
		name      string
		mediaType types.MediaType
		wantError bool
	}{
		{name: "zstd", mediaType: types.OCILayerZStd},
		{name: "unsupported compression", mediaType: types.MediaType("application/vnd.example.layer"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var layer v1.Layer
			if tc.mediaType == types.OCILayerZStd {
				layer = zstdFixtureLayer(t)
			} else {
				var err error
				layer, err = random.Layer(128, types.OCILayer)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.wantError {
				layer = layerMediaOverride{Layer: layer, mediaType: tc.mediaType}
			}
			image, err := mutate.AppendLayers(empty.Image, layer)
			if err != nil {
				t.Fatal(err)
			}
			config, err := image.ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			config.OS, config.Architecture = platform.OS, platform.Architecture
			image, err = mutate.ConfigFile(image, config)
			if err != nil {
				t.Fatal(err)
			}
			index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
			archive := filepath.Join(t.TempDir(), "compression.tar")
			writeLayoutArchive(t, archive, index)
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			err = store.Import(context.Background(), archive, "example.test/team/layer:latest", platform)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "unsupported OCI layer compression") {
					t.Fatalf("unknown layer compression error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("zstd layer validation failed: %v", err)
			}
		})
	}
}

func TestLayerValidationRejectsCorruptGzipWithoutPublishingImage(t *testing.T) {
	base := zstdFixtureLayer(t).(packedLayer)
	compressed := []byte("not-a-valid-gzip-stream")
	digest, _, err := v1.SHA256(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	layer := packedLayer{compressed: compressed, plain: base.plain, digest: digest, diffID: base.diffID, mediaType: types.OCILayer}
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	config.OS, config.Architecture = platform.OS, platform.Architecture
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
	archive := filepath.Join(t.TempDir(), "bad-gzip.tar")
	writeLayoutArchive(t, archive, index)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/bad-gzip:latest", platform); err == nil {
		t.Fatal("invalid gzip layer imported")
	}
	if list, err := store.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("corrupt compressed layer was cataloged: list=%+v err=%v", list, err)
	}
}

func TestLayerValidationAcceptsUncompressedTarAndRejectsInvalidRootfsShape(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	base := zstdFixtureLayer(t).(packedLayer)
	plainDigest, _, err := v1.SHA256(bytes.NewReader(base.plain))
	if err != nil {
		t.Fatal(err)
	}
	uncompressed := packedLayer{compressed: base.plain, plain: base.plain, digest: plainDigest, diffID: base.diffID, mediaType: types.OCIUncompressedLayer}
	image, err := mutate.AppendLayers(empty.Image, uncompressed)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = platform.OS, platform.Architecture
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
	archive := filepath.Join(t.TempDir(), "uncompressed.tar")
	writeLayoutArchive(t, archive, index)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/uncompressed:latest", platform); err != nil {
		t.Fatalf("valid uncompressed layer was rejected: %v", err)
	}

	invalid, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	badConfig, err := invalid.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	badConfig.OS, badConfig.Architecture = platform.OS, platform.Architecture
	badConfig.RootFS.Type = "squashfs"
	invalid, err = mutate.ConfigFile(invalid, badConfig)
	if err != nil {
		t.Fatal(err)
	}
	badIndex := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: invalid, Descriptor: v1.Descriptor{Platform: &platform}})
	badArchive := filepath.Join(t.TempDir(), "invalid-rootfs.tar")
	writeLayoutArchive(t, badArchive, badIndex)
	if err := store.Import(context.Background(), badArchive, "example.test/team/invalid-rootfs:latest", platform); err == nil || !strings.Contains(err.Error(), "invalid OCI rootfs layer identities") {
		t.Fatalf("invalid rootfs metadata import error=%v", err)
	}
	if entries, err := store.List(context.Background()); err != nil || len(entries) != 1 {
		t.Fatalf("invalid rootfs import changed catalog: entries=%+v err=%v", entries, err)
	}
}

func TestLayerValidationRejectsCorruptZstdWithoutPublishingImage(t *testing.T) {
	base := zstdFixtureLayer(t).(packedLayer)
	compressed := []byte("not-a-valid-zstd-stream")
	digest, _, err := v1.SHA256(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	layer := packedLayer{compressed: compressed, plain: base.plain, digest: digest, diffID: base.diffID, mediaType: types.OCILayerZStd}
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	config.OS, config.Architecture = platform.OS, platform.Architecture
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &platform}})
	archive := filepath.Join(t.TempDir(), "bad-zstd.tar")
	writeLayoutArchive(t, archive, index)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/bad-zstd:latest", platform); err == nil {
		t.Fatal("corrupt zstd layer imported")
	}
	if entries, err := store.List(context.Background()); err != nil || len(entries) != 0 {
		t.Fatalf("corrupt zstd import published catalog entry: entries=%+v err=%v", entries, err)
	}
}

func TestLayerValidationFailsClosedWhenAReferencedLayerDisappears(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/missing-layer:latest"
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
	if err := os.Remove(layerPath); err != nil {
		t.Fatal(err)
	}
	if err := validateLayers(store.dir, artifact.Manifest); err == nil {
		t.Fatal("layer validation accepted a missing referenced blob")
	}
}

func TestImportNestedIndexSelectsRequestedPlatformAndRejectsMissingPlatform(t *testing.T) {
	inner, _ := registryIndex(t, 73)
	const ref = "example.test/team/nested:latest"
	outer := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add:        inner,
		Descriptor: v1.Descriptor{Annotations: map[string]string{"org.opencontainers.image.ref.name": ref}},
	})
	archive := filepath.Join(t.TempDir(), "nested-index.tar")
	writeLayoutArchive(t, archive, outer)

	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "", platform); err != nil {
		t.Fatalf("import annotated nested index: %v", err)
	}
	artifact, err := store.Resolve(context.Background(), ref, platform)
	if err != nil {
		t.Fatal(err)
	}
	innerDigest, err := inner.Digest()
	if err != nil {
		t.Fatal(err)
	}
	armImage, err := inner.Image(indexIndexChild(t, inner, "arm64"))
	if err != nil {
		t.Fatal(err)
	}
	armDigest, err := armImage.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Root.Digest != innerDigest || artifact.Manifest.Digest != armDigest {
		t.Fatalf("import root=%s manifest=%s; want nested root=%s arm64=%s", artifact.Root.Digest, artifact.Manifest.Digest, innerDigest, armDigest)
	}

	unsupported, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := v1.Platform{OS: "linux", Architecture: "s390x"}
	err = unsupported.Import(context.Background(), archive, "", missing)
	if err == nil || !strings.Contains(err.Error(), "requested platform is not available") {
		t.Fatalf("missing platform import error=%v", err)
	}
	if entries, err := unsupported.List(context.Background()); err != nil || len(entries) != 0 {
		t.Fatalf("failed missing-platform import published image: entries=%+v err=%v", entries, err)
	}
}

func TestImportAcceptsGzipCompressedOCIArchiveAndRejectsCorruptGzipHeader(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	input, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	compressedPath := filepath.Join(t.TempDir(), "fixture.oci.tar.gz")
	output, err := os.Create(compressedPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(output)
	if _, err := io.Copy(writer, input); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/gzip-archive:latest"
	if err := store.Import(context.Background(), compressedPath, ref, platform); err != nil {
		t.Fatalf("valid gzip-compressed OCI archive rejected: %v", err)
	}
	if _, err := store.Resolve(context.Background(), ref, platform); err != nil {
		t.Fatalf("image from gzip-compressed archive did not resolve: %v", err)
	}

	corruptPath := filepath.Join(t.TempDir(), "corrupt.oci.tar.gz")
	if err := os.WriteFile(corruptPath, []byte{0x1f, 0x8b, 0x00}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), corruptPath, "example.test/team/corrupt-gzip:latest", platform); err == nil {
		t.Fatal("corrupt gzip archive header accepted")
	}
	if entries, err := store.List(context.Background()); err != nil || len(entries) != 1 || entries[0].Tags[0] != ref {
		t.Fatalf("failed gzip import mutated existing catalog: entries=%+v err=%v", entries, err)
	}
}

func TestImportRequiresValidRootIndexAndAReferenceForUntaggedArchives(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "", platform); err == nil || !strings.Contains(err.Error(), "archive needs a reference annotation or --reference") {
		t.Fatalf("untagged archive import error=%v", err)
	}

	entries := readArchiveEntries(t, archive)
	var index v1.IndexManifest
	if err := json.Unmarshal(entries["index.json"].body, &index); err != nil {
		t.Fatal(err)
	}
	index.SchemaVersion = 1
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	entry := entries["index.json"]
	entry.body = indexRaw
	entries["index.json"] = entry
	badArchive := filepath.Join(t.TempDir(), "bad-index.tar")
	writeArchiveEntries(t, badArchive, entries)
	if err := store.Import(context.Background(), badArchive, "example.test/team/bad-index:latest", platform); err == nil || !strings.Contains(err.Error(), "invalid OCI archive layout/index") {
		t.Fatalf("invalid root index schema error=%v", err)
	}
	if refs, err := store.List(context.Background()); err != nil || len(refs) != 0 {
		t.Fatalf("invalid archive attempts published refs=%+v err=%v", refs, err)
	}
}

func TestDigestPinnedExportCanBeImportedWithoutAnAdditionalReference(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchive(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const tag = "example.test/team/digest-export:latest"
	if err := store.Import(ctx, archive, tag, platform); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Resolve(ctx, tag, platform)
	if err != nil {
		t.Fatal(err)
	}
	digestRef := artifact.Root.Digest.String()
	exported := filepath.Join(t.TempDir(), "digest-pinned.oci.tar")
	if err := store.Export(ctx, digestRef, exported, platform, true); err != nil {
		t.Fatalf("export immutable root digest %s: %v", digestRef, err)
	}
	imported, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := imported.Import(ctx, exported, "", platform); err != nil {
		t.Fatalf("import digest-annotated archive without another tag: %v", err)
	}
	resolved, err := imported.Resolve(ctx, digestRef, platform)
	if err != nil || resolved.Root.Digest != artifact.Root.Digest || resolved.Manifest.Digest != artifact.Manifest.Digest {
		t.Fatalf("digest-pinned round-trip=%+v err=%v, want root=%s manifest=%s", resolved, err, artifact.Root.Digest, artifact.Manifest.Digest)
	}
}

func TestImportRejectsInvalidArchiveReferenceAndMismatchedDigestAlias(t *testing.T) {
	archive, platform := testsupport.OCIArchive(t)
	entries := readArchiveEntries(t, archive)
	var index v1.IndexManifest
	if err := json.Unmarshal(entries["index.json"].body, &index); err != nil {
		t.Fatal(err)
	}
	index.Manifests[0].Annotations = map[string]string{"org.opencontainers.image.ref.name": "not a valid image reference"}
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	entry := entries["index.json"]
	entry.body = indexRaw
	entries["index.json"] = entry
	annotated := filepath.Join(t.TempDir(), "invalid-ref.oci.tar")
	writeArchiveEntries(t, annotated, entries)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), annotated, "", platform); err == nil {
		t.Fatal("invalid archive reference annotation accepted")
	}
	const wrongDigest = "example.test/team/digest-alias@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := store.Import(context.Background(), archive, wrongDigest, platform); err == nil || !strings.Contains(err.Error(), "digest reference does not match") {
		t.Fatalf("mismatched digest alias import error=%v", err)
	}
	if refs, err := store.List(context.Background()); err != nil || len(refs) != 0 {
		t.Fatalf("invalid reference imports published images=%+v err=%v", refs, err)
	}
}
