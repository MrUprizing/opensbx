package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxStreamFrameBytes bounds each encoded NDJSON record, independent of total
// command output. API log chunks are at most 32KiB before JSON encoding.
const MaxStreamFrameBytes = 1 << 20

func streamRecords(body io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32*1024), MaxStreamFrameBytes+2)
	return scanner
}

func decodeRecord(scanner *bufio.Scanner, result any) error {
	for scanner.Scan() {
		frame := scanner.Bytes()
		if len(frame) > MaxStreamFrameBytes {
			return errors.New("NDJSON frame exceeds the 1 MiB encoded record limit")
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		if !utf8.Valid(frame) {
			return errors.New("NDJSON frame contains invalid UTF-8")
		}
		return json.Unmarshal(frame, result)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("NDJSON frame limit (1 MiB) exceeded or stream read failed: %w", err)
	}
	return io.EOF
}
