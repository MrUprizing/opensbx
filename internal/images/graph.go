package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Called only after graph verification; count shared blobs once across variants.
func graphSize(dir string, d v1.Descriptor, seen map[string]bool) (int64, error) {
	if seen[d.Digest.String()] {
		return 0, nil
	}
	seen[d.Digest.String()] = true
	kids, err := children(dir, d)
	if err != nil {
		return 0, err
	}
	n := d.Size
	for _, kid := range kids {
		size, err := graphSize(dir, kid, seen)
		if err != nil {
			return 0, err
		}
		n += size
	}
	return n, nil
}

func canonicalPlatform(p v1.Platform) v1.Platform {
	if p.Architecture == "arm64" && p.Variant == "v8" || p.Architecture == "amd64" && p.Variant == "v1" {
		p.Variant = ""
	}
	return p
}
func samePlatform(a, b v1.Platform) bool {
	a, b = canonicalPlatform(a), canonicalPlatform(b)
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant
}
func children(dir string, d v1.Descriptor) ([]v1.Descriptor, error) {
	if d.MediaType.IsIndex() {
		b, err := readBlob(dir, d)
		if err != nil {
			return nil, err
		}
		var m v1.IndexManifest
		if err = json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		if m.SchemaVersion != 2 {
			return nil, errors.New("invalid OCI index schema")
		}
		return m.Manifests, nil
	}
	if d.MediaType.IsImage() {
		b, err := readBlob(dir, d)
		if err != nil {
			return nil, err
		}
		var m v1.Manifest
		if err = json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		if m.SchemaVersion != 2 {
			return nil, errors.New("invalid OCI manifest schema")
		}
		return append([]v1.Descriptor{m.Config}, m.Layers...), nil
	}
	return nil, nil
}
func validateGraph(dir string, d v1.Descriptor, depth int, seen map[string]bool) (int64, error) {
	return validateGraphContext(context.Background(), dir, d, depth, seen)
}
func validateGraphContext(ctx context.Context, dir string, d v1.Descriptor, depth int, seen map[string]bool) (int64, error) {
	if d.MediaType == "" {
		return 0, errors.New("OCI descriptor media type is required")
	}
	if len(d.URLs) > 0 {
		return 0, errors.New("external OCI descriptor URLs are not permitted")
	}
	if len(d.Data) > 0 {
		hash, size, err := v1.SHA256(bytes.NewReader(d.Data))
		if err != nil || hash != d.Digest || size != d.Size {
			return 0, errors.New("embedded OCI descriptor data mismatch")
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if depth > 16 || len(seen) > 100000 {
		return 0, errors.New("OCI graph exceeds limits")
	}
	p, err := blobPath(dir, d.Digest)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || d.Size < 0 || d.Size > MaxArchiveBytes || info.Size() != d.Size {
		return 0, errors.New("OCI descriptor size mismatch")
	}
	if seen[d.Digest.String()] {
		return 0, nil
	}
	seen[d.Digest.String()] = true
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	h, n, err := v1.SHA256(io.LimitReader(contextReader{ctx, f}, MaxArchiveBytes+1))
	f.Close()
	if err != nil {
		return 0, err
	}
	if h != d.Digest || n != d.Size {
		return 0, errors.New("OCI graph digest or size mismatch")
	}
	kids, err := children(dir, d)
	if err != nil {
		return 0, err
	}
	for _, kid := range kids {
		size, err := validateGraphContext(ctx, dir, kid, depth+1, seen)
		if err != nil {
			return 0, err
		}
		n += size
		if n > MaxArchiveBytes {
			return 0, errors.New("OCI graph exceeds disk budget")
		}
	}
	return n, nil
}
func selectManifest(dir string, d v1.Descriptor, p v1.Platform, depth int) (v1.Descriptor, error) {
	if depth > 16 {
		return v1.Descriptor{}, errors.New("OCI index nesting limit exceeded")
	}
	if d.MediaType.IsIndex() {
		kids, err := children(dir, d)
		if err != nil {
			return v1.Descriptor{}, err
		}
		for _, kid := range kids {
			if kid.Platform != nil && !samePlatform(*kid.Platform, p) {
				continue
			}
			selected, err := selectManifest(dir, kid, p, depth+1)
			if err == nil {
				return selected, nil
			}
		}
		return v1.Descriptor{}, errors.New("requested platform is not available")
	}
	if !d.MediaType.IsImage() {
		return v1.Descriptor{}, errors.New("artifact is not an executable OCI image")
	}
	kids, err := children(dir, d)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(kids) == 0 {
		return v1.Descriptor{}, errors.New("missing image config")
	}
	b, err := readBlob(dir, kids[0])
	if err != nil {
		return v1.Descriptor{}, err
	}
	var cfg v1.ConfigFile
	if err = json.Unmarshal(b, &cfg); err != nil {
		return v1.Descriptor{}, err
	}
	if !samePlatform(v1.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}, p) {
		return v1.Descriptor{}, errors.New("image config does not match requested platform")
	}
	d.Platform = &p
	return d, nil
}
func descriptor(b []byte, mt types.MediaType) v1.Descriptor {
	h, n, _ := v1.SHA256(bytes.NewReader(b))
	return v1.Descriptor{Digest: h, Size: n, MediaType: mt}
}
