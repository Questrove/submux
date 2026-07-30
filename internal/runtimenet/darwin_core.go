//go:build darwin

package runtimenet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"submux/internal/safepath"
)

const (
	darwinCoreExecutionRoot = "/Library/Application Support/SubmuxRuntimePrivileged/core"
	darwinCoreObjectLimit   = int64(1 << 30)
	darwinCoreStartTimeout  = 10 * time.Second
	darwinCoreStopTimeout   = 10 * time.Second
)

type darwinCoreState struct {
	mu sync.Mutex

	staged    *darwinStagedCore
	cmd       *exec.Cmd
	done      chan error
	startedAt time.Time
	lastError string
}

type darwinStagedCore struct {
	request     PrivilegedCoreStage
	corePath    string
	configPath  string
	dataDir     string
	resourceDir string
}

func (system *DarwinSystem) StageCore(
	ctx context.Context,
	request PrivilegedCoreStage,
) (PrivilegedCoreStatus, error) {
	if err := system.validateCoreRequest(ctx, request.ObjectID); err != nil {
		return PrivilegedCoreStatus{}, err
	}
	if !validHex(request.CoreSHA256, 64) || !validHex(request.ConfigSHA256, 64) {
		return PrivilegedCoreStatus{}, errors.New("privileged Mihomo digest is invalid")
	}
	dataDir, err := system.fixedDarwinDataDir(request.DataObjectID)
	if err != nil {
		return PrivilegedCoreStatus{}, err
	}
	if err := validateDarwinFixedDirectory(dataDir); err != nil {
		return PrivilegedCoreStatus{}, fmt.Errorf("validate privileged Mihomo data object: %w", err)
	}
	resourceDir := filepath.Join(system.RuntimeRoot, "managed-resources")
	if err := validateDarwinFixedDirectory(resourceDir); err != nil {
		return PrivilegedCoreStatus{}, fmt.Errorf("validate privileged Mihomo resource object: %w", err)
	}

	system.core.mu.Lock()
	defer system.core.mu.Unlock()
	system.refreshDarwinCoreLocked()
	if system.core.cmd != nil {
		return system.darwinCoreStatusLocked(), errors.New("privileged Mihomo is running")
	}
	executionRoot, err := system.prepareDarwinExecutionRoot()
	if err != nil {
		return system.darwinCoreStatusLocked(), err
	}
	corePath, err := stageDarwinCoreObject(
		ctx,
		filepath.Join(system.RuntimeRoot, "core", "current", "mihomo"),
		executionRoot,
		"mihomo-",
		request.CoreSHA256,
		0700,
		system.allowTestPaths,
	)
	if err != nil {
		return system.darwinCoreStatusLocked(), fmt.Errorf("stage privileged Mihomo core: %w", err)
	}
	configPath, err := stageDarwinCoreObject(
		ctx,
		filepath.Join(system.RuntimeRoot, "config", "current", "config.yaml"),
		executionRoot,
		"config-",
		request.ConfigSHA256,
		0600,
		system.allowTestPaths,
	)
	if err != nil {
		return system.darwinCoreStatusLocked(), fmt.Errorf("stage privileged Mihomo config: %w", err)
	}
	system.core.staged = &darwinStagedCore{
		request:     request,
		corePath:    corePath,
		configPath:  configPath,
		dataDir:     dataDir,
		resourceDir: resourceDir,
	}
	system.core.lastError = ""
	return system.darwinCoreStatusLocked(), nil
}

