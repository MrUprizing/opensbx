package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"opensbx/internal/docker"
	"opensbx/models"
)

func (c *Client) running(ctx context.Context, id string) error {
	info, err := c.lookup(ctx, id)
	if err != nil {
		return err
	}
	if info.Status.State != "running" {
		return docker.ErrNotRunning
	}
	return nil
}
func validGuestPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) {
		return errors.New("guest path must be nonempty and contain no NUL")
	}
	return nil
}

func (c *Client) file(ctx context.Context, id, path, script, content string) (string, error) {
	if err := validGuestPath(path); err != nil {
		return "", err
	}
	if err := c.running(ctx, id); err != nil {
		return "", err
	}
	// Only the fixed script is shell source, and only inside the guest. A path
	// beginning with '-' is made explicitly relative by the fixed script.
	args := []string{"exec", "--interactive", id, "/bin/sh", "-c", `case "$1" in /*) ;; *) set -- "./$1";; esac; ` + script, "opensbx-file", path}
	b, err := c.run(ctx, strings.NewReader(content), args...)
	return string(b), err
}
func (c *Client) ReadFile(ctx context.Context, id, path string) (string, error) {
	return c.file(ctx, id, path, `exec cat "$1"`, "")
}
func (c *Client) WriteFile(ctx context.Context, id, path, content string) error {
	if len(content) > outputLimit {
		return errors.New("file content exceeds 4 MiB")
	}
	_, err := c.file(ctx, id, path, `mkdir -p "$(dirname "$1")" && cat > "$1"`, content)
	return err
}
func (c *Client) DeleteFile(ctx context.Context, id, path string) error {
	_, err := c.file(ctx, id, path, `exec rm -rf -- "$1"`, "")
	return err
}
func (c *Client) ListDir(ctx context.Context, id, path string) (string, error) {
	return c.file(ctx, id, path, `exec ls -la "$1"`, "")
}

type statsSample struct {
	ID     string  `json:"id"`
	CPU    *uint64 `json:"cpuUsageUsec"`
	Memory *uint64 `json:"memoryUsageBytes"`
	Limit  *uint64 `json:"memoryLimitBytes"`
	PIDs   *uint64 `json:"numProcesses"`
}

func (c *Client) sample(ctx context.Context, id string) (statsSample, time.Time, error) {
	b, err := c.run(ctx, nil, "stats", "--no-stream", "--format", "json", id)
	at := c.now()
	if err != nil {
		return statsSample{}, at, err
	}
	var samples []statsSample
	if err := json.Unmarshal(b, &samples); err != nil || len(samples) != 1 {
		return statsSample{}, at, errors.New("Apple stats sample is missing or invalid")
	}
	s := samples[0]
	if s.ID != id || s.CPU == nil || s.Memory == nil || s.Limit == nil || s.PIDs == nil || *s.Limit == 0 {
		return statsSample{}, at, errors.New("Apple stats sample is incomplete")
	}
	return s, at, nil
}
func (c *Client) Stats(ctx context.Context, id string) (models.SandboxStats, error) {
	var result models.SandboxStats
	initial, err := c.lookup(ctx, id)
	if err != nil {
		return result, err
	}
	if initial.Status.State != "running" {
		return result, docker.ErrNotRunning
	}
	// Each CLI invocation sleeps internally and only exposes its last sample.
	// Take two independent observations; never present cumulative CPU as a
	// percentage or invent zero for the first/partial sample.
	a, t1, err := c.sample(ctx, id)
	if err != nil {
		return result, err
	}
	b, t2, err := c.sample(ctx, id)
	if err != nil {
		return result, err
	}
	elapsed := t2.Sub(t1).Seconds() * 1e6
	if elapsed <= 0 || *b.CPU < *a.CPU {
		return result, errors.New("Apple CPU counters reset or sampling clock did not advance")
	}
	// Sampling is read-only and holds no lifecycle lock. Reject a result if
	// ownership disappeared or the sandbox stopped/restarted during sampling.
	current, err := c.lookup(ctx, id)
	if err != nil {
		return result, err
	}
	if current.Status.State != "running" {
		return result, docker.ErrNotRunning
	}
	if string(current.Status.StartedDate) != string(initial.Status.StartedDate) {
		return result, errors.New("sandbox restarted during Apple stats sampling")
	}
	result.CPU = float64(*b.CPU-*a.CPU) / elapsed * 100
	result.Memory = models.MemoryUsage{Usage: *b.Memory, Limit: *b.Limit, Percent: float64(*b.Memory) / float64(*b.Limit) * 100}
	result.PIDs = *b.PIDs
	return result, nil
}
