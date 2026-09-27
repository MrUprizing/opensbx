package images

import (
	"context"
	"fmt"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"os"
	"reflect"
	"sync"
)

type fileStamp struct {
	info   os.FileInfo
	change string
}
type verifiedGraph struct {
	files map[string]fileStamp
	size  int64
}
type verificationCache struct {
	mu      sync.Mutex
	entries map[string]verifiedGraph
}

func stamp(path string) (fileStamp, error) {
	i, err := os.Lstat(path)
	if err != nil {
		return fileStamp{}, err
	}
	if !i.Mode().IsRegular() {
		return fileStamp{}, fmt.Errorf("OCI blob is not a regular file")
	}
	change := ""
	v := reflect.ValueOf(i.Sys())
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
		if v.Kind() == reflect.Struct {
			for _, field := range []string{"Ctim", "Ctimespec", "Ctime"} {
				f := v.FieldByName(field)
				if f.IsValid() && f.CanInterface() {
					change = fmt.Sprint(f.Interface())
					break
				}
			}
		}
	}
	return fileStamp{i, change}, nil
}
func sameStamp(a, b fileStamp) bool {
	return os.SameFile(a.info, b.info) && a.info.Size() == b.info.Size() && a.info.ModTime() == b.info.ModTime() && a.info.Mode() == b.info.Mode() && a.change == b.change
}
func (s *Store) verified(ctx context.Context, d v1.Descriptor) (int64, error) {
	if !d.MediaType.IsImage() {
		return 0, fmt.Errorf("prepared artifact is not an image")
	}
	key := fmt.Sprintf("%s/%d/%s", d.Digest, d.Size, d.MediaType)
	s.verifiedCache.mu.Lock()
	entry, ok := s.verifiedCache.entries[key]
	s.verifiedCache.mu.Unlock()
	if ok {
		for path, old := range entry.files {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			current, err := stamp(path)
			if err != nil || !sameStamp(old, current) {
				ok = false
				break
			}
		}
		if ok {
			return entry.size, nil
		}
	}
	files := map[string]fileStamp{}
	var collect func(v1.Descriptor, int) error
	collect = func(desc v1.Descriptor, depth int) error {
		if depth > 16 || len(files) > 100000 {
			return fmt.Errorf("OCI graph exceeds limits")
		}
		path, err := blobPath(s.dir, desc.Digest)
		if err != nil {
			return err
		}
		if _, ok := files[path]; ok {
			return nil
		}
		f, err := stamp(path)
		if err != nil {
			return err
		}
		files[path] = f
		kids, err := children(s.dir, desc)
		if err != nil {
			return err
		}
		for _, kid := range kids {
			if err = collect(kid, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(d, 0); err != nil {
		return 0, err
	}
	size, err := validateGraphContext(ctx, s.dir, d, 0, map[string]bool{})
	if err != nil {
		return 0, err
	}
	if err = validateLayersContext(ctx, s.dir, d); err != nil {
		return 0, err
	}
	for path, old := range files {
		now, err := stamp(path)
		if err != nil {
			return 0, err
		}
		if !sameStamp(old, now) {
			return 0, fmt.Errorf("OCI content changed during validation")
		}
	}
	// Process-local only; bounded and populated exclusively after verification.
	if len(files) <= 256 {
		s.verifiedCache.mu.Lock()
		if len(s.verifiedCache.entries) >= 64 {
			s.verifiedCache.entries = nil
		}
		if s.verifiedCache.entries == nil {
			s.verifiedCache.entries = map[string]verifiedGraph{}
		}
		s.verifiedCache.entries[key] = verifiedGraph{files, size}
		s.verifiedCache.mu.Unlock()
	}
	return size, nil
}
