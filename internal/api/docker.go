package api

import (
	"context"
	"io"

	"opensbx/models"
)

// DockerClient defines the sandbox operations used by the API handlers.
type DockerClient interface {
	Ping(ctx context.Context) error
	List(ctx context.Context) ([]models.SandboxSummary, error)
	Create(ctx context.Context, req models.CreateSandboxRequest) (models.CreateSandboxResponse, error)
	Inspect(ctx context.Context, id string) (models.SandboxDetail, error)
	Start(ctx context.Context, id string) (models.RestartResponse, error)
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) (models.RestartResponse, error)
	Remove(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	RenewExpiration(ctx context.Context, id string, timeout int) error
	ExecCommand(ctx context.Context, sandboxID string, req models.ExecCommandRequest) (models.CommandDetail, error)
	GetCommand(ctx context.Context, sandboxID, cmdID string) (models.CommandDetail, error)
	ListCommands(ctx context.Context, sandboxID string) ([]models.CommandDetail, error)
	KillCommand(ctx context.Context, sandboxID, cmdID string, signal int) (models.CommandDetail, error)
	StreamCommandLogs(ctx context.Context, sandboxID, cmdID string) (io.ReadCloser, io.ReadCloser, error)
	GetCommandLogs(ctx context.Context, sandboxID, cmdID string) (models.CommandLogsResponse, error)
	WaitCommand(ctx context.Context, sandboxID, cmdID string) (models.CommandDetail, error)
	Stats(ctx context.Context, id string) (models.SandboxStats, error)
	ReadFile(ctx context.Context, id, path string) (string, error)
	WriteFile(ctx context.Context, id, path, content string) error
	DeleteFile(ctx context.Context, id, path string) error
	ListDir(ctx context.Context, id, path string) (string, error)
	PullImage(ctx context.Context, image string) error
	RemoveImage(ctx context.Context, id string, force bool) error
	InspectImage(ctx context.Context, id string) (models.ImageDetail, error)
	ListImages(ctx context.Context) ([]models.ImageSummary, error)
}
