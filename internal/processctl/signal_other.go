//go:build !darwin && !linux && !windows

package processctl

import (
	"os"
	"syscall"
)

func signalProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}

func processTerminationSignals() []os.Signal {
	return []os.Signal{syscall.SIGTERM}
}
