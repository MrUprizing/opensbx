package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Pull retains the source root and all nested index metadata but downloads
// layer/config blobs only for the explicitly requested platform.
func (s *Store) Pull(ctx context.Context, ref string, p v1.Platform) (pullErr error) {
	defer func() {
		if pullErr != nil {
			d := registryErrorDiagnostic(pullErr)
			log.Printf("registry_pull_failed category=%s stage=%s status=%d", d.category, d.stage, d.status)
		}
	}()
	p = canonicalPlatform(p)
	r, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	target := s
	stage, err := os.MkdirTemp(s.dir, ".pull-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = os.MkdirAll(filepath.Join(stage, "blobs", "sha256"), 0700); err != nil {
		return err
	}
	s = &Store{dir: stage, registryTransport: target.registryTransport}
	c := catalog{References: map[string]Entry{}}
	err = func(c *catalog) error {
		opts, closeTransport, err := s.registryOptionsFor(ctx, r)
		if err != nil {
			return err
		}
		defer closeTransport()
		root, err := remote.Get(r, opts...)
		if err != nil {
			return safeRegistryError(err)
		}
		if digest, ok := r.(name.Digest); ok && digest.DigestStr() != root.Digest.String() {
			return errors.New("registry root does not match requested digest")
		}
		var selected v1.Descriptor
		var budget int64
		visited := map[string]v1.Descriptor{}
		var visit func(*remote.Descriptor, int) error
		visit = func(d *remote.Descriptor, depth int) error {
			if len(visited) >= 100000 {
				return errors.New("registry graph exceeds descriptor limit")
			}
			if old, ok := visited[d.Digest.String()]; ok {
				if old.Size != d.Size || old.MediaType != d.MediaType {
					return errors.New("registry descriptor mismatch")
				}
				return nil
			}
			visited[d.Digest.String()] = d.Descriptor
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.Size < 0 || d.Size > maxMetadataBytes {
				return errors.New("registry metadata exceeds limit")
			}
			if len(d.URLs) > 0 {
				return errors.New("registry-controlled external descriptor URLs are not permitted")
			}
			if depth > 16 {
				return errors.New("OCI index nesting limit exceeded")
			}
			budget += d.Size
			if budget > MaxArchiveBytes {
				return errors.New("pull exceeds disk budget")
			}
			if err := putBlob(s.dir, d.Descriptor, bytesReader(d.Manifest)); err != nil {
				return err
			}
			if d.MediaType.IsIndex() {
				var index v1.IndexManifest
				if err := json.Unmarshal(d.Manifest, &index); err != nil {
					return err
				}
				if index.SchemaVersion != 2 {
					return errors.New("invalid registry index schema")
				}
				for _, child := range index.Manifests {
					if len(child.URLs) > 0 {
						return errors.New("registry-controlled external descriptor URLs are not permitted")
					}
					if !child.MediaType.IsIndex() && (child.Platform == nil || !samePlatform(*child.Platform, p)) {
						continue
					}
					if !child.MediaType.IsIndex() && selected.Digest.Hex != "" {
						continue
					}
					if old, ok := visited[child.Digest.String()]; ok {
						if old.Size != child.Size || old.MediaType != child.MediaType {
							return errors.New("registry descriptor mismatch")
						}
						continue
					}
					childRef := r.Context().Digest(child.Digest.String())
					next, err := remote.Get(childRef, opts...)
					if err != nil {
						return safeRegistryError(err)
					}
					if next.Digest != child.Digest || next.Size != child.Size || next.MediaType != child.MediaType {
						return errors.New("registry descriptor mismatch")
					}
					if err = visit(next, depth+1); err != nil {
						return err
					}
				}
				return nil
			}
			if !d.MediaType.IsImage() {
				return errors.New("unsupported OCI artifact")
			}
			img, err := d.Image()
			if err != nil {
				return safeRegistryError(err)
			}
			manifest, err := img.Manifest()
			if err != nil {
				return safeRegistryError(err)
			}
			if len(manifest.Config.URLs) > 0 {
				return errors.New("registry-controlled external config URLs are not permitted")
			}
			for _, layer := range manifest.Layers {
				if len(layer.URLs) > 0 {
					return errors.New("registry-controlled external layer URLs are not permitted")
				}
			}
			cfg, err := img.ConfigFile()
			if err != nil {
				return safeRegistryError(err)
			}
			if !samePlatform(v1.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}, p) {
				return fmt.Errorf("image platform does not match %s", p.String())
			}
			raw, err := img.RawConfigFile()
			if err != nil {
				return safeRegistryError(err)
			}
			budget += int64(len(raw))
			if err = putBlob(s.dir, manifest.Config, bytesReader(raw)); err != nil {
				return err
			}
			for _, layer := range manifest.Layers {
				if len(layer.URLs) > 0 {
					return errors.New("registry-controlled external layer URLs are not permitted")
				}
				budget += layer.Size
				if budget > MaxArchiveBytes {
					return errors.New("pull exceeds disk budget")
				}
				l, err := img.LayerByDigest(layer.Digest)
				if err != nil {
					return safeRegistryError(err)
				}
				rc, err := l.Compressed()
				if err != nil {
					return safeRegistryError(err)
				}
				err = putBlob(s.dir, layer, safeRegistryReader{rc})
				rc.Close()
				if err != nil {
					return err
				}
			}
			selected = d.Descriptor
			selected.Platform = &p
			return nil
		}
		if err = visit(root, 0); err != nil {
			return err
		}
		if selected.Digest.Hex == "" {
			return fmt.Errorf("image has no %s variant; emulation is not enabled", p.String())
		}
		if _, err = validateGraphContext(ctx, s.dir, selected, 0, map[string]bool{}); err != nil {
			return err
		}
		if err = validateLayersContext(ctx, s.dir, selected); err != nil {
			return err
		}
		e := Entry{Root: root.Descriptor, Prepared: map[string]v1.Descriptor{}}
		if previous, ok := c.References[r.Name()]; ok && previous.Root.Digest == root.Digest {
			e = previous
		}
		e.Prepared[p.String()] = selected
		c.References[r.Name()] = e
		return nil
	}(&c)
	if err != nil {
		return err
	}
	return target.publish(ctx, stage, c.References)
}

type safeRegistryReader struct{ io.Reader }

func (r safeRegistryReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		err = safeRegistryError(err)
	}
	return n, err
}
