package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"opensbx/internal/docker"
)

// ErrorResponse is the standard error body returned by all API endpoints.
type ErrorResponse struct {
	Code    string `json:"code" example:"BAD_REQUEST"`
	Message string `json:"message" example:"image is required"`
}

// badRequest writes a 400 response with code BAD_REQUEST and the provided message.
func badRequest(c *gin.Context, msg string) {
	c.JSON(http.StatusBadRequest, ErrorResponse{Code: "BAD_REQUEST", Message: msg})
}

// notFound writes a 404 response with code NOT_FOUND for the given resource name.
func notFound(c *gin.Context, resource string) {
	c.JSON(http.StatusNotFound, ErrorResponse{Code: "NOT_FOUND", Message: resource + " not found"})
}

// conflict writes a 409 response with code CONFLICT for state-related errors
// (e.g. starting an already-running sandbox or stopping an already-stopped one).
func conflict(c *gin.Context, msg string) {
	c.JSON(http.StatusConflict, ErrorResponse{Code: "CONFLICT", Message: msg})
}

// requestTimeout writes a 408 response with code TIMEOUT for operations that exceeded their deadline.
func requestTimeout(c *gin.Context, msg string) {
	c.JSON(http.StatusRequestTimeout, ErrorResponse{Code: "TIMEOUT", Message: msg})
}

// rateLimited writes a 429 response with code RATE_LIMITED when the caller exceeds request limits.
func rateLimited(c *gin.Context, msg string) {
	c.JSON(http.StatusTooManyRequests, ErrorResponse{Code: "RATE_LIMITED", Message: msg})
}

// internalError writes a 500 response with code INTERNAL_ERROR.
// It first checks for well-known sentinel errors and downgrades to the appropriate status code.
func internalError(c *gin.Context, err error) {
	if errors.Is(err, docker.ErrNotFound) {
		notFound(c, "sandbox")
		return
	}
	if errors.Is(err, docker.ErrImageNotFound) {
		badRequest(c, "image not found locally, use POST /v1/images/pull to download it first")
		return
	}
	if errors.Is(err, docker.ErrAlreadyRunning) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, docker.ErrAlreadyStopped) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, docker.ErrAlreadyPaused) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, docker.ErrNotPaused) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, docker.ErrNotRunning) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, docker.ErrCommandNotFound) {
		notFound(c, "command")
		return
	}
	if errors.Is(err, docker.ErrCommandFinished) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		requestTimeout(c, "operation timed out")
		return
	}
	c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "INTERNAL_ERROR", Message: err.Error()})
}
