package hostops

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"submux/internal/safepath"
)

type CoreStatus struct {
	Installed       bool   `json:"installed"`
	Version         string `json:"version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	State           string `json:"state"`
}

func previousCoreVersion(root string) string {
	metadata, err := readCoreMetadata(filepath.Join(root, "previous", "metadata.json"))
	if err != nil {
		return ""
	}
	return metadata.Version
}

type coreMetadata struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type coreOperation struct {
	Kind          string `json:"kind"`
	TargetVersion string `json:"target_version"`
	WasRunning    bool   `json:"was_running"`
}

func readCoreMetadata(path string) (coreMetadata, error) {
	var value coreMetadata
	raw, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, errors.New("Mihomo core metadata is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, errors.New("Mihomo core metadata is invalid")
	}
	if (!stableVersionPattern.MatchString(value.Version) && !alphaVersionPattern.MatchString(value.Version)) || len(value.Digest) != 64 {
		return value, errors.New("Mihomo core metadata is invalid")
	}
	if _, err := hex.DecodeString(value.Digest); err != nil {
		return value, errors.New("Mihomo core metadata is invalid")
	}
	return value, nil
}

func recoverCoreSwap(root string) error {
	current := filepath.Join(root, "current")
	previous := filepath.Join(root, "previous")
	failed := filepath.Join(root, "failed")
	currentExists, currentValid, err := coreDirectoryState(current)
	if err != nil {
		return err
	}
	previousExists, previousValid, err := coreDirectoryState(previous)
	if err != nil {
		return err
	}
	failedExists, failedValid, err := coreDirectoryState(failed)
	if err != nil {
		return err
	}
	if (currentExists && !currentValid) || (previousExists && !previousValid) || (failedExists && !failedValid) {
		return errors.New("refusing to recover an invalid Agent-managed Mihomo directory")
	}
	if !currentExists {
		if !previousValid {
			if failedExists {
				return errors.New("Mihomo core swap is incomplete and has no verified current or previous version")
			}
			return nil
		}
		if err := os.Rename(previous, current); err != nil {
			return err
		}
		if failedValid {
			if err := os.Rename(failed, previous); err != nil {
				return err
			}
		}
		return nil
	}
	if !previousExists && failedValid {
		return os.Rename(failed, previous)
	}
	if previousValid && failedValid {
		return os.RemoveAll(failed)
	}
	return nil
}

func coreDirectoryState(path string) (bool, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true, false, nil
	}
	_, metadataErr := readCoreMetadata(filepath.Join(path, "metadata.json"))
	return true, metadataErr == nil, nil
}

func beginCoreOperation(root string, value coreOperation) error {
	if (value.Kind != "install" && value.Kind != "rollback") || value.TargetVersion == "" {
		return errors.New("invalid Mihomo core operation")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	path := filepath.Join(root, "core-operation.json")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("Mihomo core operation marker must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(root, ".core-operation-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
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
	return os.Rename(temporaryPath, path)
}

func readCoreOperation(root string) (coreOperation, bool, error) {
	var value coreOperation
	path := filepath.Join(root, "core-operation.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return value, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return value, false, errors.New("Mihomo core operation marker is invalid")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return value, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, false, errors.New("Mihomo core operation marker is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || (value.Kind != "install" && value.Kind != "rollback") || value.TargetVersion == "" {
		return value, false, errors.New("Mihomo core operation marker is invalid")
	}
	return value, true, nil
}

func finishCoreOperation(root string) error {
	path := filepath.Join(root, "core-operation.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("refusing to remove an invalid Mihomo core operation marker")
	}
	return os.Remove(path)
}

type CoreManager interface {
	Install(ctx context.Context, channel, exactVersion string) error
	Uninstall(ctx context.Context) error
	RollbackCore(ctx context.Context) error
	Status(ctx context.Context) (CoreStatus, error)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	ReloadOrRestart(ctx context.Context) error
	ValidateConfig(ctx context.Context, configPath string) error
	Logs(ctx context.Context) (string, error)
}

type ReleaseFetcher interface {
	Fetch(ctx context.Context, channel, version, osName, arch string) (ReleaseBinary, error)
}

type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, errors.Join(err, errors.New(string(output)))
	}
	return output, nil
}

func reportsExactVersion(output, exactVersion string) bool {
	if exactVersion == "" {
		return false
	}
	pattern := `(?:^|[^0-9A-Za-z._+\-])` + regexp.QuoteMeta(exactVersion) + `(?:$|[^0-9A-Za-z._+\-])`
	return regexp.MustCompile(pattern).MatchString(output)
}

func ensureManagedDirectory(path string, mode os.FileMode) error {
	linked, err := safepath.ContainsLinkInExistingPath(path)
	if err != nil {
		return fmt.Errorf("could not inspect managed directory ancestors: %w", err)
	}
	if linked {
		return errors.New("managed path must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed path must be a real directory")
	}
	linked, err = safepath.ContainsLink(path)
	if err != nil {
		return fmt.Errorf("could not inspect managed directory: %w", err)
	}
	if linked {
		return errors.New("managed path must not contain symbolic or reparse links")
	}
	return os.Chmod(path, mode)
}

func validateExistingManagedDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("managed path must be a real directory")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil || linked {
		return false, errors.New("managed path must not contain symbolic or reparse links")
	}
	return true, nil
}
