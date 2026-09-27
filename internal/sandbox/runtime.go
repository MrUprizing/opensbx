package sandbox

import (
	"context"
	"io"
	"time"
)

type CommandID string
type ImageID string
type CreateOptions struct {
	Image     string
	Ports     []Port
	Timeout   time.Duration
	Resources ResourceLimits
	Env       []string
}
type RunOptions struct {
	ID        SandboxID
	Image     PreparedImage
	Ports     []Port
	Timeout   time.Duration
	Resources ResourceLimits
	Env       []string
}
type Created struct {
	ID    SandboxID
	Name  string
	Ports []Port
	URL   string
}
type Summary struct {
	ID            SandboxID
	Name          string
	Image         ImageID
	Status, State string
	Ports         []Port
	ExpiresAt     *time.Time
	URL           string
}
type Detail struct {
	Summary
	Running               bool
	Resources             ResourceLimits
	StartedAt, FinishedAt string
}
type Started struct {
	Status    string
	Ports     []Port
	ExpiresAt *time.Time
}
type PublishedPort struct {
	Guest Port
	Host  uint16
}
type Network struct {
	Main  Port
	Ports []PublishedPort
}
type Route struct {
	Running bool
	Network Network
}
type ProcessRequest struct {
	Command string
	Args    []string
	Cwd     string
	Env     map[string]string
}
type Command struct {
	ID         CommandID
	SandboxID  SandboxID
	Name       string
	Args       []string
	Cwd        string
	ExitCode   *int
	StartedAt  int64
	FinishedAt *int64
}
type Logs struct {
	Stdout, Stderr string
	ExitCode       *int
}
type MemoryUsage struct {
	Usage, Limit uint64
	Percent      float64
}
type Stats struct {
	CPU    float64
	Memory MemoryUsage
	PIDs   uint64
}
type ImageSummary struct {
	ID   ImageID
	Tags []string
	Size int64
}
type ImageDetail struct {
	ImageSummary
	Created, Architecture, OS string
}
type Provenance struct{ Root, Manifest, CacheVersion string }

// Provisioned is a bounded creation transaction. Its native reference remains
// inside the adapter; compensation cannot be redirected to an arbitrary ID.
type Provisioned interface {
	Sandbox() Created
	Adopt(context.Context, Provenance) error
	Rollback(context.Context) error
	Recover(context.Context, Provenance, error) error
}
type PreparedImage interface{ Identity() Provenance }

type Runtime interface {
	Process
	Filesystem
	Ping(context.Context) error
	Create(context.Context, RunOptions) (Provisioned, error)
	List(context.Context) ([]Summary, error)
	Inspect(context.Context, SandboxID) (Detail, error)
	Start(context.Context, SandboxID) (Started, error)
	Stop(context.Context, SandboxID) error
	Restart(context.Context, SandboxID) (Started, error)
	GetNetwork(context.Context, SandboxID) (Network, error)
	Routing(context.Context, SandboxID) (Route, error)
	Remove(context.Context, SandboxID) error
	Pause(context.Context, SandboxID) error
	Resume(context.Context, SandboxID) error
	RenewExpiration(context.Context, SandboxID, time.Duration) error
	Stats(context.Context, SandboxID) (Stats, error)
}
type Process interface {
	ExecCommand(context.Context, SandboxID, ProcessRequest) (Command, error)
	GetCommand(context.Context, SandboxID, CommandID) (Command, error)
	ListCommands(context.Context, SandboxID) ([]Command, error)
	KillCommand(context.Context, SandboxID, CommandID, int) (Command, error)
	WaitCommand(context.Context, SandboxID, CommandID) (Command, error)
	StreamCommandLogs(context.Context, SandboxID, CommandID) (io.ReadCloser, io.ReadCloser, error)
	GetCommandLogs(context.Context, SandboxID, CommandID) (Logs, error)
}
type Filesystem interface {
	ReadFile(context.Context, SandboxID, string) (string, error)
	WriteFile(context.Context, SandboxID, string, string) error
	DeleteFile(context.Context, SandboxID, string) error
	ListDir(context.Context, SandboxID, string) (string, error)
}

// Application has no native-reference or provisioning-handle surface.
type Application interface {
	Process
	Filesystem
	Ping(context.Context) error
	Create(context.Context, CreateOptions) (Created, error)
	List(context.Context) ([]Summary, error)
	Inspect(context.Context, SandboxID) (Detail, error)
	Start(context.Context, SandboxID) (Started, error)
	Stop(context.Context, SandboxID) error
	Restart(context.Context, SandboxID) (Started, error)
	GetNetwork(context.Context, SandboxID) (Network, error)
	Remove(context.Context, SandboxID) error
	Pause(context.Context, SandboxID) error
	Resume(context.Context, SandboxID) error
	RenewExpiration(context.Context, SandboxID, time.Duration) error
	Stats(context.Context, SandboxID) (Stats, error)
	PullImage(context.Context, string) error
	RemoveImage(context.Context, string, bool) error
	InspectImage(context.Context, string) (ImageDetail, error)
	ListImages(context.Context) ([]ImageSummary, error)
}
