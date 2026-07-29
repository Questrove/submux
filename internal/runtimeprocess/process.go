package runtimeprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"submux/internal/safepath"
)

type Process struct {
	BinaryPath string
	ConfigPath string
	DataDir    string
	SafePaths  []string

	mu            sync.Mutex
	cmd           *exec.Cmd
	done          chan error
	exits         chan ExitEvent
	runID         uint64
	stoppingRunID uint64
	startedAt     time.Time
}

type ExitEvent struct {
	RunID       uint64
	StartedAt   time.Time
	ExitedAt    time.Time
	Err         error
	Intentional bool
}

func (p *Process) IsRunning(context.Context) (bool, error) {
	if p == nil {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLocked()
	return p.cmd != nil, nil
}

func (p *Process) Start(ctx context.Context) error {
	if p == nil {
		return errors.New("Mihomo process manager is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLocked()
	if p.cmd != nil {
		return nil
	}
	p.ensureExitChannelLocked()
	if err := validateManagedExecutable(p.BinaryPath); err != nil {
		return err
	}
	if err := validateManagedConfig(p.ConfigPath); err != nil {
		return err
	}
	dataDir, err := prepareDataDir(p.DataDir)
	if err != nil {
		return err
	}
	command := exec.Command(p.BinaryPath, "-d", dataDir, "-f", p.ConfigPath)
	command.Dir = dataDir
	command.Env, err = mihomoEnvironment(p.SafePaths)
	if err != nil {
		return err
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	configureCommand(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Mihomo: %w", err)
	}
	done := make(chan error, 1)
	p.runID++
	runID := p.runID
	startedAt := time.Now().UTC()
	p.cmd = command
	p.done = done
	p.startedAt = startedAt
	go func() {
		waitErr := command.Wait()
		done <- waitErr
		close(done)
		p.finishRun(command, runID, startedAt, waitErr, time.Now().UTC())
	}()
	return nil
}

func (p *Process) finishRun(
	command *exec.Cmd,
	runID uint64,
	startedAt time.Time,
	waitErr error,
	exitedAt time.Time,
) {
	p.mu.Lock()
	intentional := p.stoppingRunID == runID
	if p.cmd == command {
		p.cmd = nil
		p.done = nil
	}
	if p.stoppingRunID == runID {
		p.stoppingRunID = 0
	}
	p.ensureExitChannelLocked()
	exits := p.exits
	p.mu.Unlock()
	exits <- ExitEvent{
		RunID:       runID,
		StartedAt:   startedAt,
		ExitedAt:    exitedAt,
		Err:         waitErr,
		Intentional: intentional,
	}
}

func (p *Process) ExitEvents() <-chan ExitEvent {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureExitChannelLocked()
	return p.exits
}

func (p *Process) ensureExitChannelLocked() {
	if p.exits == nil {
		p.exits = make(chan ExitEvent, 64)
	}
}

func (p *Process) refreshLocked() {
	if p.cmd == nil || p.done == nil {
		return
	}
	select {
	case <-p.done:
		p.cmd = nil
		p.done = nil
	default:
	}
}

func (p *Process) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.refreshLocked()
	command := p.cmd
	done := p.done
	if command != nil {
		p.stoppingRunID = p.runID
	}
	p.mu.Unlock()
	if command == nil {
		return nil
	}
	if err := terminateProcess(command.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop Mihomo: %w", err)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		_ = command.Process.Kill()
		<-done
		return ctx.Err()
	case <-time.After(10 * time.Second):
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("force stop Mihomo: %w", err)
		}
		<-done
		return nil
	}
}

func (p *Process) ReloadOrRestart(ctx context.Context) error {
	if err := p.Stop(ctx); err != nil {
		return err
	}
	return p.Start(ctx)
}

func validateManagedExecutable(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("Mihomo executable must use a fixed absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Mihomo executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Mihomo executable must be a regular non-linked file")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return fmt.Errorf("inspect Mihomo executable path: %w", err)
	}
	if linked {
		return errors.New("Mihomo executable path must not contain symbolic or reparse links")
	}
	return nil
}

func validateManagedConfig(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("Mihomo configuration must use a fixed absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Mihomo configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Mihomo configuration must be a regular non-linked file")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return fmt.Errorf("inspect Mihomo configuration path: %w", err)
	}
	if linked {
		return errors.New("Mihomo configuration path must not contain symbolic or reparse links")
	}
	return nil
}

func prepareDataDir(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("Mihomo data directory must use a fixed absolute path")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Mihomo data directory must be a real directory")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return "", fmt.Errorf("inspect Mihomo data directory: %w", err)
	}
	if linked {
		return "", errors.New("Mihomo data directory must not contain symbolic or reparse links")
	}
	if err := os.Chmod(path, 0700); err != nil {
		return "", err
	}
	return path, nil
}

func sanitizedEnvironment() []string {
	allowed := map[string]struct{}{
		"LANG": {}, "LC_ALL": {}, "SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
		"SYSTEMROOT": {}, "WINDIR": {}, "TEMP": {}, "TMP": {}, "TMPDIR": {},
	}
	var environment []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, keep := allowed[strings.ToUpper(key)]; keep {
			environment = append(environment, entry)
		}
	}
	return environment
}

func mihomoEnvironment(safePaths []string) ([]string, error) {
	environment := sanitizedEnvironment()
	if len(safePaths) == 0 {
		return environment, nil
	}
	normalized := make([]string, 0, len(safePaths))
	seen := make(map[string]struct{}, len(safePaths))
	for _, path := range safePaths {
		path, err := validateMihomoSafePath(path)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		normalized = append(normalized, path)
	}
	return append(
		environment,
		"SAFE_PATHS="+strings.Join(normalized, string(os.PathListSeparator)),
	), nil
}

func validateMihomoSafePath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("Mihomo safe paths must use fixed absolute paths")
	}
	absolute, err := filepath.Abs(path)
	if err != nil ||
		absolute == filepath.VolumeName(absolute)+string(filepath.Separator) ||
		strings.ContainsRune(absolute, os.PathListSeparator) {
		return "", errors.New("Mihomo safe path is invalid")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect Mihomo safe path: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Mihomo safe path must be a real directory")
	}
	linked, err := safepath.ContainsLink(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect Mihomo safe path: %w", err)
	}
	if linked {
		return "", errors.New("Mihomo safe path must not contain symbolic or reparse links")
	}
	return absolute, nil
}
