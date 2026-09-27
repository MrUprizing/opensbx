package runtimeio

import (
	"context"
	"io"
	"opensbx/internal/sandbox"
)

// Engine is adapter-internal. Native references cannot cross the domain port.
type Engine interface {
	Ping(context.Context) error
	Create(context.Context, CreateSandboxRequest) (CreateSandboxResponse, error)
	DiscardCreated(context.Context, string) error
	List(context.Context) ([]SandboxSummary, error)
	Inspect(context.Context, string) (SandboxDetail, error)
	Start(context.Context, string) (RestartResponse, error)
	Stop(context.Context, string) error
	Restart(context.Context, string) (RestartResponse, error)
	GetNetwork(context.Context, string) (SandboxNetwork, error)
	Routing(context.Context, string) (RoutingState, error)
	Remove(context.Context, string) error
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	RenewExpiration(context.Context, string, int) error
	Stats(context.Context, string) (SandboxStats, error)
	ExecCommand(context.Context, string, ExecCommandRequest) (CommandDetail, error)
	GetCommand(context.Context, string, string) (CommandDetail, error)
	ListCommands(context.Context, string) ([]CommandDetail, error)
	KillCommand(context.Context, string, string, int) (CommandDetail, error)
	WaitCommand(context.Context, string, string) (CommandDetail, error)
	StreamCommandLogs(context.Context, string, string) (io.ReadCloser, io.ReadCloser, error)
	GetCommandLogs(context.Context, string, string) (CommandLogsResponse, error)
	ReadFile(context.Context, string, string) (string, error)
	WriteFile(context.Context, string, string, string) error
	DeleteFile(context.Context, string, string) error
	ListDir(context.Context, string, string) (string, error)
}
type NativeCache interface {
	Capabilities(context.Context) (sandbox.Capabilities, error)
	Materialize(context.Context, sandbox.Image) (string, error)
}
type RoutingState struct {
	Running bool
	Network SandboxNetwork
}
