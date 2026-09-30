// Package terminaltext escapes untrusted presentation text, never guest data streams.
package terminaltext

import (
	"io"
	"strconv"
	"strings"
	"unicode"
)

// Escape makes controls and invisible formatting characters visible. Layout
// permits only newline/tab for directory listings and generated help/diagnostics.
func Escape(text string, layout bool) string {
	var out strings.Builder
	for _, r := range text {
		if layout && (r == '\n' || r == '\t') {
			out.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			quoted := strconv.QuoteRune(r)
			out.WriteString(quoted[1 : len(quoted)-1])
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

type layoutWriter struct{ out io.Writer }

// LayoutWriter sanitizes complete diagnostic/help writes while retaining layout.
// Do not use it for exec output, logs, file reads or JSON serialization.
func LayoutWriter(out io.Writer) io.Writer { return layoutWriter{out: out} }

func (w layoutWriter) Write(p []byte) (int, error) {
	text := Escape(string(p), true)
	n, err := io.WriteString(w.out, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
