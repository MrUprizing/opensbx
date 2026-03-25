package logging

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

// Setup configures stdlib and Gin log writers to also append into logFilePath.
func Setup(logFilePath string) (io.Closer, error) {
	dir := filepath.Dir(logFilePath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	logWriter := io.MultiWriter(os.Stdout, f)
	errWriter := io.MultiWriter(os.Stderr, f)

	log.SetOutput(logWriter)
	gin.DefaultWriter = logWriter
	gin.DefaultErrorWriter = errWriter

	return f, nil
}
