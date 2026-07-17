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
	return paths{
		state:  "/var/lib/submux-agent/agent.db",
		socket: "/run/submux-agent/agent.sock",
		config: "/var/lib/submux-agent/mihomo-config",
	}
}

func requireServicePrivileges() error {
	if os.Geteuid() != 0 {
		return errors.New("serve must run as root because submux-agent is a host-level service")
	}
	return nil
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
	const unitPath = "/etc/systemd/system/submux-agent.service"
	if _, err := os.Stat(unitPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("root privileges are required to start submux-agent.service")
	}
	output, err := exec.Command("systemctl", "enable", "--now", "submux-agent.service").CombinedOutput()
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

func runPlatformMihomoService() error {
	return errors.New("mihomo-service is an internal Windows SCM entry point")
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
	if os.Geteuid() != 0 && args[0] != "status" {
		return errors.New("root privileges are required to start or stop submux-agent.service")
	}
	commandArgs := []string{args[0], "submux-agent.service"}
	if args[0] == "status" {
		commandArgs = []string{"is-active", "submux-agent.service"}
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
