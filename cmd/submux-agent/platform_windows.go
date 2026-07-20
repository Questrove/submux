//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"

	"submux/internal/safepath"
)

func defaultPaths() paths {
	root := os.Getenv("LOCALAPPDATA")
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, "AppData", "Local")
		}
	}
	root = filepath.Join(root, "submux-agent")
	return paths{
		state: filepath.Join(root, "agent.db"), socket: `\\.\pipe\submux-agent`,
		config: filepath.Join(root, "mihomo-config"), core: filepath.Join(root, "mihomo-core"), runtime: filepath.Join(root, "mihomo-runtime"),
	}
}

func prepareAgentStateLocation(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || !filepath.IsAbs(path) {
		return errors.New("Agent state path must be absolute")
	}
	directory := filepath.Dir(absolute)
	linked, err := safepath.ContainsLinkInExistingPath(directory)
	if err != nil || linked {
		return errors.New("Agent state directory must not contain reparse links")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	linked, err = safepath.ContainsLink(directory)
	if err != nil || linked {
		return errors.New("Agent state directory must not contain reparse links")
	}
	return nil
}

func defaultShell() (string, []string) { return "powershell.exe", []string{"-NoLogo"} }

func activateInstalledService() error { return nil }

func runAgentService(run func(context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx)
}

func runAgentServiceCommand([]string) error {
	return errors.New("Windows Agent is user-mode; start it with 'submux-agent serve' or your preferred user startup tool")
}
