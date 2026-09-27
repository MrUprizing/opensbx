package images

import (
	"context"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"opensbx/internal/sandbox"
)

// ResolveImage exposes verified OCI content to a cache adapter, never a tag for
// the adapter to resolve independently against a registry.
func (s *Store) ResolveImage(ctx context.Context, ref string, p sandbox.Platform) (sandbox.Image, error) {
	platform := v1.Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant}
	a, err := s.Resolve(ctx, ref, platform)
	if err != nil {
		return sandbox.Image{}, err
	}
	config, err := a.Image.ConfigName()
	if err != nil {
		return sandbox.Image{}, err
	}
	image := sandbox.Image{RootDigest: a.Root.Digest.String(), ManifestDigest: a.Manifest.Digest.String(), ConfigDigest: config.String(), Platform: p, Content: a.Image}
	image.Archive = func(path, ref string) error {
		return s.exportDescriptor(ctx, a.Manifest, path, ref)
	}
	image.Validate = func(ctx context.Context) error { _, err := s.verified(ctx, a.Manifest); return err }
	return image, nil
}
