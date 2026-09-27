package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"opensbx/internal/applecontainer"
	"opensbx/internal/config"
	"opensbx/internal/images"
	"opensbx/internal/processctl"
	"opensbx/internal/runtimechoice"

	"github.com/mattn/go-isatty"
)

const startTimeout = 35 * time.Second

func runCLI(args []string, out io.Writer) error {
	if len(args) == 0 {
		return runServer(args)
	}
	if isHelpFlag(args[0]) {
		return writeRootHelp(out)
	}
	switch args[0] {
	case "help":
		return writeCommandHelp(out, args[1:])
	case "start":
		if containsHelpFlag(args[1:]) {
			return writeStartHelp(out)
		}
		return startServer(args[1:], out)
	case "stop":
		if containsHelpFlag(args[1:]) {
			return writeStopHelp(out)
		}
		return stopServer(args[1:], out)
	case "image":
		ctx, cancel := signal.NotifyContext(context.Background(), serverSignals()...)
		defer cancel()
		return images.CLI(ctx, args[1:], config.DefaultDataDir(), out)
	default:
		if strings.HasPrefix(args[0], "-") {
			return runServer(args)
		}
		return fmt.Errorf("unknown command %q; run opensbx -h to see available commands", args[0])
	}
}

func writeRootHelp(out io.Writer) error {
	_, err := fmt.Fprint(out, `OpenSBX runs isolated local sandboxes and exposes them through an API.

Usage:
  opensbx <command> [options]

Commands:
  start   Start the API and MCP server in the background
  stop    Gracefully stop the server and its managed sandboxes
  image   Manage the local OCI image catalog
  help    Show help for a command

Server options (for start):
  -runtime NAME       Runtime: docker or container (Apple Container)
  -addr ADDRESS       Loopback API address (default 127.0.0.1:18089)
  -data-dir PATH      Local image catalog and server state
  -log-file PATH      Server log file
  -legacy-db PATH     Explicit legacy execution database path

Examples:
  opensbx start
  opensbx start -runtime container
  opensbx stop
  opensbx image pull node:22
  opensbx start -h

Shortcuts: -h, -help, --help show this help. The image commands are also
available as `+"`opensbx image help`"+`.
`)
	return err
}

func writeStartHelp(out io.Writer) error {
	_, err := fmt.Fprint(out, `Usage: opensbx start [server options]

Start the OpenSBX API and MCP server in the background. With no runtime flag,
Docker is selected by default (Apple Silicon Macs may prompt for a runtime).
Use opensbx stop for a graceful shutdown. Server output is written to the log.

Options:
  -runtime NAME       Runtime: docker or container
  -addr ADDRESS       Loopback API address (default 127.0.0.1:18089)
  -data-dir PATH      Local image catalog and server state
  -log-file PATH      Server log file
  -legacy-db PATH     Explicit legacy execution database path

Use -h or -help for this message.
`)
	return err
}

func writeStopHelp(out io.Writer) error {
	_, err := fmt.Fprint(out, `Usage: opensbx stop [server options]

Send a graceful shutdown signal to the server started with the same data
directory. The server stops accepting requests and stops its managed sandboxes.

Options:
  -data-dir PATH      Data directory used by opensbx start
  -addr ADDRESS       Accepted for consistency; the server address is not used
                      to identify the process

Use -h or -help for this message.
`)
	return err
}

func writeImageHelp(out io.Writer) error {
	_, err := fmt.Fprint(out, `Usage: opensbx image <list|inspect|pull|import|export|remove> [options]

Manage OpenSBX's local OCI image catalog. These commands do not need a running
server or runtime. Use `+"`opensbx image <command> -h`"+` for command-specific options.

Use -h or -help for this message.
`)
	return err
}

func writeCommandHelp(out io.Writer, args []string) error {
	if len(args) == 0 {
		return writeRootHelp(out)
	}
	switch args[0] {
	case "start":
		return writeStartHelp(out)
	case "stop":
		return writeStopHelp(out)
	case "image":
		return writeImageHelp(out)
	default:
		return fmt.Errorf("unknown command %q; run opensbx -h to see available commands", args[0])
	}
}

