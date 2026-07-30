//go:build windows

package runtimeinstall

import (
	"os"
	"path/filepath"
)

type Defaults struct {
	InstallRoot string
	StateRoot   string
	ReceiptPath string
}

func CurrentDefaults() Defaults {
	programFiles := os.Getenv("ProgramFiles")
	programData := os.Getenv("ProgramData")
	return Defaults{
		InstallRoot: filepath.Join(programFiles, "Submux Runtime"),
		StateRoot:   filepath.Join(programData, "SubmuxRuntime"),
		ReceiptPath: filepath.Join(programData, "SubmuxRuntime", "install-receipt.json"),
	}
}
