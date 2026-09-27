package images

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/zstd"
)

// Validate expanded layer identities before any native runtime can unpack them.
// No tar member is extracted by OpenSBX. The whole selected root filesystem has
// a bounded decoded byte budget, including gzip/zstd layers inside an OCI tar.
func validateLayers(dir string, desc v1.Descriptor) error {
	return validateLayersContext(context.Background(), dir, desc)
}
func validateLayersContext(ctx context.Context, dir string, desc v1.Descriptor) error {
	b, err := readBlob(dir, desc)
	if err != nil {
		return err
	}
	var manifest v1.Manifest
	if err = json.Unmarshal(b, &manifest); err != nil {
		return err
	}
	b, err = readBlob(dir, manifest.Config)
	if err != nil {
		return err
	}
	var cfg v1.ConfigFile
	if err = json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.RootFS.Type != "layers" || len(cfg.RootFS.DiffIDs) != len(manifest.Layers) {
		return errors.New("invalid OCI rootfs layer identities")
	}
	remaining := MaxArchiveBytes
	for i, d := range manifest.Layers {
		path, err := blobPath(dir, d.Digest)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		var reader io.Reader = f
		closeDecoder := func() {}
		switch mt := string(d.MediaType); {
		case strings.HasSuffix(mt, "+gzip") || strings.HasSuffix(mt, ".gzip"):
			gz, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				return err
			}
			reader = gz
			closeDecoder = func() { _ = gz.Close() }
		case strings.HasSuffix(mt, "+zstd"):
			z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20), zstd.WithDecoderMaxWindow(128<<20))
			if err != nil {
				f.Close()
				return err
			}
			reader = z
			closeDecoder = z.Close
		case strings.HasSuffix(mt, ".tar"):
		default:
			f.Close()
			return errors.New("unsupported OCI layer compression")
		}
		h, n, err := v1.SHA256(io.LimitReader(contextReader{ctx, reader}, remaining+1))
		closeDecoder()
		f.Close()
		if err != nil {
			return err
		}
		if n > remaining {
			return errors.New("expanded OCI rootfs exceeds disk budget")
		}
		remaining -= n
		if h != cfg.RootFS.DiffIDs[i] {
			return errors.New("OCI layer diffID mismatch")
		}
	}
	return nil
}
