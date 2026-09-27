// Package images owns the local OCI layout and catalog, independently of runtimes.
package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"opensbx/internal/sandbox"
)

const MaxArchiveBytes int64 = 20 << 30
const maxMetadataBytes int64 = 16 << 20

type Entry struct {
	Root     v1.Descriptor            `json:"root"`
	Prepared map[string]v1.Descriptor `json:"prepared"`
}
type catalog struct {
	References map[string]Entry `json:"references"`
}
type Store struct {
	dir               string
	lock              *flock.Flock
	mu                sync.Mutex
	registryTransport http.RoundTripper
	verifiedCache     verificationCache
}
type Artifact struct {
	Root     v1.Descriptor
	Manifest v1.Descriptor
	Image    v1.Image
}

func Open(dataDir string, options ...Option) (*Store, error) {
	dir := filepath.Join(dataDir, "images")
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: flock.New(filepath.Join(dir, "catalog.lock"))}
	for _, option := range options {
		option(s)
	}
	err := s.transaction(context.Background(), func(c *catalog) error {
		path := filepath.Join(dir, "oci-layout")
		b, err := os.ReadFile(path)
		if err == nil {
			var layout struct {
				Version string `json:"imageLayoutVersion"`
			}
			if json.Unmarshal(b, &layout) != nil || layout.Version != "1.0.0" {
				return errors.New("invalid owned OCI layout version")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return atomicFile(path, []byte(`{"imageLayoutVersion":"1.0.0"}`))
	}, false)
	return s, err
}

func atomicFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Every reader and writer takes the same OS file lock, including separate CLI
// processes. Blobs are immutable; the catalog is replaced only after validation.
func (s *Store) transaction(ctx context.Context, fn func(*catalog) error, write bool) error {
	for !s.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer s.mu.Unlock()
	ok, err := s.lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	defer s.lock.Unlock()
	c := catalog{References: map[string]Entry{}}
	b, err := os.ReadFile(filepath.Join(s.dir, "catalog.json"))
	if err == nil {
		if err = json.Unmarshal(b, &c); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if c.References == nil {
		return errors.New("invalid image catalog")
	}
	if err := s.writeIndex(&c); err != nil {
		return err
	}
	if err := fn(&c); err != nil {
		return err
	}
	if !write {
		return nil
	}
	// Multiple tags for the same immutable root share platform availability.
	prepared := map[string]map[string]v1.Descriptor{}
	for _, entry := range c.References {
		key := entry.Root.Digest.String()
		if prepared[key] == nil {
			prepared[key] = map[string]v1.Descriptor{}
		}
		for platform, d := range entry.Prepared {
			prepared[key][platform] = d
		}
	}
	for ref, entry := range c.References {
		entry.Prepared = prepared[entry.Root.Digest.String()]
		c.References[ref] = entry
	}
	if err = s.writeIndex(&c); err != nil {
		return err
	}
	b, err = json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicFile(filepath.Join(s.dir, "catalog.json"), b)
}

func (s *Store) writeIndex(c *catalog) error {
	index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{}}
	seen := map[string]bool{}
	for ref, e := range c.References {
		d := e.Root
		d.Annotations = map[string]string{"org.opencontainers.image.ref.name": ref}
		index.Manifests = append(index.Manifests, d)
		seen[d.Digest.String()] = true
	}
	// Prepared manifests are also untagged layout roots, so layout.Image can
	// read them without traversing unprepared sibling descriptors.
	for _, e := range c.References {
		for _, d := range e.Prepared {
			if !seen[d.Digest.String()] {
				d.Annotations = nil
				index.Manifests = append(index.Manifests, d)
				seen[d.Digest.String()] = true
			}
		}
	}
	sort.Slice(index.Manifests, func(i, j int) bool {
		a, b := index.Manifests[i], index.Manifests[j]
		if a.Digest == b.Digest {
			return a.Annotations["org.opencontainers.image.ref.name"] < b.Annotations["org.opencontainers.image.ref.name"]
		}
		return a.Digest.String() < b.Digest.String()
	})
	b, err := json.Marshal(index)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "index.json")
	previous, err := os.ReadFile(path)
	if err == nil && bytes.Equal(previous, b) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicFile(path, b)
}

