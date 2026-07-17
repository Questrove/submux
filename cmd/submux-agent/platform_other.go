//go:build !linux && !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func defaultPaths() paths {
	root := os.Getenv("ProgramData")
	if root == "" {
		root = os.TempDir()
	}
	root = filepath.Join(root, "submux-agent")
	return paths{state: filepath.Join(root, "agent.db"), socket: `\\.\pipe\submux-agent`, config: filepath.Join(root, "mihomo-config")}
}

func requireServicePrivileges() error {
	return errors.New("submux-agent service mode is currently implemented only on Linux")
}

func prepareAgentStateLocation(string) error {
	return errors.New("submux-agent state is supported only on Linux and Windows")
}

func defaultShell() (string, []string) { return "powershell.exe", []string{"-NoLogo"} }

func activateInstalledService() error { return nil }

func runAgentService(run func(context.Context) error) error { return run(context.Background()) }

func runPlatformMihomoService() error {
	return errors.New("Mihomo service mode is unavailable on this platform")
}

func runAgentServiceCommand([]string) error {
	return errors.New("Agent service management is unavailable on this platform")
}
