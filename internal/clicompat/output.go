package clicompat

import "io"

// Output records write failures Cobra's help printers cannot return. Data passes
// through unchanged, including raw guest output and JSON.
type Output struct {
	Writer io.Writer
	Err    error
}

func (w *Output) Write(p []byte) (int, error) {
	if w.Err != nil {
		return 0, w.Err
	}
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.Err = err
	return n, err
}
