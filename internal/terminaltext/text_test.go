package terminaltext

import (
	"errors"
	"strings"
	"testing"
	"unicode"
)

func TestEscapeMakesTerminalControlsAndInvisibleFormattingVisible(t *testing.T) {
	source := "name\x1b[2J\x1b]52;c;clipboard\x07\x7f\u0085\u202e\u2066"
	for _, layout := range []bool{false, true} {
		got := Escape(source, layout)
		for _, r := range got {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
				t.Errorf("Escape(layout=%t) left unsafe rune U+%04X in %q", layout, r, got)
			}
		}
		if !strings.Contains(got, "name") || !strings.Contains(got, "clipboard") {
			t.Errorf("Escape(layout=%t) lost ordinary text: %q", layout, got)
		}
	}
	layout := Escape("first\nsecond\tvalue", true)
	if !strings.Contains(layout, "first\nsecond\tvalue") {
		t.Fatalf("layout-safe newline/tab were not retained: %q", layout)
	}
	plain := Escape("first\nsecond\tvalue", false)
	for _, r := range plain {
		if unicode.IsControl(r) {
			t.Fatalf("plain Escape retained control U+%04X: %q", r, plain)
		}
	}
}

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestLayoutWriterKeepsLayoutAndPropagatesOutputErrors(t *testing.T) {
	var out strings.Builder
	writer := LayoutWriter(&out)
	input := "row\x1b[31mred\x1b[0m\tvalue\n"
	n, err := writer.Write([]byte(input))
	if err != nil || n != len(input) {
		t.Fatalf("LayoutWriter.Write() = %d, %v, want original input byte count %d", n, err, len(input))
	}
	if !strings.Contains(out.String(), "\tvalue\n") || strings.ContainsAny(out.String(), "\x1b") {
		t.Fatalf("layout/control rendering = %q", out.String())
	}
	want := errors.New("terminal unavailable")
	if _, err := LayoutWriter(failedWriter{err: want}).Write([]byte("text")); !errors.Is(err, want) {
		t.Fatalf("LayoutWriter output error = %v, want %v", err, want)
	}
}
