//go:build !linux && !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func defaultPaths() paths {
	root, _ := os.UserConfigDir()
	root = filepath.Join(root, "submux-agent")
	return paths{
		state: filepath.Join(root, "agent.db"), socket: filepath.Join(root, "agent.sock"),
		config: filepath.Join(root, "mihomo-config"), core: filepath.Join(root, "mihomo-core"), runtime: filepath.Join(root, "mihomo-runtime"),
	}
}

func prepareAgentStateLocation(string) error {
	return errors.New("submux-agent state is supported only on Linux and Windows")
}

func defaultShell() (string, []string) { return "powershell.exe", []string{"-NoLogo"} }

func activateInstalledService() error { return nil }

func runAgentService(run func(context.Context) error) error { return run(context.Background()) }

func runAgentServiceCommand([]string) error {
	return errors.New("Agent service management is unavailable on this platform")
}
