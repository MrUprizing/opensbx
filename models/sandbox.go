package models

import "time"

// ResourceLimits defines CPU and memory constraints for a sandbox.
type ResourceLimits struct {
	Memory int64   `json:"memory" example:"1024"` // memory limit in MB (e.g. 512 = 512MB). Default: 1024 (1GB), Max: 8192 (8GB)
	CPUs   float64 `json:"cpus" example:"1.0"`    // fractional CPU limit (e.g. 1.5). Default: 1.0, Max: 4.0
}

// CreateSandboxRequest is the body for POST /v1/sandboxes
type CreateSandboxRequest struct {
	Image     string          `json:"image" binding:"required" example:"node:24"`
	Ports     []string        `json:"ports" example:"3000,8080"` // container ports to expose, e.g. ["3000", "8080/tcp"]. First port is the default for proxy routing.
	Timeout   int             `json:"timeout" example:"900"`     // seconds until auto-stop, 0 = default (900s)
	Resources *ResourceLimits `json:"resources"`                 // CPU/memory limits, nil = defaults (1GB RAM, 1 vCPU)
	Env       []string        `json:"env"`                       // extra environment variables (e.g. ["KEY=VALUE"])
}

// CreateSandboxResponse is the response for POST /v1/sandboxes
type CreateSandboxResponse struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`          // auto-generated name (e.g. "eager-turing")
	Ports []string `json:"ports"`         // exposed container ports, e.g. ["3000/tcp", "8080/tcp"]
	URL   string   `json:"url,omitempty"` // proxy URL, e.g. "http://eager-turing.localhost"
}

// SandboxSummary is a concise view of a sandbox for list endpoints.
type SandboxSummary struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Image     string     `json:"image"`
	Status    string     `json:"status"`
	State     string     `json:"state"`
	Ports     []string   `json:"ports"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	URL       string     `json:"url,omitempty"`
}

// SandboxDetail is the full inspect response with only relevant fields.
type SandboxDetail struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Image      string         `json:"image"`
	Status     string         `json:"status"`
	Running    bool           `json:"running"`
	Ports      []string       `json:"ports"`
	Resources  ResourceLimits `json:"resources"`
	StartedAt  string         `json:"started_at"`
	FinishedAt string         `json:"finished_at"`
	ExpiresAt  *time.Time     `json:"expires_at,omitempty"`
	URL        string         `json:"url,omitempty"`
}

// RestartResponse is the response for POST /v1/sandboxes/:id/restart
type RestartResponse struct {
	Status    string     `json:"status"`
	Ports     []string   `json:"ports"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// SandboxNetwork is the network/routing view for a sandbox.
type SandboxNetwork struct {
	MainPort string            `json:"main_port"` // selected container port for proxy routing (e.g. "3000/tcp")
	PortsMap map[string]string `json:"ports_map"` // map of container port -> docker host port
}

// ExecCommandRequest is the body for POST /v1/sandboxes/:id/cmd
type ExecCommandRequest struct {
	Command string            `json:"command" binding:"required" example:"npm"` // executable name (e.g. "npm")
	Args    []string          `json:"args" example:"install"`                   // arguments (e.g. ["install"])
	Cwd     string            `json:"cwd" example:"/app"`                       // working directory
	Env     map[string]string `json:"env"`                                      // extra environment variables
}

// CommandDetail represents a command executed in a sandbox.
type CommandDetail struct {
	ID         string   `json:"id"`                    // cmd_<hex>
	Name       string   `json:"name"`                  // executable name
	Args       []string `json:"args"`                  // arguments
	Cwd        string   `json:"cwd"`                   // working directory
	SandboxID  string   `json:"sandbox_id"`            // parent sandbox container ID
	ExitCode   *int     `json:"exit_code,omitempty"`   // nil while running
	StartedAt  int64    `json:"started_at"`            // unix milliseconds
	FinishedAt *int64   `json:"finished_at,omitempty"` // unix milliseconds, nil while running
}

// CommandResponse wraps a single command.
type CommandResponse struct {
	Command CommandDetail `json:"command"`
}

// CommandListResponse wraps a list of commands.
type CommandListResponse struct {
	Commands []CommandDetail `json:"commands"`
}

// CommandLogsResponse is the response for GET /v1/sandboxes/:id/cmd/:cmdId/logs (non-stream).
type CommandLogsResponse struct {
	Stdout   string `json:"stdout"`              // captured stdout text
	Stderr   string `json:"stderr"`              // captured stderr text
	ExitCode *int   `json:"exit_code,omitempty"` // nil while command is still running
}

// KillCommandRequest is the body for POST /v1/sandboxes/:id/cmd/:cmdId/kill
type KillCommandRequest struct {
	Signal int `json:"signal" binding:"required" example:"15"` // POSIX signal number (15=SIGTERM, 9=SIGKILL)
}

// FileReadResponse is the response for GET /v1/sandboxes/:id/files
type FileReadResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// FileWriteRequest is the body for PUT /v1/sandboxes/:id/files
type FileWriteRequest struct {
	Content string `json:"content" binding:"required" example:"console.log('hello')"`
}

// FileListResponse is the response for GET /v1/sandboxes/:id/files/list
type FileListResponse struct {
	Path   string `json:"path"`
	Output string `json:"output"`
}

// RenewExpirationRequest is the body for POST /v1/sandboxes/:id/renew-expiration
type RenewExpirationRequest struct {
	Timeout int `json:"timeout" binding:"required" example:"900"` // new TTL in seconds
}

// RenewExpirationResponse is the response for POST /v1/sandboxes/:id/renew-expiration
type RenewExpirationResponse struct {
	Status  string `json:"status"`
	Timeout int    `json:"timeout"`
}

// ImagePullRequest is the body for POST /v1/images/pull
type ImagePullRequest struct {
	Image string `json:"image" binding:"required" example:"node:22"` // image name with optional tag (e.g. "nginx:latest")
}

// ImagePullResponse is the response for POST /v1/images/pull
type ImagePullResponse struct {
	Status string `json:"status"`
	Image  string `json:"image"`
}

// SandboxStats is a curated snapshot of container resource usage.
type SandboxStats struct {
	CPU    float64     `json:"cpu_percent"` // CPU usage percentage
	Memory MemoryUsage `json:"memory"`      // memory usage and limit
	PIDs   uint64      `json:"pids"`        // number of running processes
}

// MemoryUsage holds memory consumption details.
type MemoryUsage struct {
	Usage   uint64  `json:"usage"`   // bytes currently used
	Limit   uint64  `json:"limit"`   // bytes limit
	Percent float64 `json:"percent"` // usage / limit * 100
}

// ImageDetail is the inspect response for a single Docker image.
type ImageDetail struct {
	ID           string   `json:"id"`
	Tags         []string `json:"tags"`
	Size         int64    `json:"size"`         // bytes
	Created      string   `json:"created"`      // RFC3339
	Architecture string   `json:"architecture"` // e.g. "amd64"
	OS           string   `json:"os"`           // e.g. "linux"
}

// ImageSummary is a concise view of a local Docker image.
type ImageSummary struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	Size int64    `json:"size"` // bytes
}
