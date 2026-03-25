package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// MCPMetadataLogger logs request metadata for /v1/mcp endpoints.
func MCPMetadataLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		method := "-"

		if c.Request.Body != nil {
			body, err := io.ReadAll(c.Request.Body)
			if err == nil {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
				if m := extractMCPMethods(body); m != "" {
					method = m
				}
			} else {
				c.Request.Body = io.NopCloser(bytes.NewReader(nil))
			}
		}

		c.Next()

		log.Printf(
			"mcp request method=%s path=%s status=%d duration=%s ip=%s",
			method,
			c.Request.URL.Path,
			c.Writer.Status(),
			time.Since(started).Round(time.Millisecond),
			c.ClientIP(),
		)
	}
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
	seen := make(map[string]struct{})
	appendMethod := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		methods = append(methods, v)
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