func (system *DarwinSystem) StartCore(
	ctx context.Context,
	objectID string,
) (PrivilegedCoreStatus, error) {
	if err := system.validateCoreRequest(ctx, objectID); err != nil {
		return PrivilegedCoreStatus{}, err
	}
	system.core.mu.Lock()
	system.refreshDarwinCoreLocked()
	if system.core.cmd != nil {
		status := system.darwinCoreStatusLocked()
		system.core.mu.Unlock()
		return status, nil
	}
	staged := system.core.staged
	if staged == nil {
		status := system.darwinCoreStatusLocked()
		system.core.mu.Unlock()
		return status, errors.New("privileged Mihomo has not been staged")
	}
	if err := validateDarwinStagedObject(staged.corePath, staged.request.CoreSHA256, 0700, system.allowTestPaths); err != nil {
		system.core.mu.Unlock()
		return PrivilegedCoreStatus{}, err
	}
	if err := validateDarwinStagedObject(staged.configPath, staged.request.ConfigSHA256, 0600, system.allowTestPaths); err != nil {
		system.core.mu.Unlock()
		return PrivilegedCoreStatus{}, err
	}
	if err := validateDarwinFixedDirectory(staged.dataDir); err != nil {
		system.core.mu.Unlock()
		return PrivilegedCoreStatus{}, err
	}
	if err := validateDarwinFixedDirectory(staged.resourceDir); err != nil {
		system.core.mu.Unlock()
		return PrivilegedCoreStatus{}, err
	}
	if err := system.removeStaleDarwinControlSocket(); err != nil {
		system.core.mu.Unlock()
		return PrivilegedCoreStatus{}, err
	}

	command := exec.Command(staged.corePath, "-d", staged.dataDir, "-f", staged.configPath)
	command.Dir = staged.dataDir
	command.Env = []string{
		"HOME=" + staged.dataDir,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"SAFE_PATHS=" + strings.Join(
			[]string{staged.dataDir, staged.resourceDir, filepath.Dir(staged.configPath)},
			string(os.PathListSeparator),
		),
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		system.core.lastError = err.Error()
		status := system.darwinCoreStatusLocked()
		system.core.mu.Unlock()
		return status, fmt.Errorf("start privileged Mihomo: %w", err)
	}
	done := make(chan error, 1)
	system.core.cmd = command
	system.core.done = done
	system.core.startedAt = time.Now().UTC()
	system.core.lastError = ""
	system.core.mu.Unlock()

	go func() {
		waitErr := command.Wait()
		done <- waitErr
		close(done)
		system.core.mu.Lock()
		if system.core.cmd == command {
			system.core.cmd = nil
			system.core.done = nil
			if waitErr != nil {
				system.core.lastError = waitErr.Error()
			}
		}
		system.core.mu.Unlock()
	}()

	if err := system.waitForDarwinControlSocket(ctx, command, done); err != nil {
		stopContext, cancel := context.WithTimeout(context.Background(), darwinCoreStopTimeout)
		_, stopErr := system.StopCore(stopContext, objectID)
		cancel()
		return PrivilegedCoreStatus{}, errors.Join(err, stopErr)
	}
	return system.ObserveCore(ctx, objectID)
}

func (system *DarwinSystem) StopCore(
	ctx context.Context,
	objectID string,
) (PrivilegedCoreStatus, error) {
	if err := system.validateCoreRequest(ctx, objectID); err != nil {
		return PrivilegedCoreStatus{}, err
	}
	system.core.mu.Lock()
	system.refreshDarwinCoreLocked()
	command := system.core.cmd
	done := system.core.done
	system.core.mu.Unlock()
	if command == nil || command.Process == nil {
		_ = system.removeStaleDarwinControlSocket()
		return system.ObserveCore(ctx, objectID)
	}
	signalErr := syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) && !errors.Is(signalErr, syscall.ESRCH) {
		status, observeErr := system.ObserveCore(ctx, objectID)
		return status, errors.Join(fmt.Errorf("stop privileged Mihomo: %w", signalErr), observeErr)
	}
	timer := time.NewTimer(darwinCoreStopTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		waitDarwinCoreDone(done)
		status, observeErr := system.ObserveCore(context.Background(), objectID)
		return status, errors.Join(ctx.Err(), observeErr)
	case <-timer.C:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if !waitDarwinCoreDone(done) {
			return PrivilegedCoreStatus{}, errors.New("privileged Mihomo did not exit after SIGKILL")
		}
	}
	socketErr := system.removeStaleDarwinControlSocket()
	status, observeErr := system.ObserveCore(context.Background(), objectID)
	return status, errors.Join(socketErr, observeErr)
}

