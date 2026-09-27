package images

import (
	"context"
	"os"
	"path/filepath"
)

// Network/decompression/archive I/O is already complete in private staging.
// This lock covers only immutable blob publication and the catalog merge.
func (s *Store) publish(ctx context.Context, stage string, entries map[string]Entry) error {
	files, err := os.ReadDir(filepath.Join(stage, "blobs", "sha256"))
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(c *catalog) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Rename(filepath.Join(stage, "blobs", "sha256", file.Name()), filepath.Join(s.dir, "blobs", "sha256", file.Name())); err != nil {
				return err
			}
		}
		for key, e := range entries {
			if old, ok := c.References[key]; ok && old.Root.Digest == e.Root.Digest {
				for p, d := range old.Prepared {
					if _, ok := e.Prepared[p]; !ok {
						e.Prepared[p] = d
					}
				}
			}
			c.References[key] = e
		}
		return nil
	}, true)
}
