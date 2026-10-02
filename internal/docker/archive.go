package docker

import (
	"io"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// writeImageArchive owns the compressed layer readers opened by tarball.Write,
// but not the destination writer. The pinned upstream writer does not close those
// readers, even on Size, Read or writer failures. Keep ownership local to this
// conversion rather than changing the source image's general read semantics.
func writeImageArchive(ref name.Reference, image v1.Image, writer io.Writer) (err error) {
	owned := &archiveImage{Image: image}
	defer func() {
		for _, reader := range owned.readers {
			if closeErr := reader.Close(); err == nil {
				err = closeErr
			}
		}
	}()
	return tarball.Write(ref, owned, writer)
}

type archiveImage struct {
	v1.Image
	readers []*archiveReader
}

func (image *archiveImage) Layers() ([]v1.Layer, error) {
	layers, err := image.Image.Layers()
	if err != nil {
		return nil, err
	}
	wrapped := make([]v1.Layer, len(layers))
	for i, layer := range layers {
		wrapped[i] = archiveLayer{Layer: layer, owner: image}
	}
	return wrapped, nil
}

type archiveLayer struct {
	v1.Layer
	owner *archiveImage
}

func (layer archiveLayer) Compressed() (io.ReadCloser, error) {
	reader, err := layer.Layer.Compressed()
	if reader == nil {
		return nil, err
	}
	owned := &archiveReader{ReadCloser: reader}
	layer.owner.readers = append(layer.owner.readers, owned)
	return owned, err
}

// Make Close idempotent in case upstream also closes a reader. EOF is not an
// ownership boundary: deferred cleanup must also cover partially read streams.
type archiveReader struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (reader *archiveReader) Close() error {
	reader.once.Do(func() { reader.err = reader.ReadCloser.Close() })
	return reader.err
}
