package hostops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// UserManager owns only files and processes below the current user's Agent
// data directory. It intentionally never creates system users, services, or
// configuration for other applications.
type UserManager struct {
	CoreRoot    string
	ConfigRoot  string
	RuntimeRoot string
	Source      ReleaseFetcher
	Runner      commandRunner

	mu            sync.Mutex
	command       *exec.Cmd
	done          chan struct{}
	childGuard    func() error
	stopping      bool
	failed        bool
	logs          boundedLog
	progress      CoreProgressReporter
	resourceProxy string
}

func NewUserManager(coreRoot, configRoot, runtimeRoot string, httpClient *http.Client) (CoreManager, error) {
	manager := &UserManager{
		CoreRoot: coreRoot, ConfigRoot: configRoot, RuntimeRoot: runtimeRoot,
		Source: OfficialReleaseSource(httpClient), Runner: execRunner{},
	}
	return manager, manager.validatePaths()
}

func (m *UserManager) SetResourceProxy(proxyURL string) error {
	if proxyURL == m.resourceProxy {
		return nil
	}
	client, err := officialReleaseHTTPClient(proxyURL)
	if err != nil {
		return err
	}
	source := OfficialReleaseSource(client)
	source.SetProgressReporter(m.progress)
	m.Source = source
	m.resourceProxy = proxyURL
	return nil
}

func (m *UserManager) SetProgressReporter(reporter CoreProgressReporter) {
	m.progress = reporter
	if source, ok := m.Source.(interface{ SetProgressReporter(CoreProgressReporter) }); ok {
		source.SetProgressReporter(reporter)
	}
}

func (m *UserManager) ListCoreVersions(ctx context.Context, channel string, limit int) ([]string, error) {
	source, ok := m.Source.(ReleaseVersionSource)
	if !ok {
		return nil, errors.New("Mihomo release source cannot list versions")
	}
	return source.ListVersions(ctx, channel, runtime.GOOS, runtime.GOARCH, limit)
}

func (m *UserManager) report(phase string) {
	if m.progress != nil {
		m.progress(CoreProgress{Phase: phase})
	}
}

func (m *UserManager) Install(ctx context.Context, channel, exactVersion string) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if m.Source == nil || m.Runner == nil {
		return errors.New("user core manager dependencies are incomplete")
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
		return nil
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
	binaryPath := filepath.Join(staging, userMihomoBinaryName())
	if err := os.WriteFile(binaryPath, release.Binary, 0700); err != nil {
		return err
	}
	m.report("verifying_binary")
	output, err := m.Runner.Run(ctx, binaryPath, "-v")
	if err != nil || !reportsExactVersion(string(output), exactVersion) {
		return errors.New("downloaded Mihomo binary did not report the exact requested version")
	}
	rawMetadata, _ := json.Marshal(coreMetadata{Version: exactVersion, Digest: release.AssetDigest})
	if err := os.WriteFile(filepath.Join(staging, "metadata.json"), rawMetadata, 0600); err != nil {
		return err
	}
	status, _ := m.Status(ctx)
	wasRunning := status.State == "running"
	m.report("switching_core")
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
	if wasRunning {
		m.report("restarting_core")
		if err := m.Start(ctx); err != nil {
			_ = m.restorePreviousCore()
			_ = m.Start(ctx)
			return fmt.Errorf("new Mihomo version failed to start and was rolled back: %w", err)
		}
	}
	if !hadCurrent {
		_ = m.removeManagedCoreDir(previous)
	}
	m.report("core_ready")
	return finishCoreOperation(m.CoreRoot)
}

