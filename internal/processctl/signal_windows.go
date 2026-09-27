//go:build windows

package processctl

import (
	"os"

	"golang.org/x/sys/windows"
)

func signalProcess(pid int) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}

func processTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
