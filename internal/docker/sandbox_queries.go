package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"opensbx/internal/runtimeio"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

// List returns all sandboxes tracked in the database, enriched with live
// state from Docker. Stopped containers are always included.
func (c *Client) List(ctx context.Context) ([]runtimeio.SandboxSummary, error) {
	// Fetch all persisted sandboxes from the database.
	dbSandboxes, err := c.repo.FindAll()
	if err != nil {
		return nil, err
	}
	if len(dbSandboxes) == 0 {
		return []runtimeio.SandboxSummary{}, nil
	}

	// Fetch all containers (including stopped) to build a lookup map.
	result, err := c.cli.ContainerList(ctx, moby.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}

	type containerInfo struct {
		Name   string
		Image  string
		Status string
		State  string
		Ports  map[string]string
	}
	lookup := make(map[string]containerInfo, len(result.Items))
	for _, item := range result.Items {
		ports := make(map[string]string)
		for _, p := range item.Ports {
			if p.PublicPort > 0 {
				ports[portKey(p.PrivatePort, p.Type)] = portValue(p.PublicPort)
			}
		}
		lookup[item.ID] = containerInfo{
			Name:   containerName(item.Names),
			Image:  item.Image,
			Status: item.Status,
			State:  string(item.State),
			Ports:  ports,
		}
	}

	summaries := make([]runtimeio.SandboxSummary, 0, len(dbSandboxes))
	for _, db := range dbSandboxes {
		s := runtimeio.SandboxSummary{
			ID:    db.ID,
			Name:  db.Name,
			Image: db.Image,
			Ports: portKeys(map[string]string(db.Ports)),
		}

		// Enrich with live Docker state if the container still exists.
		if info, ok := lookup[db.ID]; ok {
			s.Name = info.Name
			s.Image = info.Image
			s.Status = info.Status
			s.State = info.State
			if len(info.Ports) > 0 {
				s.Ports = portKeys(info.Ports)
			}
		} else {
			s.Status = "removed"
			s.State = "removed"
		}

		// Attach expiration info if tracked.
		if entry := c.getTimerEntry(db.ID); entry != nil {
			ea := entry.expiresAt
			s.ExpiresAt = &ea
		}

		summaries = append(summaries, s)
	}

	return summaries, nil
}

// Inspect returns a curated view of a sandbox.
func (c *Client) Inspect(ctx context.Context, id string) (runtimeio.SandboxDetail, error) {
	result, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.SandboxDetail{}, wrapNotFound(err)
	}

	info := result.Container
	detail := runtimeio.SandboxDetail{
		ID:      info.ID,
		Name:    strings.TrimPrefix(info.Name, "/"),
		Image:   info.Config.Image,
		Status:  string(info.State.Status),
		Running: info.State.Running,
		Ports:   portKeys(extractPorts(info.NetworkSettings.Ports)),
		Resources: runtimeio.ResourceLimits{
			Memory: info.HostConfig.Memory / (1024 * 1024), // bytes to MB
			CPUs:   float64(info.HostConfig.NanoCPUs) / 1e9,
		},
		StartedAt:  info.State.StartedAt,
		FinishedAt: info.State.FinishedAt,
	}

	if entry := c.getTimerEntry(id); entry != nil {
		ea := entry.expiresAt
		detail.ExpiresAt = &ea
	}

	return detail, nil
}

// Stats returns a curated snapshot of container resource usage.
func (c *Client) Stats(ctx context.Context, id string) (runtimeio.SandboxStats, error) {
	result, err := c.cli.ContainerStats(ctx, id, moby.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true,
	})
	if err != nil {
		return runtimeio.SandboxStats{}, wrapNotFound(err)
	}
	defer result.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(result.Body).Decode(&raw); err != nil {
		return runtimeio.SandboxStats{}, fmt.Errorf("decode stats: %w", err)
	}

	// CPU % = (cpuDelta / systemDelta) * numCPUs * 100
	cpuPercent := 0.0
	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage - raw.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(raw.CPUStats.SystemUsage - raw.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta >= 0 {
		cpuPercent = (cpuDelta / sysDelta) * float64(raw.CPUStats.OnlineCPUs) * 100.0
	}

	memPercent := 0.0
	if raw.MemoryStats.Limit > 0 {
		memPercent = float64(raw.MemoryStats.Usage) / float64(raw.MemoryStats.Limit) * 100.0
	}

	return runtimeio.SandboxStats{
		CPU: math.Round(cpuPercent*100) / 100, // 2 decimal places
		Memory: runtimeio.MemoryUsage{
			Usage:   raw.MemoryStats.Usage,
			Limit:   raw.MemoryStats.Limit,
			Percent: math.Round(memPercent*100) / 100,
		},
		PIDs: raw.PidsStats.Current,
	}, nil
}

// containerName extracts a clean name from Docker's name list (removes leading /).
func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
