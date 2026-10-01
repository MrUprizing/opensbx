package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"opensbx/internal/sandbox"
)

// ErrorResponse is the standard error body returned by all API endpoints.
type ErrorResponse struct {
	Code    string `json:"code" example:"BAD_REQUEST"`
	Message string `json:"message" example:"image is required"`
} // @name internal_api.ErrorResponse

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

// internalError writes a 500 response with code INTERNAL_ERROR.
// It first checks for well-known sentinel errors and downgrades to the appropriate status code.
func internalError(c *gin.Context, err error) {
	if errors.Is(err, sandbox.ErrUnsupported) || errors.Is(err, sandbox.ErrInvalidInput) {
		badRequest(c, err.Error())
		return
	}
	if errors.Is(err, sandbox.ErrNotFound) {
		notFound(c, "sandbox")
		return
	}
	if errors.Is(err, sandbox.ErrImageNotFound) {
		badRequest(c, "image not found locally, use POST /v1/images/pull to download it first")
		return
	}
	if errors.Is(err, sandbox.ErrAlreadyRunning) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, sandbox.ErrAlreadyStopped) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, sandbox.ErrAlreadyPaused) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, sandbox.ErrNotPaused) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, sandbox.ErrNotRunning) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, sandbox.ErrCommandNotFound) {
		notFound(c, "command")
		return
	}
	if errors.Is(err, sandbox.ErrCommandFinished) {
		conflict(c, err.Error())
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		requestTimeout(c, "operation timed out")
		return
	}
	c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "INTERNAL_ERROR", Message: err.Error()})
}
