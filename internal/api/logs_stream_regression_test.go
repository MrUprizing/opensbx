package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type shortReadCloser struct {
	reader io.Reader
	limit  int
}

func (r *shortReadCloser) Read(p []byte) (int, error) {
	if len(p) > r.limit {
		p = p[:r.limit]
	}
	return r.reader.Read(p)
}

func (*shortReadCloser) Close() error { return nil }

func TestLogStreamPreservesLargeAndUnterminatedUTF8Payload(t *testing.T) {
	large := strings.Repeat("x", 70*1024) + "終"
	unterminated := "split-€-終"
	d := &stub{streamCommandLogs: func(string, string) (io.ReadCloser, io.ReadCloser, error) {
		return &shortReadCloser{reader: strings.NewReader(large + "\n" + unterminated), limit: 1}, io.NopCloser(strings.NewReader("")), nil
	}}
	w := do(newRouter(d), http.MethodGet, "/v1/sandboxes/sb/cmd/cmd-1/logs?stream=true", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	decoder := json.NewDecoder(w.Body)
	var record struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	var combined strings.Builder
	for {
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		if record.Type != "stdout" || !utf8.ValidString(record.Data) {
			t.Fatalf("invalid log frame type=%q validUTF8=%t", record.Type, utf8.ValidString(record.Data))
		}
		combined.WriteString(record.Data)
	}
	want := large + "\n" + unterminated
	if combined.String() != want {
		t.Errorf("concatenated log text changed: got %d bytes, want %d", combined.Len(), len(want))
	}
}

func TestLogStreamFlushesPartialTextWhileSourceIsAliveAndKeepsFramesUTF8Valid(t *testing.T) {
	stdoutR, stdoutW := io.Pipe()
	entered := make(chan struct{})
	d := &stub{streamCommandLogs: func(string, string) (io.ReadCloser, io.ReadCloser, error) {
		close(entered)
		return stdoutR, io.NopCloser(strings.NewReader("")), nil
	}}
	router := newRouter(d)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/sandboxes/sb/cmd/cmd-1/logs?stream=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not attach to the source pipe")
	}
	type logRecord struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	type decodeResult struct {
		record logRecord
		err    error
	}
	records := make(chan decodeResult, 16)
	streamDone := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(response.Body)
		for {
			var record logRecord
			err := decoder.Decode(&record)
			if err != nil {
				streamDone <- err
				return
			}
			records <- decodeResult{record: record}
		}
	}()
	cleanup := func() {
		_ = stdoutW.Close()
		cancel()
		_ = response.Body.Close()
	}
	t.Cleanup(cleanup)

	if _, err := stdoutW.Write([]byte("ready\r")); err != nil {
		t.Fatalf("write initial unterminated source bytes: %v", err)
	}
	var combined strings.Builder
	deadline := time.After(time.Second)
	for !strings.HasPrefix(combined.String(), "ready\r") {
		select {
		case item := <-records:
			if item.err != nil {
				t.Fatalf("unexpected stream error before first flush: %v", item.err)
			}
			if !utf8.ValidString(item.record.Data) {
				t.Fatalf("stream emitted an invalid UTF-8 frame: %q", item.record.Data)
			}
			combined.WriteString(item.record.Data)
		case err := <-streamDone:
			t.Fatalf("stream ended before source producer closed: %v", err)
		case <-deadline:
			cleanup()
			t.Fatalf("source bytes were not flushed while producer remained open; received %q", combined.String())
		}
	}
	select {
	case <-streamDone:
		t.Fatal("response ended while the source producer was still open")
	default:
	}

	// Split the three-byte euro sign across independent writes. No frame may
	// expose an incomplete UTF-8 rune, and the concatenated stream stays exact.
	for _, part := range [][]byte{{0xe2}, {0x82}, {0xac}} {
		if _, err := stdoutW.Write(part); err != nil {
			t.Fatalf("write split UTF-8 source bytes: %v", err)
		}
	}
	if err := stdoutW.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case item := <-records:
			if !utf8.ValidString(item.record.Data) {
				t.Fatalf("stream emitted an invalid UTF-8 frame: %q", item.record.Data)
			}
			combined.WriteString(item.record.Data)
		case err := <-streamDone:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream decode ended with %v", err)
			}
			if combined.String() != "ready\r€" {
				t.Fatalf("concatenated stream = %q, want exact source text", combined.String())
			}
			return
		case <-time.After(2 * time.Second):
			t.Fatal("stream did not finish after the producer closed")
		}
	}
}

type blockingReadCloser struct {
	started   chan struct{}
	closed    chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return 0, errors.New("reader closed")
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func TestLogStreamCancellationClosesBlockedSourceReaders(t *testing.T) {
	stdout, stderr := newBlockingReadCloser(), newBlockingReadCloser()
	d := &stub{streamCommandLogs: func(string, string) (io.ReadCloser, io.ReadCloser, error) { return stdout, stderr, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRouter(d)
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb/cmd/cmd-1/logs?stream=true", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); r.ServeHTTP(w, req) }()
	for _, reader := range []*blockingReadCloser{stdout, stderr} {
		select {
		case <-reader.started:
		case <-time.After(time.Second):
			t.Fatal("stream reader did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = stdout.Close()
		_ = stderr.Close()
		<-done
		t.Fatal("log handler did not return after request cancellation")
	}
	for name, reader := range map[string]*blockingReadCloser{"stdout": stdout, "stderr": stderr} {
		select {
		case <-reader.closed:
		default:
			t.Errorf("%s source reader was not closed", name)
		}
	}
}

func TestLogStreamCancellationCleansUpLivePipeReaders(t *testing.T) {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	t.Cleanup(func() {
		_ = stdoutW.Close()
		_ = stderrW.Close()
	})
	d := &stub{streamCommandLogs: func(string, string) (io.ReadCloser, io.ReadCloser, error) {
		return stdoutR, stderrR, nil
	}}
	router := newRouter(d)
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/sandboxes/sb/cmd/cmd-1/logs?stream=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		_ = stdoutW.Close()
		_ = stderrW.Close()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Fatal("API stream handler remained stuck even after closing both pipe writers")
		}
		t.Fatal("API stream cleanup did not close pipe readers and return")
	}
}

type failingResponseWriter struct {
	header http.Header
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (*failingResponseWriter) WriteHeader(int)       {}
func (*failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}
func (*failingResponseWriter) Flush() {}

func TestLogStreamWriteFailureClosesBlockedSourceReaders(t *testing.T) {
	stderr := newBlockingReadCloser()
	d := &stub{streamCommandLogs: func(string, string) (io.ReadCloser, io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("line\n")), stderr, nil
	}}
	r := newRouter(d)
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb/cmd/cmd-1/logs?stream=true", nil)
	writer := &failingResponseWriter{header: make(http.Header)}
	done := make(chan struct{})
	go func() { defer close(done); r.ServeHTTP(writer, req) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = stderr.Close()
		<-done
		t.Fatal("log handler did not return after response write failure")
	}
	select {
	case <-stderr.closed:
	default:
		t.Error("stderr source reader was not closed after response write failure")
	}
}
