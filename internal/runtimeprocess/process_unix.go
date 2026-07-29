//go:build !windows

package runtimeprocess

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(process *os.Process) error {
	return syscall.Kill(-process.Pid, syscall.SIGTERM)
}
