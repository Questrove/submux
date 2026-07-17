//go:build linux

package hostops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	defaultCoreRoot    = "/var/lib/submux-agent/mihomo-core"
	defaultConfigRoot  = "/var/lib/submux-agent/mihomo-config"
	defaultRuntimeRoot = "/var/lib/submux-agent/mihomo-runtime"
	defaultUnitPath    = "/etc/systemd/system/mihomo.service"
	serviceUser        = "submux-mihomo"
	serviceUnit        = "mihomo.service"
	managedUnitMarker  = "# Managed by submux-agent; do not edit."
)

type LinuxManager struct {
	CoreRoot    string
	ConfigRoot  string
	RuntimeRoot string
	UnitPath    string
	Source      ReleaseFetcher
	Runner      commandRunner
	LookupUser  func() (int, int, error)
	Chown       func(string, int, int) error
}

func DefaultLinuxManager(httpClient *http.Client) (CoreManager, error) {
	manager := &LinuxManager{
		CoreRoot: defaultCoreRoot, ConfigRoot: defaultConfigRoot, RuntimeRoot: defaultRuntimeRoot,
		UnitPath: defaultUnitPath, Source: OfficialReleaseSource(httpClient), Runner: execRunner{}, LookupUser: lookupServiceUser, Chown: os.Chown,
	}
	return manager, manager.validatePaths()
}

