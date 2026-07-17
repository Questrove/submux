//go:build windows

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
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"submux/internal/hostops"
	"submux/internal/safepath"
)

const windowsAgentServiceName = "submux-agent"

func defaultPaths() paths {
	root := os.Getenv("ProgramData")
	if root == "" {
		root = `C:\ProgramData`
	}
	root = filepath.Join(root, "submux-agent")
	return paths{state: filepath.Join(root, "agent.db"), socket: `\\.\pipe\submux-agent`, config: filepath.Join(root, "mihomo-config")}
}

func requireServicePrivileges() error {
	process, err := windows.GetCurrentProcess()
	if err != nil {
		return err
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	if !token.IsElevated() {
		return errors.New("submux-agent service mode requires an elevated LocalSystem or Administrator token")
	}
	return nil
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
	if output, err := exec.Command("icacls.exe", directory, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F").CombinedOutput(); err != nil {
		return fmt.Errorf("could not protect Agent state directory: %s", boundedWindowsOutput(output))
	}
	if info, err := os.Lstat(absolute); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Agent state database must be a regular file")
		}
		if output, err := exec.Command("icacls.exe", absolute, "/inheritance:r", "/grant:r", "*S-1-5-18:F", "*S-1-5-32-544:F").CombinedOutput(); err != nil {
			return fmt.Errorf("could not protect Agent state database: %s", boundedWindowsOutput(output))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func boundedWindowsOutput(value []byte) string {
	result := strings.TrimSpace(string(value))
	if len(result) > 300 {
		result = result[:300]
	}
	if result == "" {
		return "command failed"
	}
	return result
}

func defaultShell() (string, []string) { return "powershell.exe", []string{"-NoLogo"} }

func runAgentService(run func(context.Context) error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if isService {
		return svc.Run(windowsAgentServiceName, &agentWindowsService{run: run})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx)
}

type agentWindowsService struct{ run func(context.Context) error }

func (s *agentWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	current := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	changes <- current
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- current
			case svc.Stop, svc.Shutdown:
				current = svc.Status{State: svc.StopPending}
				changes <- current
				cancel()
				select {
				case err := <-done:
					if err != nil {
						return true, 1
					}
					return false, 0
				case <-time.After(15 * time.Second):
					return true, 1
				}
			}
		}
	}
}

func runPlatformMihomoService() error { return hostops.RunMihomoWindowsService() }

func runAgentServiceCommand(args []string) error {
	if len(args) != 1 || (args[0] != "install" && args[0] != "uninstall" && args[0] != "start" && args[0] != "stop" && args[0] != "status") {
		return errors.New("service requires exactly install, start, stop, status or uninstall")
	}
	if err := requireServicePrivileges(); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, openErr := manager.OpenService(windowsAgentServiceName)
	if args[0] == "start" || args[0] == "stop" || args[0] == "status" {
		if openErr != nil {
			return errors.New("submux-agent service is not installed")
		}
		defer service.Close()
		config, err := service.Config()
		if err != nil || !agentServiceCommandMatches(config.BinaryPathName, executable) {
			return errors.New("refusing to control an unknown existing submux-agent service")
		}
		status, err := service.Query()
		if err != nil {
			return err
		}
		switch args[0] {
		case "status":
			fmt.Println(windowsServiceState(status.State))
			return nil
		case "start":
			if status.State == svc.Running {
				return nil
			}
			return service.Start()
		case "stop":
			if status.State == svc.Stopped {
				return nil
			}
			if _, err := service.Control(svc.Stop); err != nil {
				return err
			}
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				status, _ = service.Query()
				if status.State == svc.Stopped {
					return nil
				}
				time.Sleep(250 * time.Millisecond)
			}
			return errors.New("timed out waiting for submux-agent service to stop")
		}
	}
	if args[0] == "uninstall" {
		if openErr != nil {
			return nil
		}
		defer service.Close()
		config, err := service.Config()
		if err != nil || !agentServiceCommandMatches(config.BinaryPathName, executable) {
			return errors.New("refusing to remove an unknown existing submux-agent service")
		}
		if status, err := service.Query(); err == nil && status.State != svc.Stopped {
			_, _ = service.Control(svc.Stop)
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				status, _ = service.Query()
				if status.State == svc.Stopped {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
		}
		return service.Delete()
	}
	if openErr == nil {
		defer service.Close()
		config, err := service.Config()
		if err != nil || !agentServiceCommandMatches(config.BinaryPathName, executable) {
			return errors.New("refusing to take over an unknown existing submux-agent service")
		}
		return nil
	}
	service, err = manager.CreateService(windowsAgentServiceName, executable, mgr.Config{
		StartType: mgr.StartManual, DisplayName: "submux host runtime Agent",
		Description: "Optional host-level Agent for the submux runtime control plane",
	}, "serve")
	if err != nil {
		return err
	}
	defer service.Close()
	fmt.Println("installed submux-agent service; enroll this host to enable and start it")
	return nil
}

func windowsServiceState(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.Paused:
		return "paused"
	default:
		return "unknown"
	}
}

func agentServiceCommandMatches(value, executable string) bool {
	expected := windows.EscapeArg(executable) + " " + windows.EscapeArg("serve")
	return strings.EqualFold(strings.TrimSpace(value), expected)
}

func activateInstalledService() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(windowsAgentServiceName)
	if err != nil {
		return nil
	}
	defer service.Close()
	config, err := service.Config()
	if err != nil || !agentServiceCommandMatches(config.BinaryPathName, executable) {
		return errors.New("installed submux-agent service identity is invalid")
	}
	config.StartType = mgr.StartAutomatic
	if err := service.UpdateConfig(config); err != nil {
		return err
	}
	status, err := service.Query()
	if err == nil && status.State == svc.Running {
		return nil
	}
	return service.Start()
}
