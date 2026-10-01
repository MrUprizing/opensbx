package docker

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/klauspost/compress/zstd"
	"opensbx/internal/images"
	"opensbx/internal/sandbox"
)

// ImageExists checks if an image exists locally.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := c.cli.ImageInspect(ctx, image)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Serialize each private tag across clients. External tag changes are harmless:
// verification and subsequent creation use the inspected immutable ID, not the tag.
var materializeLocks cacheLocks

func (c *Client) Materialize(ctx context.Context, image sandbox.Image) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tag, err := name.NewTag("localhost/opensbx-cache:" + strings.TrimPrefix(image.ManifestDigest, "sha256:"))
	if err != nil {
		return "", err
	}
	release, err := materializeLocks.acquire(ctx, tag.String())
	if err != nil {
		return "", err
	}
	defer release()
	dir, err := os.MkdirTemp("", "opensbx-cache-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "image.tar")
	archive, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	// A classic store uses config IDs; a containerd store may use a synthesized
	// manifest or index ID. Neither the private tag nor its native ID proves that
	// the selected source content survived the archive conversion.
	result, inspectErr := c.cli.ImageInspect(ctx, tag.String())
	if inspectErr == nil {
		archive.Close()
		return c.verifyCache(ctx, image, result.ID, dir)
	}
	if !errdefs.IsNotFound(inspectErr) {
		archive.Close()
		return "", inspectErr
	}
	err = tarball.Write(tag, image.Content, archive)
	closeErr := archive.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	loaded, err := c.cli.ImageLoad(ctx, f)
	if err != nil {
		return "", err
	}
	defer loaded.Close()
	dec := json.NewDecoder(io.LimitReader(loaded, 16<<20))
	for {
		var message struct {
			Error string `json:"error"`
		}
		err = dec.Decode(&message)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if message.Error != "" {
			return "", errors.New(message.Error)
		}
	}
	result, err = c.cli.ImageInspect(ctx, tag.String())
	if err != nil {
		return "", err
	}
	return c.verifyCache(ctx, image, result.ID, dir)
}

func (c *Client) verifyCache(ctx context.Context, image sandbox.Image, id, dir string) (string, error) {
	h, err := v1.NewHash(id)
	if err != nil || h.Algorithm != "sha256" || len(h.Hex) != 64 || strings.Trim(h.Hex, "0123456789abcdef") != "" {
		return "", errors.New("runtime: Docker cache has no immutable image ID")
	}
	// Bind the expected config to the selected source manifest, not just to a
	// caller-supplied config digest. Docker archives can change compression and
	// manifest media types, so verify config bytes and ordered uncompressed layers.
	manifest, err := image.Content.Digest()
	if err != nil {
		return "", err
	}
	config, err := image.Content.ConfigName()
	if err != nil {
		return "", err
	}
	if manifest.String() != image.ManifestDigest || config.String() != image.ConfigDigest {
		return "", errors.New("runtime: Docker cache source manifest/config mismatch")
	}
	saved, err := c.cli.ImageSave(ctx, []string{id})
	if err != nil {
		return "", err
	}
	defer saved.Close()
	path := filepath.Join(dir, "verified.tar")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	n, copyErr := io.Copy(f, io.LimitReader(saved, images.MaxArchiveBytes+1))
	closeErr := f.Close()
	// Cancellation can close the HTTP body while Copy is blocked. Preserve the
	// caller's context error instead of leaking the resulting transport error.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n > images.MaxArchiveBytes {
		return "", errors.New("runtime: Docker cache export exceeds disk budget")
	}
	exported, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		return "", err
	}
	actualConfig, err := exported.ConfigName()
	if err != nil {
		return "", err
	}
	if actualConfig != config {
		return "", errors.New("runtime: Docker cache config digest mismatch")
	}
	cfg, err := exported.ConfigFile()
	if err != nil {
		return "", err
	}
	variant := cfg.Variant
	if cfg.Architecture == "arm64" && variant == "v8" || cfg.Architecture == "amd64" && variant == "v1" {
		variant = ""
	}
	wantVariant := image.Platform.Variant
	if image.Platform.Architecture == "arm64" && wantVariant == "v8" || image.Platform.Architecture == "amd64" && wantVariant == "v1" {
		wantVariant = ""
	}
	if cfg.OS != image.Platform.OS || cfg.Architecture != image.Platform.Architecture || variant != wantVariant {
		return "", errors.New("runtime: Docker cache platform mismatch")
	}
	archiveManifest, err := tarball.LoadManifest(func() (io.ReadCloser, error) { return os.Open(path) })
	if err != nil {
		return "", err
	}
	if len(archiveManifest) != 1 || cfg.RootFS.Type != "layers" || len(archiveManifest[0].Layers) != len(cfg.RootFS.DiffIDs) {
		return "", errors.New("runtime: Docker cache layer count mismatch")
	}
	if err := verifyCacheLayers(ctx, path, archiveManifest[0].Layers, cfg.RootFS.DiffIDs); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return id, nil
}

// Hash actual archive entries, not Layer.DiffID metadata. In particular, an
// image abstraction may resolve repeated DiffIDs to the first matching layer,
// hiding different bytes in a later archive entry with the same claimed DiffID.
func verifyCacheLayers(ctx context.Context, path string, paths []string, diffIDs []v1.Hash) error {
	wanted := make(map[string]v1.Hash, len(paths))
	for i, path := range paths {
		path = strings.TrimPrefix(path, "./")
		if previous, ok := wanted[path]; ok && previous != diffIDs[i] {
			return errors.New("runtime: Docker cache conflicting layer identities")
		}
		wanted[path] = diffIDs[i]
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(cacheContextReader{ctx, f})
	seen := make(map[string]bool, len(wanted))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entry := strings.TrimPrefix(h.Name, "./")
		want, ok := wanted[entry]
		if !ok {
			continue
		}
		if seen[entry] || h.Typeflag != tar.TypeReg {
			return errors.New("runtime: Docker cache layers must be unique regular files")
		}
		seen[entry] = true
		actual, err := cacheLayerHash(ctx, tr)
		if err != nil {
			return err
		}
		if actual != want {
			return errors.New("runtime: Docker cache layer content mismatch")
		}
	}
	if len(seen) != len(wanted) {
		return errors.New("runtime: Docker cache layer missing")
	}
	return nil
}

func cacheLayerHash(ctx context.Context, input io.Reader) (v1.Hash, error) {
	b := bufio.NewReader(input)
	magic, _ := b.Peek(4)
	var r io.Reader = b
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(b)
		if err != nil {
			return v1.Hash{}, err
		}
		defer gz.Close()
		r = gz
	} else if string(magic) == "\x28\xb5\x2f\xfd" {
		z, err := zstd.NewReader(b, zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return v1.Hash{}, err
		}
		defer z.Close()
		r = z
	}
	hash, size, err := v1.SHA256(io.LimitReader(cacheContextReader{ctx, r}, images.MaxArchiveBytes+1))
	if err == nil && size > images.MaxArchiveBytes {
		err = errors.New("runtime: Docker cache expanded layer exceeds budget")
	}
	return hash, err
}

type cacheContextReader struct {
	ctx context.Context
	io.Reader
}

func (r cacheContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