func (m *UserManager) Uninstall(ctx context.Context) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if err := m.Stop(ctx); err != nil {
		return err
	}
	for _, name := range []string{"current", "previous", "failed", "staging"} {
		if err := m.removeManagedCoreDir(filepath.Join(m.CoreRoot, name)); err != nil {
			return err
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *UserManager) RollbackCore(ctx context.Context) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if err := ensureManagedDirectory(m.CoreRoot, 0700); err != nil {
		return err
	}
	if err := recoverCoreSwap(m.CoreRoot); err != nil {
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
			_ = m.restorePreviousCore()
			_ = m.Start(ctx)
			return errors.Join(errors.New("rolled-back Mihomo version failed to start; original version restored"), err)
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *UserManager) recoverOperation(ctx context.Context) error {
	operation, exists, err := readCoreOperation(m.CoreRoot)
	if err != nil || !exists {
		return err
	}
	current, err := readCoreMetadata(filepath.Join(m.CoreRoot, "current", "metadata.json"))
	if err != nil || current.Version != operation.TargetVersion {
		return finishCoreOperation(m.CoreRoot)
	}
	if operation.WasRunning {
		status, _ := m.Status(ctx)
		if status.State != "running" {
			if err := m.Start(ctx); err != nil {
				return err
			}
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *UserManager) Status(context.Context) (CoreStatus, error) {
	metadata, err := readCoreMetadata(filepath.Join(m.CoreRoot, "current", "metadata.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CoreStatus{State: "not_installed"}, nil
		}
		return CoreStatus{}, err
	}
	result := CoreStatus{Installed: true, Version: metadata.Version, PreviousVersion: previousCoreVersion(m.CoreRoot), State: "stopped"}
	m.mu.Lock()
	if m.command != nil {
		result.State = "running"
	} else if m.failed {
		result.State = "failed"
	}
	m.mu.Unlock()
	return result, nil
}

func (m *UserManager) Start(context.Context) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	binaryPath := filepath.Join(m.CoreRoot, "current", userMihomoBinaryName())
	configPath := filepath.Join(m.ConfigRoot, "current", "config.yaml")
	if info, err := os.Lstat(binaryPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("verified Agent-managed Mihomo binary is unavailable")
	}
	if info, err := os.Lstat(configPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("verified Agent-managed Mihomo configuration is unavailable")
	}
	if err := ensureManagedDirectory(m.RuntimeRoot, 0700); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.command != nil {
		return nil
	}
	command := exec.Command(binaryPath, "-d", m.RuntimeRoot, "-f", configPath)
	configureUserChild(command)
	command.Stdout, command.Stderr = &m.logs, &m.logs
	if err := command.Start(); err != nil {
		m.failed = true
		return err
	}
	guard, err := guardUserChild(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		m.failed = true
		return fmt.Errorf("contain Mihomo child process: %w", err)
	}
	m.command, m.done, m.childGuard, m.failed, m.stopping = command, make(chan struct{}), guard, false, false
	go m.wait(command)
	return nil
}

func (m *UserManager) wait(command *exec.Cmd) {
	err := command.Wait()
	m.mu.Lock()
	if m.command == command {
		if m.childGuard != nil {
			_ = m.childGuard()
			m.childGuard = nil
		}
		m.command = nil
		m.failed = err != nil && !m.stopping
		m.stopping = false
		close(m.done)
		m.done = nil
	}
	m.mu.Unlock()
}

func (m *UserManager) Stop(context.Context) error {
	m.mu.Lock()
	command := m.command
	done := m.done
	if command == nil {
		m.failed = false
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	m.mu.Unlock()
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if done != nil {
		<-done
	}
	return nil
}

func (m *UserManager) Restart(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return m.Start(ctx)
}

func (m *UserManager) ReloadOrRestart(ctx context.Context) error { return m.Restart(ctx) }

func (m *UserManager) ValidateConfig(ctx context.Context, configPath string) error {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	configRoot, _ := filepath.Abs(m.ConfigRoot)
	relative, err := filepath.Rel(configRoot, configPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("configuration path is outside the Agent-owned config root")
	}
	_, err = m.Runner.Run(ctx, filepath.Join(m.CoreRoot, "current", userMihomoBinaryName()), "-t", "-f", configPath)
	return err
}

func (m *UserManager) Logs(context.Context) (string, error) { return m.logs.String(), nil }

func (m *UserManager) restorePreviousCore() error {
	current, previous, failed := filepath.Join(m.CoreRoot, "current"), filepath.Join(m.CoreRoot, "previous"), filepath.Join(m.CoreRoot, "failed")
	if _, err := os.Stat(previous); err != nil {
		return errors.New("no previous Agent-managed Mihomo version is available")
	}
	if err := m.removeManagedCoreDir(failed); err != nil {
		return err
	}
	if err := os.Rename(current, failed); err != nil {
		return err
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

func (m *UserManager) validatePaths() error {
	for _, path := range []string{m.CoreRoot, m.ConfigRoot, m.RuntimeRoot} {
		root := filepath.VolumeName(path) + string(filepath.Separator)
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) == root {
			return errors.New("Agent user data paths must be fixed absolute non-root paths")
		}
	}
	return nil
}

func (m *UserManager) removeManagedCoreDir(path string) error {
	root, _ := filepath.Abs(m.CoreRoot)
	path, _ = filepath.Abs(path)
	if filepath.Dir(path) != root {
		return errors.New("refusing to remove a directory outside the core root")
	}
	switch filepath.Base(path) {
	case "current", "staging", "previous", "failed":
		return os.RemoveAll(path)
	default:
		return errors.New("refusing to remove an unmanaged core directory")
	}
}

func userMihomoBinaryName() string {
	if runtime.GOOS == "windows" {
		return "mihomo.exe"
	}
	return "mihomo"
}

type boundedLog struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedLog) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, value...)
	const limit = 256 << 10
	if len(b.data) > limit {
		b.data = append([]byte(nil), b.data[len(b.data)-limit:]...)
	}
	return len(value), nil
}

func (b *boundedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.Clone(b.data))
}
