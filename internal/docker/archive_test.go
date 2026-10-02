package docker

import (
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestWriteImageArchiveClosesReadersWhenLayerSizeFails(t *testing.T) {
	sizeErr := errors.New("injected layer size failure")
	image, trackers := archiveTrackingImage(t, []archiveLayerOptions{{sizeErr: sizeErr}})
	err := writeTrackedImageArchive(t, image, io.Discard)
	if !errors.Is(err, sizeErr) {
		t.Fatalf("writeImageArchive() error = %v, want size error", err)
	}
	assertArchiveReaderCounts(t, trackers, []int{1}, []int{1})
}

func TestWriteImageArchiveClosesReaderAfterWriterFails(t *testing.T) {
	image, trackers := archiveTrackingImage(t, []archiveLayerOptions{{}})
	writeErr := errors.New("injected archive writer failure")
	writer := openAwareFailingWriter{tracker: trackers[0], err: writeErr}
	err := writeTrackedImageArchive(t, image, writer)
	if !errors.Is(err, writeErr) {
		t.Fatalf("writeImageArchive() error = %v, want writer error", err)
	}
	assertArchiveReaderCounts(t, trackers, []int{1}, []int{1})
}

func TestWriteImageArchiveReturnsCloseErrorAfterSuccessfulConversion(t *testing.T) {
	closeErr := errors.New("injected compressed stream close failure")
	image, trackers := archiveTrackingImage(t, []archiveLayerOptions{{closeErr: closeErr}})
	err := writeTrackedImageArchive(t, image, io.Discard)
	if !errors.Is(err, closeErr) {
		t.Fatalf("writeImageArchive() error = %v, want close error", err)
	}
	assertArchiveReaderCounts(t, trackers, []int{1}, []int{1})
}

func TestWriteImageArchivePreservesConversionErrorOverCloseError(t *testing.T) {
	readErr := errors.New("injected compressed stream read failure")
	closeErr := errors.New("injected compressed stream close failure")
	image, trackers := archiveTrackingImage(t, []archiveLayerOptions{{readErr: readErr, closeErr: closeErr}})
	err := writeTrackedImageArchive(t, image, io.Discard)
	if !errors.Is(err, readErr) {
		t.Fatalf("writeImageArchive() error = %v, want original conversion error", err)
	}
	if errors.Is(err, closeErr) {
		t.Fatalf("writeImageArchive() error = %v, close error must not replace conversion error", err)
	}
	assertArchiveReaderCounts(t, trackers, []int{1}, []int{1})
}

func TestWriteImageArchiveClosesEveryReaderAfterLaterCloseFailure(t *testing.T) {
	closeErr := errors.New("injected second compressed stream close failure")
	image, trackers := archiveTrackingImage(t, []archiveLayerOptions{{}, {closeErr: closeErr}, {}})
	err := writeTrackedImageArchive(t, image, io.Discard)
	if !errors.Is(err, closeErr) {
		t.Fatalf("writeImageArchive() error = %v, want later close error", err)
	}
	assertArchiveReaderCounts(t, trackers, []int{1, 1, 1}, []int{1, 1, 1})
}

type archiveLayerOptions struct {
	sizeErr       error
	readErr       error
	closeErr      error
	compressedErr error
}

type trackedArchiveImage struct {
	v1.Image
	layers []*archiveTrackingLayer
}

func (image *trackedArchiveImage) Layers() ([]v1.Layer, error) {
	layers := make([]v1.Layer, len(image.layers))
	for i, layer := range image.layers {
		layers[i] = layer
	}
	return layers, nil
}

type archiveTrackingLayer struct {
	v1.Layer
	options archiveLayerOptions
	mu      sync.Mutex
	opened  int
	closed  int
}

func (layer *archiveTrackingLayer) Size() (int64, error) {
	if layer.options.sizeErr != nil {
		return 0, layer.options.sizeErr
	}
	return layer.Layer.Size()
}

func (layer *archiveTrackingLayer) Compressed() (io.ReadCloser, error) {
	reader, err := layer.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	layer.mu.Lock()
	layer.opened++
	layer.mu.Unlock()
	if layer.options.readErr != nil {
		reader = archiveReadCloser{Reader: archiveFailingReader{err: layer.options.readErr}, closer: reader}
	}
	return &archiveTrackingReadCloser{ReadCloser: reader, onClose: func() error {
		layer.mu.Lock()
		layer.closed++
		layer.mu.Unlock()
		return layer.options.closeErr
	}}, layer.options.compressedErr
}

func (layer *archiveTrackingLayer) counts() (int, int) {
	layer.mu.Lock()
	defer layer.mu.Unlock()
	return layer.opened, layer.closed
}

type archiveReadCloser struct {
	io.Reader
	closer io.Closer
}

func (reader archiveReadCloser) Close() error { return reader.closer.Close() }

type archiveFailingReader struct{ err error }

func (reader archiveFailingReader) Read([]byte) (int, error) { return 0, reader.err }

type archiveTrackingReadCloser struct {
	io.ReadCloser
	onClose func() error
	once    sync.Once
	err     error
}

func (reader *archiveTrackingReadCloser) Close() error {
	reader.once.Do(func() {
		_ = reader.ReadCloser.Close()
		reader.err = reader.onClose()
	})
	return reader.err
}

type openAwareFailingWriter struct {
	tracker *archiveTrackingLayer
	err     error
}

func (writer openAwareFailingWriter) Write(p []byte) (int, error) {
	opened, _ := writer.tracker.counts()
	if opened > 0 {
		return 0, writer.err
	}
	return len(p), nil
}

func archiveTrackingImage(t *testing.T, options []archiveLayerOptions) (*trackedArchiveImage, []*archiveTrackingLayer) {
	t.Helper()
	base, err := random.Image(1024, int64(len(options)))
	if err != nil {
		t.Fatal(err)
	}
	layers, err := base.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) == 0 {
		t.Fatal("fixture image has no layers")
	}
	tracked := make([]*archiveTrackingLayer, len(options))
	for i, option := range options {
		tracked[i] = &archiveTrackingLayer{Layer: layers[i], options: option}
	}
	return &trackedArchiveImage{Image: base, layers: tracked}, tracked
}

