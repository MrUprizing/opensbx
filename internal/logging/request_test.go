package logging

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"opensbx/internal/api"

	"github.com/gin-gonic/gin"
)

func TestRequestLogFormatterEscapesAndBoundsAllClientControlledFields(t *testing.T) {
	params := gin.LogFormatterParams{
		TimeStamp:    time.Date(2026, time.October, 1, 10, 0, 0, 0, time.UTC),
		StatusCode:   http.StatusUnauthorized,
		Latency:      1234 * time.Millisecond,
		BodySize:     73,
		Method:       "GET\r\n\x1b[31m" + strings.Repeat("猫", 512),
		Path:         "/v1/mcp/路径?next=secret\r\n\x1b[2J" + strings.Repeat("界", 4096),
		ClientIP:     "客户端\r\n\x1b[31m" + strings.Repeat("猫", 512),
		ErrorMessage: "unauthorized\r\n\x1b[2K" + strings.Repeat("错误", 2048),
	}
	line := RequestLogFormatter(params)
	if len(line) > maxRequestLogBytes {
		t.Fatalf("formatted log line is %d bytes, want at most %d", len(line), maxRequestLogBytes)
	}
	if !utf8.ValidString(line) {
		t.Fatalf("formatted log line is invalid UTF-8: %q", line)
	}
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 || strings.Contains(line, "\r") {
		t.Fatalf("formatted log line must contain only its final newline: %q", line)
	}
	if strings.ContainsRune(line, '\x1b') {
		t.Fatalf("formatted log line contains an ANSI escape: %q", line)
	}
	for _, expected := range []string{"status=401", "duration=1.234s", "next=secret", "路径", "客户端"} {
		if !strings.Contains(line, expected) {
			t.Errorf("formatted log line lost %q: %q", expected, line)
		}
	}
}

func TestRequestLoggerProtectsUnauthenticatedMCPRouteLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousWriter := gin.DefaultWriter
	previousErrorWriter := gin.DefaultErrorWriter
	var output bytes.Buffer
	gin.DefaultWriter = &output
	gin.DefaultErrorWriter = &output
	gin.ForceConsoleColor()
	t.Cleanup(func() {
		gin.DefaultWriter = previousWriter
		gin.DefaultErrorWriter = previousErrorWriter
		gin.DisableConsoleColor()
	})

	r := gin.New()
	r.Use(RequestLogger(), gin.Recovery())
	v1 := r.Group("/v1")
	v1.Use(api.APIKeyAuth("test-secret"))
	mcp := v1.Group("")
	mcp.Use(api.MCPMetadataLogger())
	mcp.Any("/mcp", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	mcp.Any("/mcp/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	target := server.URL + "/v1/mcp/%0d%0a%1b%5b31mowned?next=%0d%0a%1b%5b2J"
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("unauthenticated MCP request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}

	line := output.String()
	if len(line) > maxRequestLogBytes {
		t.Fatalf("access log line is %d bytes, want at most %d", len(line), maxRequestLogBytes)
	}
	if !utf8.ValidString(line) {
		t.Fatalf("access log line is invalid UTF-8: %q", line)
	}
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 || strings.Contains(line, "\r") {
		t.Fatalf("access logger emitted more than one physical line: %q", line)
	}
	if strings.ContainsRune(line, '\x1b') {
		t.Fatalf("access logger emitted an ANSI escape despite forced console colors: %q", line)
	}
	if !strings.Contains(line, "status=401") || !strings.Contains(line, "next=") {
		t.Fatalf("access log omitted status or encoded query metadata: %q", line)
	}
}
