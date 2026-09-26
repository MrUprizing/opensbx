package docker

import (
	"io"
	"testing"
)

func TestRingBufferReturnsWrittenBytesAndEOFAfterClose(t *testing.T) {
	r := newRingBuffer(8)
	if n, err := r.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("Write() = (%d, %v), want (5, nil)", n, err)
	}
	reader := r.NewReader()
	r.Close()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("ReadAll() = %q, want hello", got)
	}
	if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Read() after consuming closed buffer = %v, want EOF", err)
	}
}

func TestRingBufferRetainsMostRecentBytesWhenCapacityIsExceeded(t *testing.T) {
	r := newRingBuffer(5)
	if _, err := r.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("defg")); err != nil {
		t.Fatal(err)
	}
	if got := string(r.Bytes()); got != "cdefg" {
		t.Fatalf("Bytes() = %q, want cdefg", got)
	}

	reader := r.NewReader()
	r.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cdefg" {
		t.Fatalf("reader returned %q, want cdefg", got)
	}
}

func TestRingBufferReaderCloseUnblocksFutureReads(t *testing.T) {
	r := newRingBuffer(8)
	reader := r.NewReader()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Read() after reader close = %v, want EOF", err)
	}
	r.Close()
}

func TestRingBufferReturnsNilBytesWhenEmpty(t *testing.T) {
	if got := newRingBuffer(4).Bytes(); got != nil {
		t.Fatalf("Bytes() on empty buffer = %v, want nil", got)
	}
}