func waitDarwinCoreDone(done <-chan error) bool {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (system *DarwinSystem) ObserveCore(
	ctx context.Context,
	objectID string,
) (PrivilegedCoreStatus, error) {
	if err := system.validateCoreRequest(ctx, objectID); err != nil {
		return PrivilegedCoreStatus{}, err
	}
	system.core.mu.Lock()
	defer system.core.mu.Unlock()
	system.refreshDarwinCoreLocked()
	return system.darwinCoreStatusLocked(), nil
}

func (system *DarwinSystem) validateCoreRequest(ctx context.Context, objectID string) error {
	if err := system.validate(); err != nil {
		return err
	}
	if ctx == nil {
		return errors.New("privileged Mihomo context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if objectID != PrivilegedCoreObjectMihomo {
		return errors.New("privileged Mihomo object is not authorized")
	}
	return nil
}

func (system *DarwinSystem) fixedDarwinDataDir(objectID string) (string, error) {
	root := filepath.Join(system.RuntimeRoot, "mihomo-data")
	if objectID == "" {
		return root, nil
	}
	if !validOpaqueDarwinObjectID(objectID) {
		return "", errors.New("privileged Mihomo data object is invalid")
	}
	return filepath.Join(root, "sources", objectID), nil
}

func (system *DarwinSystem) prepareDarwinExecutionRoot() (string, error) {
	root := darwinCoreExecutionRoot
	if system.allowTestPaths {
		root = system.coreExecutionRoot
	}
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("privileged Mihomo execution root is invalid")
	}
	linked, err := safepath.ContainsLinkInExistingPath(root)
	if err != nil {
		return "", err
	}
	if linked {
		return "", errors.New("privileged Mihomo execution root must not contain links")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if linked, err := safepath.ContainsLink(root); err != nil {
		return "", err
	} else if linked {
		return "", errors.New("privileged Mihomo execution root must not contain links")
	}
	if !system.allowTestPaths {
		if os.Geteuid() != 0 {
			return "", errors.New("privileged Mihomo execution root requires root")
		}
		if err := os.Chown(root, 0, 0); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", errors.New("privileged Mihomo execution root is not locked")
	}
	if !system.allowTestPaths {
		stat, ok := info.Sys().(*unix.Stat_t)
		if !ok || stat.Uid != 0 || stat.Gid != 0 {
			return "", errors.New("privileged Mihomo execution root ownership is invalid")
		}
	}
	return root, nil
}

func (system *DarwinSystem) waitForDarwinControlSocket(
	ctx context.Context,
	command *exec.Cmd,
	done <-chan error,
) error {
	timer := time.NewTimer(darwinCoreStartTimeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				return errors.New("privileged Mihomo exited before creating its control Socket")
			}
			return fmt.Errorf("privileged Mihomo exited before creating its control Socket: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("privileged Mihomo control Socket did not become ready")
		case <-ticker.C:
			ready, err := system.secureDarwinControlSocket(command)
			if err != nil {
				return err
			}
			if ready {
				return nil
			}
		}
	}
}

func (system *DarwinSystem) secureDarwinControlSocket(command *exec.Cmd) (bool, error) {
	info, err := os.Lstat(system.ControlEndpoint)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("privileged Mihomo control endpoint is not a Socket")
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok || stat.Uid != 0 {
		return false, errors.New("privileged Mihomo control Socket ownership is invalid")
	}
	if command == nil || command.Process == nil {
		return false, errors.New("privileged Mihomo process is unavailable")
	}
	if err := os.Chown(system.ControlEndpoint, int(system.RuntimeUID), int(system.RuntimeGID)); err != nil {
		return false, err
	}
	if err := os.Chmod(system.ControlEndpoint, 0660); err != nil {
		return false, err
	}
	after, err := os.Lstat(system.ControlEndpoint)
	if err != nil || !os.SameFile(info, after) {
		return false, errors.New("privileged Mihomo control Socket changed while securing it")
	}
	return true, nil
}

func (system *DarwinSystem) removeStaleDarwinControlSocket() error {
	linked, err := safepath.ContainsLinkInExistingPath(filepath.Dir(system.ControlEndpoint))
	if err != nil {
		return err
	}
	if linked {
		return errors.New("privileged Mihomo control Socket path contains links")
	}
	info, err := os.Lstat(system.ControlEndpoint)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("privileged Mihomo control endpoint is not a removable Socket")
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != system.RuntimeUID) {
		return errors.New("privileged Mihomo control Socket is owned by another identity")
	}
	return os.Remove(system.ControlEndpoint)
}

func (system *DarwinSystem) refreshDarwinCoreLocked() {
	if system.core.cmd == nil || system.core.done == nil {
		return
	}
	select {
	case err := <-system.core.done:
		system.core.cmd = nil
		system.core.done = nil
		if err != nil {
			system.core.lastError = err.Error()
		}
	default:
	}
}

