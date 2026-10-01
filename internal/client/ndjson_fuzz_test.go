package client

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzDecodeRecordBoundsAndValidatesNDJSON(f *testing.F) {
	f.Add([]byte(`{"type":"stdout","data":"hello"}`))
	f.Add([]byte("{\"data\":\"\xff\"}\n"))
	f.Add([]byte("not-json\n"))
	f.Add([]byte(strings.Repeat("x", MaxStreamFrameBytes+1) + "\n"))
	f.Fuzz(func(t *testing.T, frame []byte) {
		scanner := streamRecords(bytes.NewReader(frame))
		var record json.RawMessage
		err := decodeRecord(scanner, &record)
		if err == nil {
			line := scanner.Bytes()
			if len(line) > MaxStreamFrameBytes {
				t.Fatalf("accepted oversized encoded frame: %d bytes", len(line))
			}
			if !utf8.Valid(line) {
				t.Fatal("accepted NDJSON frame with invalid UTF-8")
			}
			if !json.Valid(line) {
				t.Fatalf("accepted malformed JSON frame %q", line)
			}
		}
	})
}
