package images

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/go-containerregistry/pkg/name"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Import accepts a standard OCI layout tar (optionally gzip-compressed). It
// never extracts paths: only allowlisted regular entries are written to staging.
func (s *Store) Import(ctx context.Context, path, ref string, p v1.Platform) error {
	p = canonicalPlatform(p)
	return func() error {
		stage, err := os.MkdirTemp(s.dir, ".import-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		if err = os.MkdirAll(filepath.Join(stage, "blobs", "sha256"), 0700); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		var input io.Reader = f
		var magic [2]byte
		_, _ = io.ReadFull(f, magic[:])
		if _, err = f.Seek(0, 0); err != nil {
			return err
		}
		if magic == [2]byte{0x1f, 0x8b} {
			gz, err := gzip.NewReader(f)
			if err != nil {
				return err
			}
			defer gz.Close()
			input = gz
		}
		limited := &io.LimitedReader{R: contextReader{ctx, input}, N: MaxArchiveBytes + 1}
		tr := tar.NewReader(limited)
		seen := map[string]bool{}
		var index v1.IndexManifest
		var layoutOK bool
		for count := 0; ; count++ {
			if err = ctx.Err(); err != nil {
				return err
			}
			if count > 100000 {
				return errors.New("archive has too many entries")
			}
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if limited.N <= 0 {
				return errors.New("archive exceeds disk budget")
			}
			entry := strings.TrimPrefix(h.Name, "./")
			if h.Typeflag == tar.TypeDir && (entry == "blobs/" || entry == "blobs" || entry == "blobs/sha256/" || entry == "blobs/sha256" || entry == "" || entry == ".") {
				continue
			}
			if h.Typeflag != tar.TypeReg || seen[entry] || h.Size < 0 {
				return errors.New("archive must contain unique regular OCI files, not links")
			}
			seen[entry] = true
			switch entry {
			case "oci-layout", "index.json":
				if h.Size > maxMetadataBytes {
					return errors.New("archive metadata exceeds limit")
				}
				b, err := io.ReadAll(tr)
				if err != nil {
					return err
				}
				if entry == "index.json" {
					if err = json.Unmarshal(b, &index); err != nil {
						return err
					}
				} else {
					var l struct {
						Version string `json:"imageLayoutVersion"`
					}
					if err = json.Unmarshal(b, &l); err != nil {
						return err
					}
					layoutOK = l.Version == "1.0.0"
				}
			default:
				if !strings.HasPrefix(entry, "blobs/sha256/") {
					return errors.New("unexpected OCI archive path")
				}
				hash, err := v1.NewHash("sha256:" + strings.TrimPrefix(entry, "blobs/sha256/"))
				if err != nil {
					return err
				}
				if err = putBlob(stage, v1.Descriptor{Digest: hash, Size: h.Size}, tr); err != nil {
					return err
				}
			}
		}
		if limited.N <= 0 {
			return errors.New("archive exceeds disk budget")
		}
		if !layoutOK || index.SchemaVersion != 2 || len(index.Manifests) == 0 {
			return errors.New("invalid OCI archive layout/index")
		}
		if ref != "" && len(index.Manifests) != 1 {
			return errors.New("--reference requires a single archive root")
		}
		entries := map[string]Entry{}
		for _, root := range index.Manifests {
			// Portable import is strict: every root graph must be complete.
			if _, err = validateGraphContext(ctx, stage, root, 0, map[string]bool{}); err != nil {
				return err
			}
			d, err := selectManifest(stage, root, p, 0)
			if err != nil {
				return err
			}
			if err = validateLayersContext(ctx, stage, d); err != nil {
				return err
			}
			key := ref
			if key == "" {
				key = root.Annotations["org.opencontainers.image.ref.name"]
			}
			if key == "" {
				return errors.New("archive needs a reference annotation or --reference")
			}
			key, err = reference(key)
			if err != nil {
				return err
			}
			parsed, err := name.ParseReference(key)
			if err != nil {
				return err
			}
			if digest, ok := parsed.(name.Digest); ok && digest.DigestStr() != root.Digest.String() {
				return errors.New("archive digest reference does not match its root descriptor")
			}
			entries[key] = Entry{Root: root, Prepared: map[string]v1.Descriptor{p.String(): d}}
		}
		return s.publish(ctx, stage, entries)
	}()
}

// Export defaults to one prepared variant, whose manifest is the archive root.
// Full export is permitted only after validating every blob in the source index.
func (s *Store) Export(ctx context.Context, ref, path string, p v1.Platform, full bool) error {
	return s.export(ctx, ref, path, p, full, ref)
}
func (s *Store) export(ctx context.Context, ref, path string, p v1.Platform, full bool, archiveRef string) error {
	p = canonicalPlatform(p)
	var root v1.Descriptor
	err := s.transaction(ctx, func(c *catalog) error {
		e, err := lookup(c, ref)
		if err != nil {
			return err
		}
		root = e.Root
		if !full {
			var ok bool
			root, ok = e.Prepared[p.String()]
			if !ok {
				return errors.New("platform is not prepared")
			}
		}
		return nil
	}, false)
	if err != nil {
		return err
	}
	return s.exportDescriptor(ctx, root, path, archiveRef)
}

// exportDescriptor operates on a pinned immutable graph, never a catalog alias.
func (s *Store) exportDescriptor(ctx context.Context, root v1.Descriptor, path, archiveRef string) error {
	var err error
	if _, err = validateGraphContext(ctx, s.dir, root, 0, map[string]bool{}); err != nil {
		return err
	}
	if _, err = os.Lstat(path); err == nil {
		return errors.New("export destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".export-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	tw := tar.NewWriter(f)
	write := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b))}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err = write("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if strings.HasPrefix(archiveRef, "sha256:") {
		archiveRef = "localhost/opensbx-export@" + root.Digest.String()
	}
	parsed, err := name.ParseReference(archiveRef)
	if err != nil {
		return err
	}
	if _, ok := parsed.(name.Digest); ok {
		archiveRef = parsed.Context().Digest(root.Digest.String()).Name()
	}
	root.Annotations = map[string]string{"org.opencontainers.image.ref.name": archiveRef}
	b, err := json.Marshal(v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{root}})
	if err != nil {
		return err
	}
	if err = write("index.json", b); err != nil {
		return err
	}
	seen := map[string]bool{}
	var copyGraph func(v1.Descriptor) error
	copyGraph = func(d v1.Descriptor) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[d.Digest.String()] {
			return nil
		}
		seen[d.Digest.String()] = true
		path, err := blobPath(s.dir, d.Digest)
		if err != nil {
			return err
		}
		blob, err := os.Open(path)
		if err != nil {
			return err
		}
		if err = tw.WriteHeader(&tar.Header{Name: "blobs/sha256/" + d.Digest.Hex, Mode: 0600, Size: d.Size}); err != nil {
			blob.Close()
			return err
		}
		_, err = io.Copy(tw, contextReader{ctx, blob})
		blob.Close()
		if err != nil {
			return err
		}
		kids, err := children(s.dir, d)
		if err != nil {
			return err
		}
		for _, kid := range kids {
			if err = copyGraph(kid); err != nil {
				return err
			}
		}
		return nil
	}
	if err = copyGraph(root); err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Link is an atomic no-replace publication, unlike Rename over an existing file.
	return os.Link(f.Name(), path)
}
