//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"submux/internal/safepath"
)

func defaultPaths() paths {
	home, _ := os.UserHomeDir()
	stateRoot := os.Getenv("XDG_STATE_HOME")
	if stateRoot == "" {
		stateRoot = filepath.Join(home, ".local", "state")
	}
	dataRoot := os.Getenv("XDG_DATA_HOME")
	if dataRoot == "" {
		dataRoot = filepath.Join(home, ".local", "share")
	}
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeRoot == "" {
		runtimeRoot = filepath.Join(stateRoot, "submux-agent", "run")
	} else {
		runtimeRoot = filepath.Join(runtimeRoot, "submux-agent")
	}
	dataRoot = filepath.Join(dataRoot, "submux-agent")
	return paths{
		state: filepath.Join(stateRoot, "submux-agent", "agent.db"), socket: filepath.Join(runtimeRoot, "agent.sock"),
		config: filepath.Join(dataRoot, "mihomo-config"), core: filepath.Join(dataRoot, "mihomo-core"), runtime: filepath.Join(dataRoot, "mihomo-runtime"),
	}
}

func prepareAgentStateLocation(path string) error {
	directory := filepath.Dir(path)
	if !filepath.IsAbs(path) {
		return errors.New("Agent state path must be absolute")
	}
	linked, err := safepath.ContainsLinkInExistingPath(directory)
	if err != nil || linked {
		return errors.New("Agent state directory must not contain symbolic links")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	linked, err = safepath.ContainsLink(directory)
	if err != nil || linked {
		return errors.New("Agent state directory must not contain symbolic links")
	}
	return os.Chmod(directory, 0700)
}

func defaultShell() (string, []string) {
	if value := os.Getenv("SHELL"); value != "" {
		return value, nil
	}
	return "/bin/sh", nil
}

func activateInstalledService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	unitPath := filepath.Join(configHome, "systemd", "user", "submux-agent.service")
	if _, err := os.Stat(unitPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	output, err := exec.Command("systemctl", "--user", "enable", "--now", "submux-agent.service").CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 300 {
			message = message[:300]
		}
		return fmt.Errorf("systemctl enable --now failed: %s", message)
	}
	return nil
}

func runAgentService(run func(context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}

func runAgentServiceCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("service requires exactly start, stop, status, install or uninstall")
	}
	if args[0] == "install" || args[0] == "uninstall" {
		return errors.New("on Linux, install or uninstall the fixed service with scripts/install-agent.sh")
	}
	if args[0] != "start" && args[0] != "stop" && args[0] != "status" {
		return fmt.Errorf("unknown service command %q", args[0])
	}
	commandArgs := []string{"--user", args[0], "submux-agent.service"}
	if args[0] == "status" {
		commandArgs = []string{"--user", "is-active", "submux-agent.service"}
	}
	output, err := exec.Command("systemctl", commandArgs...).CombinedOutput()
	message := strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %s", args[0], message)
	}
	if message != "" {
		fmt.Println(message)
	}
	return nil
}
