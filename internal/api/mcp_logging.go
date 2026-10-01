package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
)

const (
	mcpMaxBodyBytes    = 1 << 20
	mcpBodyReadTimeout = 10 * time.Second
	mcpMethodLogBytes  = 1024
)

// MCPMetadataLogger bounds request reading before the SDK and logs safe metadata.
func MCPMetadataLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		method := "-"
		defer func() {
			log.Printf(
				"mcp request method=%s path=%s status=%d duration=%s ip=%s",
				method,
				safeMCPMetadata(c.Request.URL.Path, 512),
				c.Writer.Status(),
				time.Since(started).Round(time.Millisecond),
				safeMCPMetadata(c.ClientIP(), 128),
			)
		}()

		if c.Request.Body != nil {
			body, err := readMCPBody(c)
			if err != nil {
				status := http.StatusBadRequest
				var tooLarge *http.MaxBytesError
				var timeout net.Error
				switch {
				case errors.As(err, &tooLarge):
					status = http.StatusRequestEntityTooLarge
				case errors.As(err, &timeout) && timeout.Timeout():
					status = http.StatusRequestTimeout
				}
				// Do not reuse an HTTP/1 connection with an unread/rejected body.
				c.Header("Connection", "close")
				c.AbortWithStatusJSON(status, gin.H{"error": "MCP request body could not be read within limits"})
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			if m := extractMCPMethods(body); m != "" {
				method = m
			}
		}

		c.Next()
	}
}

func readMCPBody(c *gin.Context) (body []byte, err error) {
	controller := http.NewResponseController(c.Writer)
	readTimeout := mcpBodyReadTimeout
	// Respect a host server's shorter read policy rather than replacing it
	// with our production default. The shared API/proxy server has none.
	if server, ok := c.Request.Context().Value(http.ServerContextKey).(*http.Server); ok && server.ReadTimeout > 0 && server.ReadTimeout < readTimeout {
		readTimeout = server.ReadTimeout
	}
	deadlineErr := controller.SetReadDeadline(time.Now().Add(readTimeout))
	// Recorders and non-network writers may not support deadlines. Gin's
	// production writer unwraps to net/http, which does support them.
	if deadlineErr != nil && !errors.Is(deadlineErr, http.ErrNotSupported) {
		_ = c.Request.Body.Close()
		return nil, deadlineErr
	}
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, mcpMaxBodyBytes)
	defer func() {
		if err != nil && deadlineErr == nil {
			// Prevent Close from draining a slow, incomplete rejected body.
			_ = controller.SetReadDeadline(time.Now())
		}
		_ = limited.Close()
		if deadlineErr == nil {
			if resetErr := controller.SetReadDeadline(time.Time{}); err == nil {
				err = resetErr
			}
		}
	}()
	return io.ReadAll(limited)
}

// Keep metadata single-line, terminal-safe and UTF-8 valid, including when truncated.
func safeMCPMetadata(value string, maxBytes int) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			r = '_'
		}
		if out.Len()+len(string(r)) > maxBytes-3 {
			out.WriteString("...")
			return out.String()
		}
		out.WriteRune(r)
	}
	return out.String()
}

func extractMCPMethods(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}

	type request struct {
		Method string `json:"method"`
	}

	methods := make([]string, 0, 2)
	remaining := mcpMethodLogBytes
	seen := make(map[string]struct{})
	appendMethod := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || remaining < 4 {
			return
		}
		v = safeMCPMetadata(v, remaining)
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		methods = append(methods, v)
		remaining -= len(v) + 1 // Reserve the next comma as part of the aggregate budget.
	}

	switch trimmed[0] {
	case '{':
		var req request
		if err := json.Unmarshal(trimmed, &req); err == nil {
			appendMethod(req.Method)
		}
	case '[':
		var reqs []request
		if err := json.Unmarshal(trimmed, &reqs); err == nil {
			for _, req := range reqs {
				appendMethod(req.Method)
			}
		}
	}

	return strings.Join(methods, ",")
}
