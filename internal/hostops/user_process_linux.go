//go:build linux

package hostops

import (
	"os/exec"
	"syscall"
)

func configureUserChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func guardUserChild(*exec.Cmd) (func() error, error) { return func() error { return nil }, nil }
