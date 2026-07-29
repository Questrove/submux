//go:build windows

package runtimeprocess

import (
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

func configureCommand(command *exec.Cmd) {
	command.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func terminateProcess(process *os.Process) error {
	return process.Kill()
}
