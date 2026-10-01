//go:build windows

package processctl

import (
	"os/exec"
	"syscall"
)

func configureTestServerCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