func (system *DarwinSystem) darwinCoreStatusLocked() PrivilegedCoreStatus {
	status := PrivilegedCoreStatus{
		ObjectID:    PrivilegedCoreObjectMihomo,
		State:       "stopped",
		ObservedAt:  time.Now().UTC(),
		PreviewOnly: true,
		Error:       system.core.lastError,
	}
	if system.core.staged != nil {
		status.State = "staged"
		status.CoreSHA256 = system.core.staged.request.CoreSHA256
		status.ConfigSHA256 = system.core.staged.request.ConfigSHA256
		status.DataObjectID = system.core.staged.request.DataObjectID
	}
	if system.core.cmd != nil && system.core.cmd.Process != nil {
		status.State = "running"
		status.PID = system.core.cmd.Process.Pid
		status.StartedAt = system.core.startedAt
	}
	return status
}

func stageDarwinCoreObject(
	ctx context.Context,
	sourcePath string,
	executionRoot string,
	namePrefix string,
	expectedDigest string,
	mode os.FileMode,
	allowTestPaths bool,
) (string, error) {
	source, size, err := openDarwinFixedRegular(sourcePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	tempPath := filepath.Join(executionRoot, ".stage-"+hex.EncodeToString(body))
	temp, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, digest), &contextReader{ctx: ctx, reader: source})
	if copyErr != nil || written != size {
		cleanup()
		return "", errors.Join(copyErr, errors.New("fixed object changed while staging"))
	}
	actualDigest := hex.EncodeToString(digest.Sum(nil))
	if actualDigest != expectedDigest {
		cleanup()
		return "", errors.New("fixed object digest does not match the verified request")
	}
	if !allowTestPaths {
		if err := temp.Chown(0, 0); err != nil {
			cleanup()
			return "", err
		}
	}
	if err := temp.Chmod(mode); err != nil {
		cleanup()
		return "", err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	finalPath := filepath.Join(executionRoot, namePrefix+actualDigest)
	if _, err := os.Lstat(finalPath); err == nil {
		_ = os.Remove(tempPath)
		if err := validateDarwinStagedObject(finalPath, expectedDigest, mode, allowTestPaths); err != nil {
			return "", err
		}
		return finalPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := validateDarwinStagedObject(finalPath, expectedDigest, mode, allowTestPaths); err != nil {
		return "", err
	}
	return finalPath, nil
}

func validateDarwinStagedObject(
	path string,
	expectedDigest string,
	mode os.FileMode,
	allowTestPaths bool,
) error {
	file, size, err := openDarwinFixedRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != mode.Perm() {
		return errors.New("staged privileged Mihomo object permissions are invalid")
	}
	if !allowTestPaths {
		stat, ok := info.Sys().(*unix.Stat_t)
		if !ok || stat.Uid != 0 || stat.Gid != 0 {
			return errors.New("staged privileged Mihomo object ownership is invalid")
		}
	}
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil || written != size {
		return errors.Join(err, errors.New("staged privileged Mihomo object changed"))
	}
	if hex.EncodeToString(digest.Sum(nil)) != expectedDigest {
		return errors.New("staged privileged Mihomo object digest changed")
	}
	return nil
}

func openDarwinFixedRegular(path string) (*os.File, int64, error) {
	if !filepath.IsAbs(path) {
		return nil, 0, errors.New("fixed object path is not absolute")
	}
	components := strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 {
		return nil, 0, errors.New("fixed object path is invalid")
	}
	parentFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(parentFD)
			return nil, 0, errors.New("fixed object path is invalid")
		}
		nextFD, openErr := unix.Openat(
			parentFD,
			component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		_ = unix.Close(parentFD)
		if openErr != nil {
			return nil, 0, openErr
		}
		parentFD = nextFD
	}
	finalName := components[len(components)-1]
	fd, err := unix.Openat(parentFD, finalName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	_ = unix.Close(parentFD)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, 0, errors.New("open fixed object")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > darwinCoreObjectLimit {
		_ = file.Close()
		return nil, 0, errors.New("fixed object is not an allowed regular file")
	}
	return file, info.Size(), nil
}

func validateDarwinFixedDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("fixed data object path is not absolute")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return err
	}
	if linked {
		return errors.New("fixed data object path must not contain links")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixed data object is not a real directory")
	}
	return nil
}

func validOpaqueDarwinObjectID(value string) bool {
	if len(value) < 3 || len(value) > 96 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' ||
			character == '-' {
			continue
		}
		return false
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(body []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(body)
}
