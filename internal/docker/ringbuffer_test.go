package docker

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"opensbx/internal/sandbox"
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

func TestRingBufferSnapshotRetainsTailButReaderReportsTruncation(t *testing.T) {
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
	buf := make([]byte, 8)
	n, err := reader.Read(buf)
	if n != 0 || !errors.Is(err, sandbox.ErrLogTruncated) {
		t.Fatalf("reader after initial overflow returned %q, %v; want explicit truncation", buf[:n], err)
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

func TestRingBufferReaderCloseUnblocksAnAlreadyBlockedRead(t *testing.T) {
	r := newRingBuffer(8)
	waiting := make(chan struct{})
	r.cond = sync.NewCond(&observedCondLocker{locker: &r.mu, waiting: waiting})
	reader := r.NewReader()
	type result struct {
		err error
	}
	done := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := reader.Read(make([]byte, 1))
		done <- result{err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reader goroutine did not start")
	}
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("Read did not enter the ring condition wait")
	}

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if !errors.Is(result.err, io.EOF) {
			t.Fatalf("blocked Read returned %v, want EOF after reader Close", result.err)
		}
	case <-time.After(time.Second):
		// Wake the reader through its owning buffer so a failing regression never
		// leaves a goroutine behind.
		r.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("reader remained blocked even after owner buffer closed")
		}
		t.Fatal("reader Close did not wake an already-blocked Read")
	}
}

type observedCondLocker struct {
	locker  sync.Locker
	waiting chan struct{}
	once    sync.Once
}

func (l *observedCondLocker) Lock() { l.locker.Lock() }
func (l *observedCondLocker) Unlock() {
	l.once.Do(func() { close(l.waiting) })
	l.locker.Unlock()
}

func TestRingBufferReturnsNilBytesWhenEmpty(t *testing.T) {
	if got := newRingBuffer(4).Bytes(); got != nil {
		t.Fatalf("Bytes() on empty buffer = %v, want nil", got)
	}
}

func TestRingBufferReaderReportsTruncationOnInitialAttachAndAfterFallingBehind(t *testing.T) {
	t.Run("initial attach after overflow", func(t *testing.T) {
		r := newRingBuffer(4)
		_, _ = io.WriteString(r, "abcdef")
		reader := r.NewReader()
		defer reader.Close()
		buf := make([]byte, 8)
		n, err := reader.Read(buf)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "truncat") {
			t.Fatalf("first read = %q, %v; want explicit truncation rather than silent tail", buf[:n], err)
		}
	})

	t.Run("consumer falls behind", func(t *testing.T) {
		r := newRingBuffer(4)
		reader := r.NewReader()
		defer reader.Close()
		_, _ = io.WriteString(r, "ab")
		buf := make([]byte, 2)
		if n, err := reader.Read(buf); err != nil || string(buf[:n]) != "ab" {
			t.Fatalf("initial read = %q, %v", buf[:n], err)
		}
		_, _ = io.WriteString(r, "cdefgh")
		buf = make([]byte, 8)
		n, err := reader.Read(buf)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "truncat") {
			t.Fatalf("read after overwrite = %q, %v; want explicit truncation", buf[:n], err)
		}
	})
}
