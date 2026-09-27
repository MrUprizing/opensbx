package docker

import "opensbx/internal/sandbox"

// Keep adapter regressions readable while asserting the shared domain errors.
var (
	ErrNotFound        = sandbox.ErrNotFound
	ErrImageNotFound   = sandbox.ErrImageNotFound
	ErrAlreadyRunning  = sandbox.ErrAlreadyRunning
	ErrAlreadyStopped  = sandbox.ErrAlreadyStopped
	ErrAlreadyPaused   = sandbox.ErrAlreadyPaused
	ErrNotPaused       = sandbox.ErrNotPaused
	ErrNotRunning      = sandbox.ErrNotRunning
	ErrCommandNotFound = sandbox.ErrCommandNotFound
	ErrCommandFinished = sandbox.ErrCommandFinished
)
