//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"submux/internal/safepath"
)

const (
	dockerDaemonConfigPath = "/etc/docker/daemon.json"
	dockerOwnershipPath    = "/var/lib/submux-agent/integrations/docker-daemon/ownership.json"
)

type linuxDockerOps struct{}

func DefaultDockerDaemonManager() (DockerDaemonManager, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("Docker daemon integration requires the root submux-agent service")
	}
	return &DockerManager{Ops: linuxDockerOps{}}, nil
}

func (linuxDockerOps) ReadConfig(context.Context) ([]byte, bool, error) {
	return secureReadRegular(dockerDaemonConfigPath, 2<<20)
}

func (linuxDockerOps) WriteConfig(_ context.Context, value []byte) error {
	return secureAtomicWrite(dockerDaemonConfigPath, value, 0644)
}

func (linuxDockerOps) RemoveConfig(context.Context) error {
	info, err := os.Lstat(dockerDaemonConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("refusing to remove a non-regular Docker daemon configuration")
	}
	return os.Remove(dockerDaemonConfigPath)
}

func (linuxDockerOps) ReadOwnership(context.Context) ([]byte, bool, error) {
	return secureReadRegular(dockerOwnershipPath, 64<<10)
}

func (linuxDockerOps) WriteOwnership(_ context.Context, value []byte) error {
	return secureAtomicWrite(dockerOwnershipPath, value, 0600)
}

func (linuxDockerOps) RemoveOwnership(context.Context) error {
	info, err := os.Lstat(dockerOwnershipPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("refusing to remove invalid Docker integration ownership state")
	}
	return os.Remove(dockerOwnershipPath)
}

func (linuxDockerOps) Validate(ctx context.Context) error {
	command := exec.CommandContext(ctx, "dockerd", "--validate", "--config-file="+dockerDaemonConfigPath)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dockerd validation failed: %s", safeCommandOutput(output))
	}
	return nil
}

func (linuxDockerOps) Restart(ctx context.Context) error {
	command := exec.CommandContext(ctx, "systemctl", "restart", "docker.service")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker.service restart failed: %s", safeCommandOutput(output))
	}
	return nil
}

func (linuxDockerOps) InspectProxy(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .}}")
	output, err := command.Output()
	if err != nil {
		return "", errors.New("docker info failed")
	}
	var value struct {
		HTTPProxy  string `json:"HTTPProxy"`
		HTTPSProxy string `json:"HTTPSProxy"`
	}
	if err := json.Unmarshal(output, &value); err != nil {
		return "", errors.New("docker info returned invalid status")
	}
	if normalizeProxyURL(value.HTTPProxy) != normalizeProxyURL(value.HTTPSProxy) {
		return "", errors.New("Docker reports different HTTP and HTTPS proxy values")
	}
	return normalizeProxyURL(value.HTTPProxy), nil
}

func secureReadRegular(path string, limit int64) ([]byte, bool, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, errors.New("managed Docker file must be a regular file and not a symlink")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(value)) > limit {
		return nil, false, errors.New("managed Docker file exceeds the size limit")
	}
	return value, true, nil
}

func secureAtomicWrite(path string, value []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	directoryMode := os.FileMode(0700)
	if mode == 0644 {
		directoryMode = 0755
	}
	if err := ensureFixedDirectory(directory, directoryMode); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("managed Docker file must be a regular file and not a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".submux-agent-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
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
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("managed Docker file changed to a non-regular file during update")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func ensureFixedDirectory(path string, mode os.FileMode) error {
	linked, err := safepath.ContainsLinkInExistingPath(path)
	if err != nil || linked {
		return errors.New("managed Docker directory must not contain symbolic links")
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	linked, err = safepath.ContainsLink(path)
	if err != nil || linked {
		return errors.New("managed Docker directory must not contain symbolic links")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return errors.New("managed Docker path parent is not a directory")
	}
	return nil
}

func safeCommandOutput(value []byte) string {
	text := strings.TrimSpace(string(value))
	if len(text) > 300 {
		text = text[:300]
	}
	if text == "" {
		return "command failed"
	}
	return text
}