func reference(raw string) (string, error) {
	r, err := name.ParseReference(raw)
	if err != nil {
		return "", err
	}
	return r.Name(), nil
}
func lookup(c *catalog, ref string) (Entry, error) {
	if key, err := reference(ref); err == nil {
		if e, ok := c.References[key]; ok {
			return e, nil
		}
	}
	for _, e := range c.References {
		if e.Root.Digest.String() == ref {
			return e, nil
		}
	}
	return Entry{}, sandbox.ErrImageNotFound
}
func blobPath(dir string, h v1.Hash) (string, error) {
	if h.Algorithm != "sha256" || len(h.Hex) != 64 {
		return "", errors.New("only sha256 OCI digests are supported")
	}
	if _, err := v1.NewHash(h.String()); err != nil {
		return "", err
	}
	for _, c := range h.Hex {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", errors.New("invalid blob digest")
		}
	}
	return filepath.Join(dir, "blobs", "sha256", h.Hex), nil
}
func putBlob(dir string, d v1.Descriptor, r io.Reader) error {
	p, err := blobPath(dir, d.Digest)
	if err != nil {
		return err
	}
	if d.Size < 0 || d.Size > MaxArchiveBytes {
		return errors.New("blob exceeds disk budget")
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".blob-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	h, n, err := v1.SHA256(io.TeeReader(io.LimitReader(r, d.Size+1), f))
	if err != nil {
		f.Close()
		return err
	}
	if n != d.Size || h != d.Digest {
		f.Close()
		return errors.New("OCI blob digest or size mismatch")
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
func readBlob(dir string, d v1.Descriptor) ([]byte, error) {
	if d.Size > maxMetadataBytes {
		return nil, errors.New("OCI metadata exceeds limit")
	}
	p, err := blobPath(dir, d.Digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	h, n, err := v1.SHA256(bytesReader(b))
	if err != nil || n != d.Size || h != d.Digest {
		return nil, errors.New("corrupt OCI metadata")
	}
	return b, nil
}

func (s *Store) Resolve(ctx context.Context, ref string, platform v1.Platform) (Artifact, error) {
	platform = canonicalPlatform(platform)
	var a Artifact
	var entry Entry
	err := s.transaction(ctx, func(c *catalog) error {
		e, err := lookup(c, ref)
		if err != nil {
			return err
		}
		entry = e
		return nil
	}, false)
	if err != nil {
		return a, err
	}
	if _, err := readBlob(s.dir, entry.Root); err != nil {
		return a, err
	}
	d, ok := entry.Prepared[platform.String()]
	if !ok {
		return a, fmt.Errorf("%w: %s is not prepared; explicitly pull/import this platform", sandbox.ErrUnsupported, platform.String())
	}
	if _, err := s.verified(ctx, d); err != nil {
		return a, err
	}
	img, err := contentAt(s.dir, d)
	if err != nil {
		return a, err
	}
	a = Artifact{Root: entry.Root, Manifest: d, Image: img}
	return a, err
}
func (s *Store) Remove(ctx context.Context, ref string, force bool) error {
	return s.transaction(ctx, func(c *catalog) error {
		e, err := lookup(c, ref)
		if err != nil {
			if force && errors.Is(err, sandbox.ErrImageNotFound) {
				return nil
			}
			return err
		}
		if key, err := reference(ref); err == nil {
			if _, ok := c.References[key]; ok {
				delete(c.References, key)
				return nil
			}
		}
		for key, item := range c.References {
			if item.Root.Digest == e.Root.Digest {
				delete(c.References, key)
			}
		}
		// Never garbage collect here: owned execution metadata can still pin blobs.
		return nil
	}, true)
}
func (s *Store) List(ctx context.Context) ([]sandbox.ImageSummary, error) {
	out := []sandbox.ImageSummary{}
	var entries map[string]Entry
	err := s.transaction(ctx, func(c *catalog) error { entries = c.References; return nil }, false)
	if err != nil {
		return nil, err
	}
	err = func() error {
		byID := map[string]*sandbox.ImageSummary{}
		for ref, e := range entries {
			id := e.Root.Digest.String()
			item := byID[id]
			if item == nil {
				item = &sandbox.ImageSummary{ID: sandbox.ImageID(id)}
				byID[id] = item
				seen := map[string]bool{}
				for _, d := range e.Prepared {
					_, err := s.verified(ctx, d)
					if err != nil {
						return err
					}
					n, err := graphSize(s.dir, d, seen)
					if err != nil {
						return err
					}
					item.Size += n
				}
			}
			item.Tags = append(item.Tags, ref)
		}
		for _, item := range byID {
			sort.Strings(item.Tags)
			out = append(out, *item)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return nil
	}()
	return out, err
}
func (s *Store) Inspect(ctx context.Context, ref string, p v1.Platform) (sandbox.ImageDetail, error) {
	a, err := s.Resolve(ctx, ref, p)
	if err != nil {
		return sandbox.ImageDetail{}, err
	}
	cfg, err := a.Image.ConfigFile()
	if err != nil {
		return sandbox.ImageDetail{}, err
	}
	all, err := s.List(ctx)
	if err != nil {
		return sandbox.ImageDetail{}, err
	}
	for _, item := range all {
		if string(item.ID) == a.Root.Digest.String() {
			return sandbox.ImageDetail{ImageSummary: item, Created: cfg.Created.Time.Format(time.RFC3339), OS: cfg.OS, Architecture: cfg.Architecture}, nil
		}
	}
	return sandbox.ImageDetail{}, sandbox.ErrImageNotFound
}
