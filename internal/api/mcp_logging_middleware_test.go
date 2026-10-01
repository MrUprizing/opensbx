package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

func TestMCPMetadataLoggerPreservesRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(MCPMetadataLogger())
	r.POST("/v1/mcp", func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Data(http.StatusOK, "application/json", b)
	})

	body := []byte(`{"jsonrpc":"2.0","method":"tools/list"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != string(body) {
		t.Fatalf("handler received modified body: %q", w.Body.String())
	}
}

func TestMCPMetadataLoggerAcceptsExactBodyLimitAndRejectsOneByteMore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validJSON := []byte(`{"method":"tools/list"}`)
	for _, tt := range []struct {
		name           string
		bodyLength     int
		wantStatus     int
		wantDownstream bool
	}{
		{name: "exactly 1 MiB", bodyLength: mcpMaxBodyBytes, wantStatus: http.StatusOK, wantDownstream: true},
		{name: "one byte over", bodyLength: mcpMaxBodyBytes + 1, wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := append(append([]byte(nil), validJSON...), bytes.Repeat([]byte(" "), tt.bodyLength-len(validJSON))...)
			original := &trackedRequestBody{reader: bytes.NewReader(body)}
			requestContext := context.WithValue(context.Background(), mcpContextKey{}, "request-context")
			request := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil).WithContext(requestContext)
			request.Body = original
			called := false
			r := gin.New()
			r.Use(MCPMetadataLogger())
			r.POST("/v1/mcp", func(c *gin.Context) {
				called = true
				if got := c.Request.Context().Value(mcpContextKey{}); got != "request-context" {
					t.Errorf("request context value = %v, want preserved value", got)
				}
				forwarded, err := io.ReadAll(c.Request.Body)
				if err != nil {
					t.Errorf("read forwarded body: %v", err)
				}
				if !bytes.Equal(forwarded, body) {
					t.Errorf("forwarded body differs from original (%d vs %d bytes)", len(forwarded), len(body))
				}
				c.Status(http.StatusOK)
			})
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if called != tt.wantDownstream {
				t.Fatalf("downstream called = %t, want %t", called, tt.wantDownstream)
			}
			if !original.closed {
				t.Fatal("original request body was not closed")
			}
		})
	}
}

type mcpContextKey struct{}

type trackedRequestBody struct {
	reader io.Reader
	closed bool
}

func (body *trackedRequestBody) Read(p []byte) (int, error) { return body.reader.Read(p) }
func (body *trackedRequestBody) Close() error               { body.closed = true; return nil }

type failingRequestBody struct{ closed bool }

func (*failingRequestBody) Read([]byte) (int, error) {
	return 0, errors.New("synthetic body read failure")
}
func (body *failingRequestBody) Close() error { body.closed = true; return nil }

func TestMCPMetadataLoggerRejectsBodyReadErrorInsteadOfForwardingEmptyBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	called := false
	r := gin.New()
	r.Use(MCPMetadataLogger())
	r.POST("/v1/mcp", func(c *gin.Context) {
		called = true
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
	body := &failingRequestBody{}
	req.Body = body
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a failed body read", w.Code)
	}
	if called {
		t.Fatal("downstream handler ran after the body read failed")
	}
	if !body.closed {
		t.Fatal("original request body was not closed after a read error")
	}
}

func TestMCPMetadataLoggerHonorsShorterServerReadTimeoutWithoutTimingOutResponseStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const readTimeout = 100 * time.Millisecond
	var downstreamCalls int
	r := gin.New()
	r.Use(MCPMetadataLogger())
	r.POST("/v1/mcp", func(c *gin.Context) {
		downstreamCalls++
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.Status(http.StatusOK)
	})
	r.GET("/stream", func(c *gin.Context) {
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write([]byte("first\n"))
		c.Writer.Flush()
		time.Sleep(2 * readTimeout)
		_, _ = c.Writer.Write([]byte("second\n"))
	})

	server := httptest.NewUnstartedServer(r)
	server.Config.ReadTimeout = readTimeout
	server.Start()
	t.Cleanup(server.Close)

	// A partial request body remains open past the server's request-read bound.
	pipeReader, pipeWriter := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/mcp", pipeReader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = 32
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = pipeWriter.Write([]byte("{"))
		time.Sleep(3 * readTimeout)
		_ = pipeWriter.Close()
	}()
	started := time.Now()
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("read-timeout request failed before receiving an HTTP rejection: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded body-read response took %s; want a prompt timeout", elapsed)
	}
	<-writeDone
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("partial-body status = %d, want 408", response.StatusCode)
	}
	if downstreamCalls != 0 {
		t.Fatalf("downstream handler called %d times after timed-out body read", downstreamCalls)
	}

	// ReadTimeout applies to request reading, not response writing: an MCP
	// response stream can legitimately outlive the request-read window.
	streamResponse, err := server.Client().Get(server.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := io.ReadAll(streamResponse.Body)
	_ = streamResponse.Body.Close()
	if err != nil {
		t.Fatalf("response stream failed after the read timeout: %v", err)
	}
	if string(stream) != "first\nsecond\n" {
		t.Fatalf("stream body = %q, want both chunks", stream)
	}
}

func TestMCPMetadataLoggerEnforcesItsReadDeadlineWithoutServerReadTimeout(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	called := false
	r := gin.New()
	r.Use(MCPMetadataLogger())
	r.POST("/v1/mcp", func(c *gin.Context) {
		called = true
		c.Status(http.StatusNoContent)
	})
	server := httptest.NewUnstartedServer(r)
	if server.Config.ReadTimeout != 0 {
		t.Fatalf("test server unexpectedly configures ReadTimeout=%s", server.Config.ReadTimeout)
	}
	server.Start()
	t.Cleanup(server.Close)

	pipeReader, pipeWriter := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/mcp", pipeReader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = 32
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := pipeWriter.Write([]byte("{"))
		writeDone <- writeErr
	}()
	started := time.Now()
	client := &http.Client{Timeout: 14 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		_ = pipeWriter.Close()
		t.Fatalf("request without server ReadTimeout did not receive an HTTP rejection: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	_ = pipeWriter.Close()
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("request body writer remained blocked after the timeout response")
	}

	elapsed := time.Since(started)
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", response.StatusCode)
	}
	if elapsed < 8*time.Second || elapsed > 13*time.Second {
		t.Fatalf("middleware deadline response took %s; want its approximately 10s bound", elapsed)
	}
	if called {
		t.Fatal("downstream handler ran after middleware's own body-read deadline")
	}
}

func TestMCPMetadataLoggerClearsDeadlineForKeepaliveAfterItWouldHaveExpired(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MCPMetadataLogger())
	r.POST("/v1/mcp", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/v1/mcp/stream", func(c *gin.Context) {
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write([]byte("first\n"))
		c.Writer.Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = c.Writer.Write([]byte("second\n"))
	})
	server := httptest.NewUnstartedServer(r)
	if server.Config.ReadTimeout != 0 {
		t.Fatalf("test server unexpectedly configures ReadTimeout=%s", server.Config.ReadTimeout)
	}
	server.Start()
	t.Cleanup(server.Close)
	connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	reader := bufio.NewReader(connection)
	body := `{"method":"tools/list"}`
	if _, err := fmt.Fprintf(connection, "POST /v1/mcp HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n%s", server.Listener.Addr(), len(body), body); err != nil {
		t.Fatalf("write valid MCP request: %v", err)
	}
	firstResponse, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read valid MCP response: %v", err)
	}
	_, _ = io.Copy(io.Discard, firstResponse.Body)
	_ = firstResponse.Body.Close()
	if firstResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("valid MCP status = %d, want 204", firstResponse.StatusCode)
	}

	// The deadline set while reading the first request would now be expired if
	// middleware failed to clear it before returning the accepted response.
	time.Sleep(mcpBodyReadTimeout + 250*time.Millisecond)
	if _, err := fmt.Fprintf(connection, "GET /v1/mcp/stream HTTP/1.1\r\nHost: %s\r\n\r\n", server.Listener.Addr()); err != nil {
		t.Fatalf("write keepalive request after deadline expiry: %v", err)
	}
	streamResponse, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read keepalive response after deadline expiry: %v", err)
	}
	stream, err := io.ReadAll(streamResponse.Body)
	_ = streamResponse.Body.Close()
	if err != nil {
		t.Fatalf("read streamed keepalive response: %v", err)
	}
	if streamResponse.StatusCode != http.StatusOK || string(stream) != "first\nsecond\n" {
		t.Fatalf("stream response status=%d body=%q", streamResponse.StatusCode, stream)
	}
	if streamResponse.Close {
		t.Fatal("server closed the keepalive connection after the streamed response")
	}
}

func TestMCPMetadataLoggerSanitizesAndBoundsLoggedMethods(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		body        string
		path        string
		remoteAddr  string
		wantUnicode bool
	}{
		{name: "line break and terminal escape in method", body: `{"method":"tools/list\n\u001b[31mred"}`},
		{name: "very large method", body: fmt.Sprintf(`{"method":%q}`, strings.Repeat("m", 32<<10))},
		{name: "large batch", body: largeMCPBatch()},
		{
			name:        "untrusted path and client address with unicode",
			body:        `{"method":"工具/列出\u200b"}`,
			path:        "/v1/mcp/路径\n\x1b[31m" + strings.Repeat("猫", 512),
			remoteAddr:  "客户端\n\x1b[31m" + strings.Repeat("猫", 256),
			wantUnicode: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			t.Cleanup(func() { log.SetOutput(previous) })

			r := gin.New()
			r.Use(MCPMetadataLogger())
			r.POST("/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			request := httptest.NewRequest(http.MethodPost, "/v1/mcp", strings.NewReader(tt.body))
			if tt.path != "" {
				request.URL.Path = tt.path
			}
			if tt.remoteAddr != "" {
				request.RemoteAddr = tt.remoteAddr
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, request)

			line := strings.TrimSuffix(output.String(), "\n")
			if strings.ContainsAny(line, "\r\n\x1b") {
				t.Fatalf("logged method contains line/terminal controls: %q", line)
			}
			if len(line) > 2048 {
				t.Fatalf("metadata log line has %d bytes, want at most 2048", len(line))
			}
			if !utf8.ValidString(line) {
				t.Fatalf("metadata log line is not valid UTF-8: %q", line)
			}
			if tt.wantUnicode && !strings.Contains(line, "工具/列出") {
				t.Fatalf("safe Unicode method characters were lost: %q", line)
			}
		})
	}
}

func largeMCPBatch() string {
	methods := make([]string, 128)
	for i := range methods {
		methods[i] = fmt.Sprintf(`{"method":%q}`, fmt.Sprintf("method-%03d-%s", i, strings.Repeat("x", 256)))
	}
	return "[" + strings.Join(methods, ",") + "]"
}

func TestExtractMCPMethodsIgnoresUnknownShapes(t *testing.T) {
	if got := extractMCPMethods([]byte(`{"jsonrpc":"2.0"}`)); got != "" {
		t.Fatalf("extractMCPMethods without method = %q, want empty", got)
	}

	if got := extractMCPMethods([]byte(`[{"foo":"bar"}]`)); got != "" {
		t.Fatalf("extractMCPMethods unknown batch = %q, want empty", got)
	}
}
