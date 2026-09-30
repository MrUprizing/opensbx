package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"opensbx/internal/client"
	"opensbx/internal/terminaltext"
	"opensbx/models"
)

func command(ctx context.Context, c *client.Client, action, id string, pos, program []string, env map[string]string, sig int, o *options, out, stderr io.Writer) error {
	base := client.Path("sandboxes", id, "cmd")
	if action == "exec" {
		var value models.CommandResponse
		if err := c.Request(ctx, "POST", base, models.ExecCommandRequest{Command: program[0], Args: program[1:], Cwd: o.cwd, Env: env}, &value); err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "Interrupted before receiving a command ID; execution outcome is unknown. Inspect command history before retrying.")
				return &ExitError{Code: 130}
			}
			return err
		}
		if value.Command.ID == "" || value.Command.SandboxID != id {
			return errors.New("exec response is missing a valid command identity; inspect command history before retrying")
		}
		if o.detach {
			if o.quiet {
				_, err := fmt.Fprintln(out, terminaltext.Escape(value.Command.ID, false))
				return err
			}
			return output(out, value, o.json)
		}
		return foreground(ctx, c, id, value.Command.ID, o.json, out, stderr)
	}
	if action == "list" {
		var value models.CommandListResponse
		if err := c.Request(ctx, "GET", base, nil, &value); err != nil {
			return err
		}
		return output(out, value, o.json)
	}
	commandID := pos[1]
	base = client.Path("sandboxes", id, "cmd", commandID)
	if action == "logs" {
		if o.follow {
			return c.Logs(ctx, id, commandID, out, stderr)
		}
		var value models.CommandLogsResponse
		if err := c.Request(ctx, "GET", base+"/logs", nil, &value); err != nil {
			return err
		}
		if o.json {
			return output(out, value, true)
		}
		if _, err := io.WriteString(out, value.Stdout); err != nil {
			return err
		}
		_, err := io.WriteString(stderr, value.Stderr)
		return err
	}
	if action == "wait" {
		value, err := c.Wait(ctx, id, commandID)
		if err != nil {
			return err
		}
		if err := output(out, models.CommandResponse{Command: value}, o.json); err != nil {
			return err
		}
		return guestExit(value)
	}
	var value models.CommandResponse
	method := "GET"
	var body any
	if action == "kill" {
		method = "POST"
		base += "/kill"
		body = models.KillCommandRequest{Signal: sig}
	}
	if err := c.Request(ctx, method, base, body, &value); err != nil {
		return err
	}
	return output(out, value, o.json)
}

func guestExit(value models.CommandDetail) error {
	if value.ExitCode == nil {
		return errors.New("command completion has no confirmed exit code")
	}
	if *value.ExitCode < 0 || *value.ExitCode > 255 {
		return fmt.Errorf("guest returned unsupported exit code %d", *value.ExitCode)
	}
	if *value.ExitCode != 0 {
		return &ExitError{Code: *value.ExitCode}
	}
	return nil
}

func foreground(ctx context.Context, c *client.Client, sandboxID, commandID string, asJSON bool, out, stderr io.Writer) error {
	observeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var capture jsonCapture
	var stdout, guestStderr io.Writer = out, stderr
	if asJSON {
		stdout = captureWriter{capture: &capture, buffer: &capture.stdout}
		guestStderr = captureWriter{capture: &capture, buffer: &capture.stderr}
	}
	type completion struct {
		value models.CommandDetail
		err   error
	}
	status := make(chan completion, 1)
	logs := make(chan error, 1)
	statusDone, logsDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(statusDone)
		value, err := c.Wait(observeCtx, sandboxID, commandID)
		status <- completion{value, err}
	}()
	go func() { defer close(logsDone); logs <- c.Logs(observeCtx, sandboxID, commandID, stdout, guestStderr) }()
	defer func() { cancel(); <-statusDone; <-logsDone }()
	var terminal models.CommandDetail
	var logComplete, statusComplete bool
	var drain <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for !logComplete || !statusComplete {
		select {
		case <-ctx.Done():
			cancel()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := c.Request(cleanupCtx, "POST", client.Path("sandboxes", sandboxID, "cmd", commandID, "kill"), models.KillCommandRequest{Signal: 2}, nil)
			cleanupCancel()
			if err != nil {
				fmt.Fprintf(stderr, "Interrupted command %s; SIGINT was not confirmed: %s. Inspect or signal it explicitly.\n", terminaltext.Escape(commandID, false), terminaltext.Escape(err.Error(), false))
			} else {
				fmt.Fprintf(stderr, "Sent SIGINT to command %s; guest termination is not confirmed. Inspect or wait explicitly.\n", terminaltext.Escape(commandID, false))
			}
			return &ExitError{Code: 130}
		case result := <-status:
			if result.err != nil {
				if ctx.Err() != nil {
					continue
				}
				return fmt.Errorf("command %s: %w", commandID, result.err)
			}
			terminal = result.value
			statusComplete = true
			status = nil
			timer = time.NewTimer(5 * time.Second)
			drain = timer.C
		case err := <-logs:
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				return fmt.Errorf("command %s: %w (observation detached; inspect command status)", commandID, err)
			}
			logComplete = true
			logs = nil
		case <-drain:
			return fmt.Errorf("command %s exited, but log stream did not close; observation detached", commandID)
		}
	}
	if asJSON {
		result := struct {
			Command models.CommandDetail `json:"command"`
			Stdout  string               `json:"stdout"`
			Stderr  string               `json:"stderr"`
		}{terminal, capture.stdout.String(), capture.stderr.String()}
		if err := output(out, result, true); err != nil {
			return err
		}
	}
	return guestExit(terminal)
}
