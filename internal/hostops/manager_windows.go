//go:build windows

package hostops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"submux/internal/mihomo"
)

const (
	windowsMihomoService = "submux-mihomo"
	windowsMihomoCommand = "mihomo-service"
)

type WindowsManager struct {
	CoreRoot    string
	ConfigRoot  string
	RuntimeRoot string
	Source      ReleaseFetcher
	Runner      commandRunner
	AgentBinary string
}

func DefaultLinuxManager(httpClient *http.Client) (CoreManager, error) {
	root := windowsAgentRoot()
	agentBinary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	manager := &WindowsManager{
		CoreRoot: filepath.Join(root, "mihomo-core"), ConfigRoot: filepath.Join(root, "mihomo-config"), RuntimeRoot: filepath.Join(root, "mihomo-runtime"),
		Source: OfficialReleaseSource(httpClient), Runner: execRunner{}, AgentBinary: agentBinary,
	}
	return manager, manager.validatePaths()
}

func windowsAgentRoot() string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "submux-agent")
}

func (m *WindowsManager) Install(ctx context.Context, channel, exactVersion string) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if m.Source == nil || m.Runner == nil {
		return errors.New("Windows core manager dependencies are incomplete")
	}
	if err := ensureManagedDirectory(m.CoreRoot, 0700); err != nil {
		return err
	}
	if err := recoverCoreSwap(m.CoreRoot); err != nil {
		return err
	}
	if err := m.recoverOperation(ctx); err != nil {
		return err
	}
	metadata, metadataErr := readCoreMetadata(filepath.Join(m.CoreRoot, "current", "metadata.json"))
	if metadataErr == nil && metadata.Version == exactVersion {
		return m.ensureService(ctx)
	}
	if _, err := os.Stat(filepath.Join(m.CoreRoot, "current")); err == nil && metadataErr != nil {
		return errors.New("refusing to replace an unknown existing Mihomo installation")
	}
	release, err := m.Source.Fetch(ctx, channel, exactVersion, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	staging := filepath.Join(m.CoreRoot, "staging")
	if err := m.removeManagedCoreDir(staging); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return err
	}
	defer m.removeManagedCoreDir(staging)
	binaryPath := filepath.Join(staging, "mihomo.exe")
	if err := os.WriteFile(binaryPath, release.Binary, 0700); err != nil {
		return err
	}
	output, err := m.Runner.Run(ctx, binaryPath, "-v")
	if err != nil || !reportsExactVersion(string(output), exactVersion) {
		return errors.New("downloaded Mihomo binary did not report the exact requested version")
	}
	metadataRaw, _ := json.Marshal(coreMetadata{Version: exactVersion, Digest: release.AssetDigest})
	if err := os.WriteFile(filepath.Join(staging, "metadata.json"), metadataRaw, 0600); err != nil {
		return err
	}
	status, _ := m.Status(ctx)
	wasRunning := status.State == "running"
	if err := beginCoreOperation(m.CoreRoot, coreOperation{Kind: "install", TargetVersion: exactVersion, WasRunning: wasRunning}); err != nil {
		return err
	}
	if wasRunning {
		if err := m.Stop(ctx); err != nil {
			return err
		}
	}
	current, previous := filepath.Join(m.CoreRoot, "current"), filepath.Join(m.CoreRoot, "previous")
	hadCurrent := false
	if err := m.removeManagedCoreDir(previous); err != nil {
		return err
	}
	if _, err := os.Stat(current); err == nil {
		hadCurrent = true
		if err := os.Rename(current, previous); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, current); err != nil {
		_ = os.Rename(previous, current)
		return err
	}
	if err := m.ensureService(ctx); err != nil {
		if hadCurrent {
			_ = m.restorePreviousCore()
		} else {
			_ = m.removeManagedCoreDir(current)
		}
		return err
	}
	if wasRunning {
		if err := m.Start(ctx); err != nil {
			_ = m.restorePreviousCore()
			_ = m.Start(ctx)
			return fmt.Errorf("new Mihomo version failed to start and was rolled back: %w", err)
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *WindowsManager) Uninstall(ctx context.Context) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if _, err := validateExistingManagedDirectory(m.CoreRoot); err != nil {
		return err
	}
	metadataPath := filepath.Join(m.CoreRoot, "current", "metadata.json")
	if _, err := os.Stat(filepath.Join(m.CoreRoot, "current")); err == nil {
		if _, metadataErr := readCoreMetadata(metadataPath); metadataErr != nil {
			return errors.New("refusing to uninstall an unknown Mihomo installation")
		}
	}
	_ = m.Stop(ctx)
	manager, err := mgr.Connect()
	if err == nil {
		if service, openErr := manager.OpenService(windowsMihomoService); openErr == nil {
			config, configErr := service.Config()
			if configErr != nil || !exactWindowsServiceCommand(config.BinaryPathName, m.AgentBinary, windowsMihomoCommand) {
				_ = service.Close()
				_ = manager.Disconnect()
				return errors.New("refusing to remove an unknown existing submux-mihomo service")
			}
			_ = service.Delete()
			_ = service.Close()
		}
		_ = manager.Disconnect()
	}
	for _, name := range []string{"staging", "previous", "failed"} {
		if err := m.removeManagedCoreDir(filepath.Join(m.CoreRoot, name)); err != nil {
			return err
		}
	}
	return os.RemoveAll(filepath.Join(m.CoreRoot, "current"))
}

