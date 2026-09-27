package runtimeio

import (
	"context"
	"io"
	"opensbx/internal/sandbox"
)

func command(item CommandDetail, id sandbox.SandboxID, err error) (sandbox.Command, error) {
	if err != nil {
		return sandbox.Command{}, err
	}
	return sandbox.Command{ID: sandbox.CommandID(item.ID), SandboxID: id, Name: item.Name, Args: item.Args, Cwd: item.Cwd, ExitCode: item.ExitCode, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt}, nil
}
func (a *Adapter) ExecCommand(ctx context.Context, id sandbox.SandboxID, req sandbox.ProcessRequest) (sandbox.Command, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Command{}, err
	}
	item, err := a.engine.ExecCommand(ctx, row.RuntimeID(), ExecCommandRequest{Command: req.Command, Args: req.Args, Cwd: req.Cwd, Env: req.Env})
	return command(item, id, err)
}
func (a *Adapter) GetCommand(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (sandbox.Command, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Command{}, err
	}
	item, err := a.engine.GetCommand(ctx, row.RuntimeID(), string(cmd))
	return command(item, id, err)
}
func (a *Adapter) ListCommands(ctx context.Context, id sandbox.SandboxID) ([]sandbox.Command, error) {
	row, err := a.native(id)
	if err != nil {
		return nil, err
	}
	items, err := a.engine.ListCommands(ctx, row.RuntimeID())
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.Command, 0, len(items))
	for _, item := range items {
		c, _ := command(item, id, nil)
		out = append(out, c)
	}
	return out, nil
}
func (a *Adapter) KillCommand(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID, signal int) (sandbox.Command, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Command{}, err
	}
	item, err := a.engine.KillCommand(ctx, row.RuntimeID(), string(cmd), signal)
	return command(item, id, err)
}
func (a *Adapter) WaitCommand(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (sandbox.Command, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Command{}, err
	}
	item, err := a.engine.WaitCommand(ctx, row.RuntimeID(), string(cmd))
	return command(item, id, err)
}
func (a *Adapter) GetCommandLogs(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (sandbox.Logs, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Logs{}, err
	}
	item, err := a.engine.GetCommandLogs(ctx, row.RuntimeID(), string(cmd))
	return sandbox.Logs{Stdout: item.Stdout, Stderr: item.Stderr, ExitCode: item.ExitCode}, err
}
func (a *Adapter) StreamCommandLogs(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (io.ReadCloser, io.ReadCloser, error) {
	row, err := a.native(id)
	if err != nil {
		return nil, nil, err
	}
	return a.engine.StreamCommandLogs(ctx, row.RuntimeID(), string(cmd))
}
func (a *Adapter) ReadFile(ctx context.Context, id sandbox.SandboxID, path string) (string, error) {
	row, err := a.native(id)
	if err != nil {
		return "", err
	}
	return a.engine.ReadFile(ctx, row.RuntimeID(), path)
}
func (a *Adapter) WriteFile(ctx context.Context, id sandbox.SandboxID, path, content string) error {
	row, err := a.native(id)
	if err != nil {
		return err
	}
	return a.engine.WriteFile(ctx, row.RuntimeID(), path, content)
}
func (a *Adapter) DeleteFile(ctx context.Context, id sandbox.SandboxID, path string) error {
	row, err := a.native(id)
	if err != nil {
		return err
	}
	return a.engine.DeleteFile(ctx, row.RuntimeID(), path)
}
func (a *Adapter) ListDir(ctx context.Context, id sandbox.SandboxID, path string) (string, error) {
	row, err := a.native(id)
	if err != nil {
		return "", err
	}
	return a.engine.ListDir(ctx, row.RuntimeID(), path)
}

var _ sandbox.Runtime = (*Adapter)(nil)
var _ sandbox.Cache = (*Adapter)(nil)
