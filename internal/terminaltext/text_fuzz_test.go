package terminaltext

import (
	"testing"
	"unicode"
	"unicode/utf8"
)

func FuzzEscapeAlwaysReturnsSafeValidUTF8(f *testing.F) {
	f.Add("plain text")
	f.Add("\x1b[2J\x00\u202e\u2066")
	f.Add("row\ncolumn\tvalue")
	f.Add(string([]byte{0xff, 0xfe, 'x'}))
	f.Fuzz(func(t *testing.T, input string) {
		for _, layout := range []bool{false, true} {
			got := Escape(input, layout)
			if !utf8.ValidString(got) {
				t.Fatalf("Escape(layout=%t) returned invalid UTF-8", layout)
			}
			if len(got) > 10*len(input) {
				t.Fatalf("Escape(layout=%t) expanded %d input bytes to %d, beyond the 10x UTF-8-safe bound", layout, len(input), len(got))
			}
			for _, r := range got {
				if layout && (r == '\n' || r == '\t') {
					continue
				}
				if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
					t.Fatalf("Escape(layout=%t) left unsafe rune U+%04X in %q", layout, r, got)
				}
			}
		}
	})
}
