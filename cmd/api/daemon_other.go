//go:build !darwin && !linux && !windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

func detach(*exec.Cmd) error {
	return errors.New("background start is supported on macOS and Linux; run opensbx -h for foreground usage")
}

func serverSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
