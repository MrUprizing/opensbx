package client

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestDecodeRecordAcceptsExactlyOneMiBAndRejectsOneByteMore(t *testing.T) {
	const prefix = `{"type":"stdout","data":"`
	const suffix = `"}`
	for _, tc := range []struct {
		name      string
		overLimit bool
	}{
		{"exact encoded-frame limit", false},
		{"encoded frame one byte over limit", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frameSize := MaxStreamFrameBytes
			if tc.overLimit {
				frameSize++
			}
			data := strings.Repeat("x", frameSize-len(prefix)-len(suffix))
			encoded := []byte(fmt.Sprintf("%s%s%s\n", prefix, data, suffix))
			if len(bytes.TrimSuffix(encoded, []byte{'\n'})) != frameSize {
				t.Fatalf("fixture encoded size=%d, want=%d", len(bytes.TrimSuffix(encoded, []byte{'\n'})), frameSize)
			}
			scanner := streamRecords(bytes.NewReader(encoded))
			var record struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			err := decodeRecord(scanner, &record)
			if tc.overLimit {
				if err == nil || !strings.Contains(err.Error(), "1 MiB") {
					t.Fatalf("over-limit frame error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact-limit frame rejected: %v", err)
			}
			if record.Type != "stdout" || len(record.Data) != len(data) {
				t.Fatalf("decoded exact-limit frame type=%q dataBytes=%d", record.Type, len(record.Data))
			}
		})
	}
}
