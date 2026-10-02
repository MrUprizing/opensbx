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
	"time"

	"opensbx/internal/applecontainer"
	"opensbx/internal/cli"
	"opensbx/internal/config"
	"opensbx/internal/processctl"
	"opensbx/internal/runtimechoice"

	"github.com/mattn/go-isatty"
)

const startTimeout = 35 * time.Second

func runCLI(args []string, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), serverSignals()...)
	defer cancel()
	err := cli.Execute(ctx, args, os.Stdin, out, os.Stderr, cli.ServerHooks{Foreground: func(args []string, _ io.Writer) error { return runServer(args) }, Start: startServer, Stop: stopServer})
	if ctx.Err() != nil && err != nil {
		var exit *cli.ExitError
		if !errors.As(err, &exit) {
			return &cli.ExitError{Code: 130}
		}
	}
	return err
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
			return fmt.Errorf("runtime: Apple Container is not ready: %w", err)
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
		return listenError(addr, err)
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
