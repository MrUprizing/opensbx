package applecontainer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"opensbx/internal/images"
	"opensbx/internal/sandbox"
)

// CacheReference uses an explicit DNS authority recognized identically by OCI
// tooling and Apple. go-containerregistry treats bare localhost as a Docker Hub
// path, while Apple's parser recognizes it as an authority.
func CacheReference(manifestDigest string) (string, error) {
	h, err := v1.NewHash(manifestDigest)
	if err != nil || h.Algorithm != "sha256" || len(h.Hex) != 64 || strings.Trim(h.Hex, "0123456789abcdef") != "" {
		return "", errors.New("invalid cache manifest digest")
	}
	ref, err := imageRef("opensbx.invalid/cache@" + h.String())
	if err != nil {
		return "", err
	}
	oci, err := name.NewDigest(ref, name.StrictValidation)
	if err != nil {
		return "", err
	}
	if oci.Name() != ref {
		return "", errors.New("cache reference normalization mismatch")
	}
	return ref, nil
}

func (c *Client) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	if err := c.Ping(ctx); err != nil {
		return sandbox.Capabilities{}, err
	}
	return sandbox.Capabilities{Runtime: "container", Version: "1.4.1/oci-load-v1", Platform: sandbox.Platform{OS: "linux", Architecture: "arm64"}}, nil
}
func (c *Client) Materialize(ctx context.Context, image sandbox.Image) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if image.Platform.OS != "linux" || image.Platform.Architecture != "arm64" || image.Platform.Variant != "" {
		return "", sandbox.ErrUnsupported
	}
	dir, err := os.MkdirTemp("", "opensbx-apple-cache-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	archive := filepath.Join(dir, "image.tar")
	ref, err := CacheReference(image.ManifestDigest)
	if err != nil {
		return "", err
	}
	if err = image.Archive(archive, ref); err != nil {
		return "", err
	}
	// Reimport from the owned layout, not a registry. The private reference is
	// derived from content and is never a mutable caller-supplied tag.
	if _, err = c.run(ctx, nil, "image", "load", "--input", archive); err != nil {
		return "", err
	}
	// Loading/tagging may synthesize a native index. Verify the exported selected
	// manifest instead of confusing the native index ID with source OCI identity.
	saved := filepath.Join(dir, "verified.tar")
	if _, err = c.run(ctx, nil, "image", "save", "--output", saved, "--platform", "linux/arm64", ref); err != nil {
		return "", err
	}
	store, err := images.Open(filepath.Join(dir, "verify"))
	if err != nil {
		return "", err
	}
	p := v1.Platform{OS: "linux", Architecture: "arm64"}
	const verificationRef = "localhost/opensbx-verification:cache"
	if err = store.Import(ctx, saved, verificationRef, p); err != nil {
		return "", err
	}
	a, err := store.Resolve(ctx, verificationRef, p)
	if err != nil {
		return "", err
	}
	config, err := a.Image.ConfigName()
	if err != nil {
		return "", err
	}
	if a.Manifest.Digest.String() != image.ManifestDigest || config.String() != image.ConfigDigest {
		return "", errors.New("Apple 1.4.1 cache manifest/config verification failed")
	}
	return ref, nil
}