func writeTrackedImageArchive(t *testing.T, image v1.Image, writer io.Writer) error {
	t.Helper()
	ref, err := name.NewTag("localhost/archive-fixture:test")
	if err != nil {
		t.Fatal(err)
	}
	return writeImageArchive(ref, image, writer)
}

func assertArchiveReaderCounts(t *testing.T, layers []*archiveTrackingLayer, opened, closed []int) {
	t.Helper()
	if len(layers) != len(opened) || len(layers) != len(closed) {
		t.Fatalf("test expectation lengths layers/open/closed = %d/%d/%d", len(layers), len(opened), len(closed))
	}
	for i, layer := range layers {
		gotOpened, gotClosed := layer.counts()
		if gotOpened != opened[i] || gotClosed != closed[i] {
			t.Errorf("layer %d reader open/close count = %d/%d, want %d/%d", i, gotOpened, gotClosed, opened[i], closed[i])
		}
	}
}

func TestWriteImageArchiveClosesReaderReturnedAlongsideCompressedError(t *testing.T) {
	compressedErr := errors.New("compressed stream setup failure")
	image, trackers := archiveTrackingImage(t, []archiveLayerOptions{{compressedErr: compressedErr}})
	err := writeTrackedImageArchive(t, image, io.Discard)
	if !errors.Is(err, compressedErr) {
		t.Fatalf("writeImageArchive() error = %v, want Compressed error", err)
	}
	assertArchiveReaderCounts(t, trackers, []int{1}, []int{1})
}