func startServer(args []string, out io.Writer) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return fmt.Errorf("invalid start options: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	choice, err := runtimechoice.Select(ctx, cfg.Runtime, runtime.GOOS, isatty.IsTerminal(os.Stdin.Fd()), os.Stdin, out)
	if err != nil {
		return fmt.Errorf("could not select a runtime: %w", err)
	}
	if choice == "container" {
		if _, err := applecontainer.Resolve(ctx); err != nil {
			return fmt.Errorf("Apple Container is not ready: %w", err)
		}
	}
	if pid, running, err := processctl.Running(cfg.DataDir); err != nil {
		return err
	} else if running {
		if pid > 0 {
			return fmt.Errorf("OpenSBX is already running (PID %d); use `opensbx stop` first", pid)
		}
		return errors.New("OpenSBX is already starting; wait a moment and retry")
	}
	if err := checkAddressAvailable(cfg.Addr); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate OpenSBX executable: %w", err)
	}
	childArgs := append([]string(nil), args...)
	childArgs = append(childArgs, "-runtime", choice)
	cmd := exec.Command(executable, childArgs...)
	if err := detach(cmd); err != nil {
		return err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("prepare background process: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return fmt.Errorf("prepare log directory: %w", err)
	}
	logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", cfg.LogFile, err)
	}
	logInfo, statErr := logFile.Stat()
	logOffset := int64(0)
	if statErr == nil {
		logOffset = logInfo.Size()
	}
	_ = logFile.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start OpenSBX process: %w", err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	deadline := time.NewTimer(startTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pid, running, err := processctl.Running(cfg.DataDir)
		if err != nil {
			return err
		}
		if running && pid == cmd.Process.Pid {
			_, err := fmt.Fprintf(out, "OpenSBX started (PID %d). API: http://%s\nLogs: %s\n", pid, cfg.Addr, cfg.LogFile)
			return err
		}
		select {
		case childErr := <-childDone:
			if running && pid > 0 {
				return fmt.Errorf("OpenSBX is already running (PID %d); use `opensbx stop` first", pid)
			}
			return startupError(cfg.LogFile, logOffset, childErr)
		case <-deadline.C:
			_ = cmd.Process.Kill()
			return fmt.Errorf("OpenSBX did not become ready within %s; check its log at %q", startTimeout, cfg.LogFile)
		case <-ticker.C:
		}
	}
}

func stopServer(args []string, out io.Writer) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return fmt.Errorf("invalid stop options: %w", err)
	}
	pid, err := processctl.Stop(cfg.DataDir, 60*time.Second)
	if errors.Is(err, processctl.ErrNotRunning) {
		return fmt.Errorf("OpenSBX is not running for data directory %q", cfg.DataDir)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "OpenSBX stopped (PID %d).\n", pid)
	return err
}

func checkAddressAvailable(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("address %s is already in use; choose another loopback port with `-addr 127.0.0.1:18090`", addr)
		}
		return fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	return listener.Close()
}

func startupError(logPath string, offset int64, childErr error) error {
	file, readErr := os.Open(logPath)
	if readErr == nil {
		defer file.Close()
		if info, err := file.Stat(); err == nil && info.Size() > offset {
			if info.Size()-offset > 64*1024 {
				offset = info.Size() - 64*1024
			}
			contents := make([]byte, info.Size()-offset)
			if _, err := file.ReadAt(contents, offset); err == nil {
				lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
				for i := len(lines) - 1; i >= 0; i-- {
					line := strings.TrimSpace(lines[i])
					if line != "" {
						return fmt.Errorf("OpenSBX could not start: %s (log: %s)", line, logPath)
					}
				}
			}
		}
	}
	if childErr != nil {
		return fmt.Errorf("OpenSBX could not start: %w (check log %s)", childErr, logPath)
	}
	return fmt.Errorf("OpenSBX exited before becoming ready; check log %s", logPath)
}

func isHelpFlag(value string) bool {
	switch value {
	case "-h", "-help", "--help", "--h":
		return true
	default:
		return false
	}
}

func containsHelpFlag(args []string) bool {
	for _, arg := range args {
		if isHelpFlag(arg) {
			return true
		}
	}
	return false
}