func (m *LinuxManager) Install(ctx context.Context, channel, exactVersion string) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if m.Source == nil || m.Runner == nil || m.LookupUser == nil || m.Chown == nil {
		return errors.New("Linux core manager dependencies are incomplete")
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
	currentMetadata, metadataErr := readCoreMetadata(filepath.Join(m.CoreRoot, "current", "metadata.json"))
	if metadataErr == nil && currentMetadata.Version == exactVersion {
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
	binaryPath := filepath.Join(staging, "mihomo")
	if err := os.WriteFile(binaryPath, release.Binary, 0750); err != nil {
		return err
	}
	output, err := m.Runner.Run(ctx, binaryPath, "-v")
	if err != nil || !reportsExactVersion(string(output), exactVersion) {
		return errors.New("downloaded Mihomo binary did not report the exact requested version")
	}
	metadata, _ := json.Marshal(coreMetadata{Version: exactVersion, Digest: release.AssetDigest})
	if err := os.WriteFile(filepath.Join(staging, "metadata.json"), metadata, 0600); err != nil {
		return err
	}
	status, _ := m.Status(ctx)
	wasRunning := status.State == "running"
	if err := beginCoreOperation(m.CoreRoot, coreOperation{Kind: "install", TargetVersion: exactVersion, WasRunning: wasRunning}); err != nil {
		return err
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
		if err := m.Restart(ctx); err != nil {
			_ = m.restorePreviousCore()
			_ = m.Restart(ctx)
			return fmt.Errorf("new Mihomo version failed to restart and was rolled back: %w", err)
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *LinuxManager) RollbackCore(ctx context.Context) error {
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
	if err := m.restorePreviousCore(); err != nil {
		return err
	}
	if wasRunning {
		if err := m.Restart(ctx); err != nil {
			restoreErr := m.restorePreviousCore()
			if restoreErr == nil {
				restoreErr = m.Restart(ctx)
			}
			return errors.Join(errors.New("rolled-back Mihomo version failed to start; original version restored"), err, restoreErr)
		}
	}
	return finishCoreOperation(m.CoreRoot)
}

func (m *LinuxManager) recoverOperation(ctx context.Context) error {
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

func (m *LinuxManager) Uninstall(ctx context.Context) error {
	if err := m.validatePaths(); err != nil {
		return err
	}
	if _, err := validateExistingManagedDirectory(m.CoreRoot); err != nil {
		return err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if status.State == "running" || status.State == "failed" {
		if err := m.Stop(ctx); err != nil {
			return err
		}
	}
	for _, name := range []string{"current", "previous", "failed", "staging"} {
		path := filepath.Join(m.CoreRoot, name)
		if filepath.Dir(path) != filepath.Clean(m.CoreRoot) {
			return errors.New("refusing to remove a core path outside the Agent-owned root")
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	if err := m.removeManagedUnit(); err != nil {
		return err
	}
	_, err = m.Runner.Run(ctx, "systemctl", "daemon-reload")
	return err
}

func (m *LinuxManager) restorePreviousCore() error {
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

func (m *LinuxManager) Status(ctx context.Context) (CoreStatus, error) {
	metadata, err := readCoreMetadata(filepath.Join(m.CoreRoot, "current", "metadata.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return CoreStatus{State: "not_installed"}, nil
		}
		return CoreStatus{}, err
	}
	result := CoreStatus{Installed: true, Version: metadata.Version, PreviousVersion: previousCoreVersion(m.CoreRoot), State: "stopped"}
	if _, err := m.Runner.Run(ctx, "systemctl", "is-active", "--quiet", serviceUnit); err == nil {
		result.State = "running"
	} else if _, failedErr := m.Runner.Run(ctx, "systemctl", "is-failed", "--quiet", serviceUnit); failedErr == nil {
		result.State = "failed"
	}
	return result, nil
}

func (m *LinuxManager) Start(ctx context.Context) error {
	if err := m.ensureService(ctx); err != nil {
		return err
	}
	if _, err := m.Runner.Run(ctx, "systemctl", "start", serviceUnit); err != nil {
		return err
	}
	_, err := m.Runner.Run(ctx, "systemctl", "is-active", "--quiet", serviceUnit)
	return err
}

func (m *LinuxManager) Stop(ctx context.Context) error {
	_, err := m.Runner.Run(ctx, "systemctl", "stop", serviceUnit)
	return err
}

func (m *LinuxManager) Restart(ctx context.Context) error {
	if err := m.prepareConfigPermissions(); err != nil {
		return err
	}
	if _, err := m.Runner.Run(ctx, "systemctl", "restart", serviceUnit); err != nil {
		return err
	}
	_, err := m.Runner.Run(ctx, "systemctl", "is-active", "--quiet", serviceUnit)
	return err
}

func (m *LinuxManager) ReloadOrRestart(ctx context.Context) error { return m.Restart(ctx) }

func (m *LinuxManager) ValidateConfig(ctx context.Context, configPath string) error {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	configRoot, _ := filepath.Abs(m.ConfigRoot)
	relative, err := filepath.Rel(configRoot, configPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("configuration path is outside the Agent-owned config root")
	}
	binary := filepath.Join(m.CoreRoot, "current", "mihomo")
	_, err = m.Runner.Run(ctx, binary, "-t", "-f", configPath)
	return err
}

func (m *LinuxManager) Logs(ctx context.Context) (string, error) {
	output, err := m.Runner.Run(ctx, "journalctl", "-u", serviceUnit, "-n", "200", "--no-pager", "-o", "short-iso")
	return string(output), err
}

func (m *LinuxManager) ensureService(ctx context.Context) error {
	uid, gid, err := m.LookupUser()
	if err != nil {
		if _, err := m.Runner.Run(ctx, "useradd", "--system", "--home-dir", m.RuntimeRoot, "--shell", "/usr/sbin/nologin", serviceUser); err != nil {
			return err
		}
		uid, gid, err = m.LookupUser()
		if err != nil {
			return err
		}
	}
	if err := ensureManagedDirectory(m.RuntimeRoot, 0700); err != nil {
		return err
	}
	if err := m.Chown(m.RuntimeRoot, uid, gid); err != nil {
		return err
	}
	unit := fmt.Sprintf(`%s
[Unit]
Description=Mihomo managed by submux-agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s
ExecStart=%s -d %s -f %s
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=%s
RestrictSUIDSGID=true
LockPersonality=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
AmbientCapabilities=
Restart=on-failure
RestartSec=3s

[Install]
WantedBy=multi-user.target
`, managedUnitMarker, serviceUser, serviceUser, m.RuntimeRoot, filepath.Join(m.CoreRoot, "current", "mihomo"), m.RuntimeRoot, filepath.Join(m.ConfigRoot, "current", "config.yaml"), m.RuntimeRoot)
	if err := m.writeManagedUnit([]byte(unit)); err != nil {
		return err
	}
	if _, err := m.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := m.prepareCorePermissions(); err != nil {
		return err
	}
	return m.prepareConfigPermissions()
}

func (m *LinuxManager) prepareConfigPermissions() error {
	_, gid, err := m.LookupUser()
	if err != nil {
		return err
	}
	exists, err := validateExistingManagedDirectory(m.ConfigRoot)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := os.Chmod(m.ConfigRoot, 0750); err != nil {
		return err
	}
	if err := m.Chown(m.ConfigRoot, 0, gid); err != nil && !os.IsNotExist(err) {
		return err
	}
	current := filepath.Join(m.ConfigRoot, "current")
	if _, err := os.Stat(current); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(current, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink found in Agent-owned configuration tree")
		}
		mode := os.FileMode(0640)
		if entry.IsDir() {
			mode = 0750
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		return m.Chown(path, 0, gid)
	})
}

func (m *LinuxManager) writeManagedUnit(value []byte) error {
	directory := filepath.Dir(m.UnitPath)
	if err := ensureManagedDirectory(directory, 0755); err != nil {
		return err
	}
	if info, err := os.Lstat(m.UnitPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing to replace a non-regular mihomo.service")
		}
		existing, readErr := os.ReadFile(m.UnitPath)
		if readErr != nil || !strings.Contains(string(existing), managedUnitMarker) {
			return errors.New("refusing to take over an existing unmanaged mihomo.service")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".mihomo.service-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, m.UnitPath)
}

func (m *LinuxManager) removeManagedUnit() error {
	info, err := os.Lstat(m.UnitPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove a non-regular mihomo.service")
	}
	value, err := os.ReadFile(m.UnitPath)
	if err != nil || !strings.Contains(string(value), managedUnitMarker) {
		return errors.New("refusing to remove an unmanaged mihomo.service")
	}
	return os.Remove(m.UnitPath)
}

func (m *LinuxManager) prepareCorePermissions() error {
	_, gid, err := m.LookupUser()
	if err != nil {
		return err
	}
	if err := os.Chmod(m.CoreRoot, 0750); err != nil {
		return err
	}
	if err := m.Chown(m.CoreRoot, 0, gid); err != nil {
		return err
	}
	current := filepath.Join(m.CoreRoot, "current")
	return filepath.WalkDir(current, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink found in Agent-owned core tree")
		}
		mode := os.FileMode(0640)
		if entry.IsDir() || filepath.Base(path) == "mihomo" {
			mode = 0750
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		return m.Chown(path, 0, gid)
	})
}

func (m *LinuxManager) validatePaths() error {
	for _, path := range []string{m.CoreRoot, m.ConfigRoot, m.RuntimeRoot, m.UnitPath} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return errors.New("host operation paths must be fixed absolute non-root paths")
		}
	}
	return nil
}

func (m *LinuxManager) removeManagedCoreDir(path string) error {
	root, _ := filepath.Abs(m.CoreRoot)
	path, _ = filepath.Abs(path)
	if filepath.Dir(path) != root {
		return errors.New("refusing to remove a directory outside the core root")
	}
	base := filepath.Base(path)
	if base != "current" && base != "staging" && base != "previous" && base != "failed" {
		return errors.New("refusing to remove an unmanaged core directory")
	}
	return os.RemoveAll(path)
}

func lookupServiceUser() (int, int, error) {
	value, err := user.Lookup(serviceUser)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(value.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(value.Gid)
	return uid, gid, err
}