func (m *WindowsManager) RollbackCore(ctx context.Context) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if err := ensureManagedDirectory(m.CoreRoot, 0700); err != nil {
		return err
	}
	if err := recoverCoreSwap(m.CoreRoot); err != nil {
		return err
	}
	if err := m.recoverOperation(ctx); err != nil {
		return err
	}
	status, _ := m.Status(ctx)
	wasRunning := status.State == "running"
	target, err := readCoreMetadata(filepath.Join(m.CoreRoot, "previous", "metadata.json"))
	if err != nil {
		return errors.New("no verified previous Mihomo version is available")
	}
	if err := beginCoreOperation(m.CoreRoot, coreOperation{Kind: "rollback", TargetVersion: target.Version, WasRunning: wasRunning}); err != nil {
		return err
	}
	if wasRunning {
		if err := m.Stop(ctx); err != nil {
			return err
		}
	}
	if err := m.restorePreviousCore(); err != nil {
		return err
	}
	if wasRunning {
		if err := m.Start(ctx); err != nil {
			restoreErr := m.restorePreviousCore()
			if restoreErr == nil {
				restoreErr = m.Start(ctx)
			}
			return errors.Join(errors.New("rolled-back Mihomo version failed to start; original version restored"), err, restoreErr)
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *WindowsManager) recoverOperation(ctx context.Context) error {
	operation, exists, err := readCoreOperation(m.CoreRoot)
	if err != nil || !exists {
		return err
	}
	current, err := readCoreMetadata(filepath.Join(m.CoreRoot, "current", "metadata.json"))
	if err != nil || current.Version != operation.TargetVersion {
		return finishCoreOperation(m.CoreRoot)
	}
	if err := m.ensureService(ctx); err != nil {
		return err
	}
	if operation.WasRunning {
		if err := m.Restart(ctx); err != nil {
			return err
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *WindowsManager) Status(context.Context) (CoreStatus, error) {
	metadata, err := readCoreMetadata(filepath.Join(m.CoreRoot, "current", "metadata.json"))
	if os.IsNotExist(err) {
		return CoreStatus{State: "not_installed"}, nil
	}
	if err != nil {
		return CoreStatus{}, errors.New("managed Mihomo metadata is invalid")
	}
	manager, err := mgr.Connect()
	if err != nil {
		return CoreStatus{}, err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(windowsMihomoService)
	if err != nil {
		return CoreStatus{Installed: true, Version: metadata.Version, PreviousVersion: previousCoreVersion(m.CoreRoot), State: "failed"}, nil
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return CoreStatus{}, err
	}
	state := "failed"
	switch status.State {
	case svc.Stopped:
		state = "stopped"
	case svc.Running, svc.StartPending, svc.ContinuePending:
		state = "running"
	}
	return CoreStatus{Installed: true, Version: metadata.Version, PreviousVersion: previousCoreVersion(m.CoreRoot), State: state}, nil
}

func (m *WindowsManager) Start(ctx context.Context) error {
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if !status.Installed {
		return errors.New("Mihomo is not installed")
	}
	if status.State == "running" {
		return nil
	}
	if err := checkWindowsProxyPorts(filepath.Join(m.ConfigRoot, "current", "config.yaml")); err != nil {
		return err
	}
	service, closeService, err := openWindowsMihomoService()
	if err != nil {
		return err
	}
	defer closeService()
	if err := service.Start(); err != nil {
		return err
	}
	return waitWindowsService(ctx, service, svc.Running)
}

func (m *WindowsManager) Stop(ctx context.Context) error {
	service, closeService, err := openWindowsMihomoService()
	if err != nil {
		return nil
	}
	defer closeService()
	status, err := service.Query()
	if err != nil || status.State == svc.Stopped {
		return err
	}
	if _, err := service.Control(svc.Stop); err != nil {
		return err
	}
	return waitWindowsService(ctx, service, svc.Stopped)
}

func (m *WindowsManager) Restart(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return m.Start(ctx)
}

func (m *WindowsManager) ReloadOrRestart(ctx context.Context) error { return m.Restart(ctx) }

func (m *WindowsManager) ValidateConfig(ctx context.Context, configPath string) error {
	root, _ := filepath.Abs(m.ConfigRoot)
	target, _ := filepath.Abs(configPath)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Base(target) != "config.yaml" {
		return errors.New("configuration validation path is outside the Agent-managed root")
	}
	binary := filepath.Join(m.CoreRoot, "current", "mihomo.exe")
	_, err = m.Runner.Run(ctx, binary, "-t", "-f", target)
	return err
}

func (m *WindowsManager) Logs(context.Context) (string, error) {
	path := filepath.Join(m.RuntimeRoot, "mihomo.log")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := info.Size() - (512 << 10)
	if offset < 0 {
		offset = 0
	}
	_, _ = file.Seek(offset, io.SeekStart)
	value, err := io.ReadAll(io.LimitReader(file, 512<<10))
	return string(value), err
}

func (m *WindowsManager) ensureService(ctx context.Context) error {
	if err := ensureManagedDirectory(m.RuntimeRoot, 0700); err != nil {
		return err
	}
	for _, permission := range []struct{ path, access string }{
		{m.CoreRoot, "RX"}, {m.ConfigRoot, "RX"}, {m.RuntimeRoot, "M"},
	} {
		if err := ensureManagedDirectory(permission.path, 0700); err != nil {
			return err
		}
		if _, err := m.Runner.Run(ctx, "icacls.exe", permission.path, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-19:(OI)(CI)"+permission.access); err != nil {
			return errors.New("could not apply the fixed Mihomo service ACL")
		}
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(windowsMihomoService)
	if err == nil {
		defer service.Close()
		config, configErr := service.Config()
		if configErr != nil {
			return configErr
		}
		if !exactWindowsServiceCommand(config.BinaryPathName, m.AgentBinary, windowsMihomoCommand) {
			return errors.New("refusing to take over an unknown existing submux-mihomo service")
		}
		return nil
	}
	service, err = manager.CreateService(windowsMihomoService, m.AgentBinary, mgr.Config{
		StartType: mgr.StartManual, ServiceStartName: `NT AUTHORITY\LocalService`,
		DisplayName: "submux managed Mihomo", Description: "Low-privilege Mihomo runtime managed by submux-agent",
	}, windowsMihomoCommand)
	if err != nil {
		return err
	}
	return service.Close()
}

func (m *WindowsManager) restorePreviousCore() error {
	current, previous, failed := filepath.Join(m.CoreRoot, "current"), filepath.Join(m.CoreRoot, "previous"), filepath.Join(m.CoreRoot, "failed")
	if _, err := readCoreMetadata(filepath.Join(previous, "metadata.json")); err != nil {
		return errors.New("no verified previous Mihomo version is available")
	}
	_ = m.removeManagedCoreDir(failed)
	if _, err := os.Stat(current); err == nil {
		if err := os.Rename(current, failed); err != nil {
			return err
		}
	}
	if err := os.Rename(previous, current); err != nil {
		_ = os.Rename(failed, current)
		return err
	}
	if err := m.removeManagedCoreDir(previous); err != nil {
		return err
	}
	return os.Rename(failed, previous)
}

func (m *WindowsManager) removeManagedCoreDir(path string) error {
	root, _ := filepath.Abs(m.CoreRoot)
	target, _ := filepath.Abs(path)
	if !strings.EqualFold(filepath.Dir(target), root) {
		return errors.New("refusing to remove a directory outside the core root")
	}
	switch strings.ToLower(filepath.Base(target)) {
	case "current", "staging", "previous", "failed":
		return os.RemoveAll(target)
	default:
		return errors.New("refusing to remove an unmanaged core directory")
	}
}

func (m *WindowsManager) validatePaths() error {
	expectedRoot, _ := filepath.Abs(windowsAgentRoot())
	for _, path := range []string{m.CoreRoot, m.ConfigRoot, m.RuntimeRoot} {
		absolute, err := filepath.Abs(path)
		if err != nil || !filepath.IsAbs(absolute) || !strings.EqualFold(filepath.Dir(absolute), expectedRoot) {
			return errors.New("Windows host operation paths must be fixed under ProgramData\\submux-agent")
		}
	}
	if m.AgentBinary == "" || !filepath.IsAbs(m.AgentBinary) {
		return errors.New("Windows Agent service binary path is invalid")
	}
	return nil
}

func exactWindowsServiceCommand(value, executable string, args ...string) bool {
	parts := []string{windows.EscapeArg(executable)}
	for _, arg := range args {
		parts = append(parts, windows.EscapeArg(arg))
	}
	return strings.EqualFold(strings.TrimSpace(value), strings.Join(parts, " "))
}

func openWindowsMihomoService() (*mgr.Service, func(), error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, func() {}, err
	}
	service, err := manager.OpenService(windowsMihomoService)
	if err != nil {
		manager.Disconnect()
		return nil, func() {}, err
	}
	return service, func() { _ = service.Close(); _ = manager.Disconnect() }, nil
}

func waitWindowsService(ctx context.Context, service *mgr.Service, desired svc.State) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == desired {
			return nil
		}
		if status.State == svc.Stopped && desired != svc.Stopped {
			return fmt.Errorf("Mihomo service stopped with Windows exit code %d", status.Win32ExitCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("timed out waiting for the Mihomo Windows service")
		case <-ticker.C:
		}
	}
}

func checkWindowsProxyPorts(configPath string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return errors.New("Agent-managed Mihomo configuration is unavailable")
	}
	proxyPort, _, err := mihomo.ProxyEndpoint(raw)
	if err != nil {
		return err
	}
	if proxyPort == 9090 {
		return errors.New("Mihomo proxy listener conflicts with the fixed loopback controller port 9090")
	}
	addresses := []string{"127.0.0.1:9090"}
	if proxyPort > 0 {
		addresses = append(addresses, net.JoinHostPort("127.0.0.1", strconv.Itoa(proxyPort)))
	}
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("required loopback port %s is already in use; refusing to take over another Mihomo or desktop client", address)
		}
		_ = listener.Close()
	}
	return nil
}

// RunMihomoWindowsService is the fixed SCM-only wrapper used to keep the core
// under a low-privilege LocalService identity.
func RunMihomoWindowsService() error {
	return svc.Run(windowsMihomoService, mihomoWindowsService{})
}

type mihomoWindowsService struct{}

func (mihomoWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	root := windowsAgentRoot()
	runtimeRoot := filepath.Join(root, "mihomo-runtime")
	if err := os.MkdirAll(runtimeRoot, 0700); err != nil {
		return true, 1
	}
	logPath := filepath.Join(runtimeRoot, "mihomo.log")
	if info, err := os.Stat(logPath); err == nil && info.Size() > 8<<20 {
		_ = os.Truncate(logPath, 0)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return true, 1
	}
	defer logFile.Close()
	command := exec.Command(filepath.Join(root, "mihomo-core", "current", "mihomo.exe"),
		"-d", runtimeRoot, "-f", filepath.Join(root, "mihomo-config", "current", "config.yaml"))
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		return true, 1
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				_ = command.Process.Kill()
				<-exited
				return false, 0
			}
		case err := <-exited:
			if err != nil {
				return true, 1
			}
			return false, 0
		}
	}
}
