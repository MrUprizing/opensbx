//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	return nil
}

func serverSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
