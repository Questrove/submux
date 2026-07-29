//go:build windows

package runtimepaths

import (
	"os"
	"path/filepath"
)

func platformDefaults() Defaults {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	stateRoot := filepath.Join(programData, "SubmuxRuntime")
	return Defaults{
		StateRoot:       stateRoot,
		Endpoint:        `\\.\pipe\submux-runtime`,
		LockFile:        filepath.Join(stateRoot, "runtime.lock"),
		ControlEndpoint: `\\.\pipe\submux-runtime-mihomo`,
	}
}
