//go:build windows

package integration

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"

	"submux/internal/safepath"
)

type windowsDesktopOps struct {
	configPath    string
	ownershipPath string
}

func DefaultDockerDesktopManager() (DockerDesktopManager, error) {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return &DesktopManager{Ops: windowsDesktopOps{
		configPath:    filepath.Join(programData, "DockerDesktop", "admin-settings.json"),
		ownershipPath: filepath.Join(programData, "submux-agent", "integrations", "docker-desktop", "ownership.json"),
	}}, nil
}

func (o windowsDesktopOps) ReadConfig(context.Context) ([]byte, bool, error) {
	return windowsSecureRead(o.configPath, 2<<20)
}
func (o windowsDesktopOps) WriteConfig(_ context.Context, value []byte) error {
	return windowsAtomicWrite(o.configPath, value, false)
}
func (o windowsDesktopOps) RemoveConfig(context.Context) error {
	return windowsRemoveRegular(o.configPath)
}
func (o windowsDesktopOps) ReadOwnership(context.Context) ([]byte, bool, error) {
	return windowsSecureRead(o.ownershipPath, 64<<10)
}
func (o windowsDesktopOps) WriteOwnership(_ context.Context, value []byte) error {
	return windowsAtomicWrite(o.ownershipPath, value, true)
}
func (o windowsDesktopOps) RemoveOwnership(context.Context) error {
	return windowsRemoveRegular(o.ownershipPath)
}
func (windowsDesktopOps) RestartAndVerify(ctx context.Context) error {
	command := exec.CommandContext(ctx, "docker.exe", "desktop", "restart", "--timeout", "120")
	if output, err := command.CombinedOutput(); err != nil {
		return errors.New("Docker Desktop CLI restart failed: " + safeDesktopOutput(output))
	}
	command = exec.CommandContext(ctx, "docker.exe", "desktop", "status", "--format", "json")
	if output, err := command.CombinedOutput(); err != nil {
		return errors.New("Docker Desktop status verification failed: " + safeDesktopOutput(output))
	}
	return nil
}

func windowsSecureRead(path string, limit int64) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || windowsPathIsReparse(path) {
		return nil, false, errors.New("managed Docker Desktop file must be regular and not a reparse link")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(value)) > limit {
		return nil, false, errors.New("managed Docker Desktop file exceeds the size limit")
	}
	return value, true, nil
}

func windowsAtomicWrite(path string, value []byte, private bool) error {
	directory := filepath.Dir(path)
	linked, err := safepath.ContainsLinkInExistingPath(directory)
	if err != nil || linked {
		return errors.New("managed Docker Desktop directory must not contain reparse links")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	linked, err = safepath.ContainsLink(directory)
	if err != nil || linked {
		return errors.New("managed Docker Desktop directory must not contain reparse links")
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || windowsPathIsReparse(path)) {
		return errors.New("managed Docker Desktop file must be regular and not a reparse link")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".submux-agent-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
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
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || windowsPathIsReparse(path)) {
		return errors.New("managed Docker Desktop file changed to a reparse link during update")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	from, _ := windows.UTF16PtrFromString(temporaryPath)
	to, _ := windows.UTF16PtrFromString(path)
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}
	if private {
		if output, err := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", "*S-1-5-18:F", "*S-1-5-32-544:F").CombinedOutput(); err != nil {
			return errors.New("could not protect Docker Desktop ownership state: " + safeDesktopOutput(output))
		}
	}
	return nil
}

func windowsRemoveRegular(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || windowsPathIsReparse(path) {
		return errors.New("refusing to remove an invalid managed Docker Desktop file")
	}
	return os.Remove(path)
}

func windowsPathIsReparse(path string) bool {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	attributes, err := windows.GetFileAttributes(pointer)
	return err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func safeDesktopOutput(value []byte) string {
	if len(value) > 300 {
		value = value[:300]
	}
	if len(value) == 0 {
		return "command failed"
	}
	return string(value)
}
