package sandbox

import "errors"

// ErrNotFound is returned when a container does not exist.
var ErrNotFound = errors.New("sandbox not found")

// ErrImageNotFound is returned when an image does not exist locally.
var ErrImageNotFound = errors.New("image not found locally")

// ErrAlreadyRunning is returned when trying to start a sandbox that is already running.
var ErrAlreadyRunning = errors.New("sandbox is already running")

// ErrAlreadyStopped is returned when trying to stop a sandbox that is already stopped.
var ErrAlreadyStopped = errors.New("sandbox is already stopped")

// ErrAlreadyPaused is returned when trying to pause a sandbox that is already paused.
var ErrAlreadyPaused = errors.New("sandbox is already paused")

// ErrNotPaused is returned when trying to resume a sandbox that is not paused.
var ErrNotPaused = errors.New("sandbox is not paused")

// ErrNotRunning is returned when trying to exec/pause on a sandbox that is not running.
var ErrNotRunning = errors.New("sandbox is not running")

// ErrCommandNotFound is returned when a command ID does not exist.
var ErrCommandNotFound = errors.New("command not found")

// ErrCommandFinished is returned when trying to kill a command that has already exited.
var ErrCommandFinished = errors.New("command has already finished")

// ErrLogTruncated means a streaming reader missed overwritten retained output.
var ErrLogTruncated = errors.New("command logs truncated: retained output was overwritten")

var ErrUnsupported = errors.New("runtime capability is unsupported")
var ErrInvalidInput = errors.New("invalid input")
