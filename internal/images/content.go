package images

import (
	"encoding/json"
	"fmt"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"io"
	"os"
)

// Content is constructed from a descriptor, not index.json or a mutable tag.
type localContent struct {
	dir         string
	descriptor  v1.Descriptor
	manifest    v1.Manifest
	raw, config []byte
}

func contentAt(dir string, d v1.Descriptor) (v1.Image, error) {
	raw, err := readBlob(dir, d)
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	config, err := readBlob(dir, manifest.Config)
	if err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&localContent{dir: dir, descriptor: d, manifest: manifest, raw: raw, config: config})
}
func (i *localContent) MediaType() (types.MediaType, error) { return i.descriptor.MediaType, nil }
func (i *localContent) RawManifest() ([]byte, error)        { return append([]byte(nil), i.raw...), nil }
func (i *localContent) RawConfigFile() ([]byte, error)      { return append([]byte(nil), i.config...), nil }
func (i *localContent) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	for _, d := range append([]v1.Descriptor{i.manifest.Config}, i.manifest.Layers...) {
		if d.Digest == h {
			return localLayer{dir: i.dir, d: d}, nil
		}
	}
	return nil, fmt.Errorf("unknown OCI blob")
}

type localLayer struct {
	dir string
	d   v1.Descriptor
}

func (l localLayer) Digest() (v1.Hash, error)            { return l.d.Digest, nil }
func (l localLayer) Size() (int64, error)                { return l.d.Size, nil }
func (l localLayer) MediaType() (types.MediaType, error) { return l.d.MediaType, nil }
func (l localLayer) Compressed() (io.ReadCloser, error) {
	path, err := blobPath(l.dir, l.d.Digest)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}
