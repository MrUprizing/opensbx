//go:build !windows

package processctl

import "os/exec"

func configureTestServerCommand(*exec.Cmd) {}
