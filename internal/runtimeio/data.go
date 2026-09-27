// Package runtimeio is the private adapter boundary for native execution data.
// These records are not HTTP DTOs and are never returned by the application port.
package runtimeio

import "time"

type ResourceLimits struct {
	Memory int64
	CPUs   float64
}
type CreateSandboxRequest struct {
	Image     string
	Ports     []string
	Timeout   int
	Resources *ResourceLimits
	Env       []string
}
type CreateSandboxResponse struct {
	ID, Name string
	Ports    []string
}
type SandboxSummary struct {
	ID, Name, Image, Status, State string
	Ports                          []string
	ExpiresAt                      *time.Time
}
type SandboxDetail struct {
	ID, Name, Image, Status string
	Running                 bool
	Ports                   []string
	Resources               ResourceLimits
	StartedAt, FinishedAt   string
	ExpiresAt               *time.Time
}
type RestartResponse struct {
	Status    string
	Ports     []string
	ExpiresAt *time.Time
}
type SandboxNetwork struct {
	MainPort string
	PortsMap map[string]string
}
type ExecCommandRequest struct {
	Command string
	Args    []string
	Cwd     string
	Env     map[string]string
}
type CommandDetail struct {
	ID, Name       string
	Args           []string
	Cwd, SandboxID string
	ExitCode       *int
	StartedAt      int64
	FinishedAt     *int64
}
type CommandLogsResponse struct {
	Stdout, Stderr string
	ExitCode       *int
}
type SandboxStats struct {
	CPU    float64
	Memory MemoryUsage
	PIDs   uint64
}
type MemoryUsage struct {
	Usage, Limit uint64
	Percent      float64
}
