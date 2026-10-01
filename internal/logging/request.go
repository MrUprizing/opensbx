package logging

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"opensbx/internal/terminaltext"

	"github.com/gin-gonic/gin"
)

const maxRequestLogBytes = 2048

// RequestLogger retains Gin's request logging lifecycle and configured writer,
// including requests rejected before route middleware, without terminal colors.
func RequestLogger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(RequestLogFormatter)
}

// RequestLogFormatter returns at most 2048 bytes, including one trailing newline.
// All client-controlled fields are quoted, escaped and independently bounded.
// Gin supplies Path with its raw query appended; Request and Keys are not logged.
func RequestLogFormatter(p gin.LogFormatterParams) string {
	line := fmt.Sprintf("http request time=%s status=%d duration=%s bytes=%d method=%s path=%s ip=%s error=%s",
		p.TimeStamp.Format(time.RFC3339), p.StatusCode, p.Latency.Round(time.Millisecond), p.BodySize,
		requestLogField(p.Method, 96), requestLogField(p.Path, 896),
		requestLogField(p.ClientIP, 160), requestLogField(p.ErrorMessage, 640))
	// Preserve status/duration at the front even if unusually large numeric or
	// timestamp values push the aggregate past its budget.
	if len(line) >= maxRequestLogBytes {
		end := maxRequestLogBytes - 4
		for !utf8.RuneStart(line[end]) {
			end--
		}
		line = line[:end] + "..."
	}
	return line + "\n"
}

func requestLogField(value string, maxBytes int) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		// Reuse the CLI's control/Unicode-format policy, then quote literal
		// backslashes and quotes so input cannot masquerade as another field.
		quoted := strconv.Quote(terminaltext.Escape(string(r), false))
		piece := quoted[1 : len(quoted)-1]
		if out.Len()+len(piece) > maxBytes-4 {
			out.WriteString("...")
			break
		}
		out.WriteString(piece)
	}
	out.WriteByte('"')
	return out.String()
}
