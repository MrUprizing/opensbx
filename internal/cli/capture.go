package cli

import (
	"bytes"
	"errors"
)

const jsonCaptureLimit = 8 << 20

// A single log observer writes both streams, sharing one aggregate byte budget.
type jsonCapture struct {
	total          int
	stdout, stderr bytes.Buffer
}

type captureWriter struct {
	capture *jsonCapture
	buffer  *bytes.Buffer
}

func (w captureWriter) Write(p []byte) (int, error) {
	if len(p) > jsonCaptureLimit-w.capture.total {
		return 0, errors.New("foreground JSON capture limit exceeded (8 MiB total); use raw streaming for larger output")
	}
	n, err := w.buffer.Write(p)
	w.capture.total += n
	return n, err
}
