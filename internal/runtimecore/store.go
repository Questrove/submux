package runtimecore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"

	"submux/internal/safepath"
)

var (
	stableVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$`)
	alphaVersionPattern  = regexp.MustCompile(`^alpha-[0-9a-f]{7,40}$`)
)

type Binary struct {
	Version      string
	BinaryDigest string
	Data         []byte
}

type Status struct {
	Installed       bool   `json:"installed"`
	Version         string `json:"version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
}

type BinaryVerifier interface {
	VerifyBinary(ctx context.Context, binaryPath, exactVersion string) error
}

type Activation interface {
	IsRunning(ctx context.Context) (bool, error)
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

type Store struct {
	Root       string
	Verifier   BinaryVerifier
	Activation Activation

	mu sync.Mutex
}

type metadata struct {
	Version      string `json:"version"`
	BinaryDigest string `json:"binary_digest"`
}

type operation struct {
	Kind          string `json:"kind"`
	TargetVersion string `json:"target_version"`
	WasRunning    bool   `json:"was_running"`
}

// Activate atomically installs a binary that the caller has already
// authorized. Store validates its local integrity and exact version, but it
// deliberately does not establish TUF trust or decide update policy.
func (s *Store) Activate(ctx context.Context, binary Binary) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	root, err := s.safeRoot()
	if err != nil {
		return err
	}
	if s.Verifier == nil {
		return errors.New("Mihomo binary verifier is required")
	}
	if err := validateBinary(binary); err != nil {
		return err
	}
	if err := recoverSwap(root); err != nil {
		return err
	}
	if err := s.recoverOperation(ctx, root); err != nil {
		return err
	}
	currentMetadata, metadataErr := readMetadata(filepath.Join(root, "current", "metadata.json"))
	if metadataErr == nil && currentMetadata.Version == binary.Version && stringsEqualFold(currentMetadata.BinaryDigest, binary.BinaryDigest) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, "current")); err == nil && metadataErr != nil {
		return errors.New("refusing to replace an unknown existing Mihomo core")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	staging := filepath.Join(root, "staging")
	if err := removeManagedCoreDir(root, staging); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return err
	}
	defer removeManagedCoreDir(root, staging)
	binaryPath := filepath.Join(staging, binaryName())
	if err := writeFile(binaryPath, binary.Data, 0700); err != nil {
		return err
	}
	if err := s.Verifier.VerifyBinary(ctx, binaryPath, binary.Version); err != nil {
		return fmt.Errorf("verify Mihomo binary: %w", err)
	}
	rawMetadata, err := json.Marshal(metadata{Version: binary.Version, BinaryDigest: binary.BinaryDigest})
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(staging, "metadata.json"), rawMetadata, 0600); err != nil {
		return err
	}

	wasRunning, err := s.isRunning(ctx)
	if err != nil {
		return err
	}
	if err := beginOperation(root, operation{Kind: "install", TargetVersion: binary.Version, WasRunning: wasRunning}); err != nil {
		return err
	}
	if wasRunning {
		if err := s.Activation.Stop(ctx); err != nil {
			return err
		}
	}

	current := filepath.Join(root, "current")
	previous := filepath.Join(root, "previous")
	hadCurrent := false
	if err := removeManagedCoreDir(root, previous); err != nil {
		return err
	}
	if _, err := os.Stat(current); err == nil {
		hadCurrent = true
		if err := os.Rename(current, previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(staging, current); err != nil {
		if hadCurrent {
			_ = os.Rename(previous, current)
		}
		return err
	}
	if wasRunning {
		if err := s.Activation.Start(ctx); err != nil {
			restoreErr := restorePrevious(root)
			if restoreErr == nil {
				restoreErr = s.Activation.Start(ctx)
			}
			return errors.Join(fmt.Errorf("new Mihomo version failed to start: %w", err), restoreErr)
		}
	}
	if !hadCurrent {
		_ = removeManagedCoreDir(root, previous)
	}
	return finishOperation(root)
}

func (s *Store) Rollback(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	root, err := s.safeRoot()
	if err != nil {
		return err
	}
	if err := recoverSwap(root); err != nil {
		return err
	}
	if err := s.recoverOperation(ctx, root); err != nil {
		return err
	}
	target, err := readMetadata(filepath.Join(root, "previous", "metadata.json"))
	if err != nil {
		return errors.New("no verified previous Mihomo version is available")
	}
	wasRunning, err := s.isRunning(ctx)
	if err != nil {
		return err
	}
	if err := beginOperation(root, operation{Kind: "rollback", TargetVersion: target.Version, WasRunning: wasRunning}); err != nil {
		return err
	}
	if wasRunning {
		if err := s.Activation.Stop(ctx); err != nil {
			return err
		}
	}
	if err := restorePrevious(root); err != nil {
		return err
	}
	if wasRunning {
		if err := s.Activation.Start(ctx); err != nil {
			restoreErr := restorePrevious(root)
			if restoreErr == nil {
				restoreErr = s.Activation.Start(ctx)
			}
			return errors.Join(errors.New("rolled-back Mihomo version failed to start; original version restored"), err, restoreErr)
		}
	}
	return finishOperation(root)
}

func (s *Store) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	root, err := s.safeRoot()
	if err != nil {
		return err
	}
	if err := recoverSwap(root); err != nil {
		return err
	}
	return s.recoverOperation(ctx, root)
}

func (s *Store) Status() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	root, err := s.safeRoot()
	if err != nil {
		return Status{}, err
	}
	if err := recoverSwap(root); err != nil {
		return Status{}, err
	}
	current, err := readMetadata(filepath.Join(root, "current", "metadata.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	previous, _ := readMetadata(filepath.Join(root, "previous", "metadata.json"))
	return Status{
		Installed:       true,
		Version:         current.Version,
		PreviousVersion: previous.Version,
	}, nil
}

func (s *Store) CurrentBinaryPath() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	root, err := s.safeRoot()
	if err != nil {
		return "", err
	}
	if err := recoverSwap(root); err != nil {
		return "", err
	}
	current := filepath.Join(root, "current")
	if err := verifyCoreDirectory(current); err != nil {
		return "", err
	}
	return filepath.Join(current, binaryName()), nil
}

func (s *Store) recoverOperation(ctx context.Context, root string) error {
	value, exists, err := readOperation(root)
	if err != nil || !exists {
		return err
	}
	current, err := readMetadata(filepath.Join(root, "current", "metadata.json"))
	if err != nil || current.Version != value.TargetVersion {
		return finishOperation(root)
	}
	if value.WasRunning {
		if s.Activation == nil {
			return errors.New("Mihomo activation is required to recover a running core operation")
		}
		running, err := s.Activation.IsRunning(ctx)
		if err != nil {
			return err
		}
		if !running {
			if err := s.Activation.Start(ctx); err != nil {
				return err
			}
		}
	}
	return finishOperation(root)
}

func (s *Store) isRunning(ctx context.Context) (bool, error) {
	if s.Activation == nil {
		return false, nil
	}
	return s.Activation.IsRunning(ctx)
}

func (s *Store) safeRoot() (string, error) {
	root, err := filepath.Abs(s.Root)
	if err != nil || s.Root == "" || root == filepath.VolumeName(root)+string(filepath.Separator) {
		return "", errors.New("Mihomo core root must be a fixed absolute non-root path")
	}
	linked, err := safepath.ContainsLinkInExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("inspect Mihomo core root ancestors: %w", err)
	}
	if linked {
		return "", errors.New("Mihomo core root must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Mihomo core root must be a real directory")
	}
	linked, err = safepath.ContainsLink(root)
	if err != nil {
		return "", fmt.Errorf("inspect Mihomo core root: %w", err)
	}
	if linked {
		return "", errors.New("Mihomo core root must not contain symbolic or reparse links")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	return root, nil
}

type CommandVerifier struct{}

func (CommandVerifier) VerifyBinary(ctx context.Context, binaryPath, exactVersion string) error {
	info, err := os.Lstat(binaryPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Mihomo binary must be a regular managed file")
	}
	output, err := exec.CommandContext(ctx, binaryPath, "-v").CombinedOutput()
	if err != nil {
		return errors.Join(err, errors.New(string(output)))
	}
	if !reportsExactVersion(string(output), exactVersion) {
		return errors.New("Mihomo binary did not report the exact expected version")
	}
	return nil
}

func validateBinary(binary Binary) error {
	if (!stableVersionPattern.MatchString(binary.Version) && !alphaVersionPattern.MatchString(binary.Version)) || len(binary.BinaryDigest) != 64 || len(binary.Data) == 0 {
		return errors.New("Mihomo binary metadata is invalid")
	}
	if _, err := hex.DecodeString(binary.BinaryDigest); err != nil {
		return errors.New("Mihomo binary metadata is invalid")
	}
	digest := sha256.Sum256(binary.Data)
	if !stringsEqualFold(hex.EncodeToString(digest[:]), binary.BinaryDigest) {
		return errors.New("Mihomo binary SHA-256 does not match its metadata")
	}
	return nil
}

func readMetadata(path string) (metadata, error) {
	var value metadata
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
	if (!stableVersionPattern.MatchString(value.Version) && !alphaVersionPattern.MatchString(value.Version)) || len(value.BinaryDigest) != 64 {
		return value, errors.New("Mihomo core metadata is invalid")
	}
	if _, err := hex.DecodeString(value.BinaryDigest); err != nil {
		return value, errors.New("Mihomo core metadata is invalid")
	}
	return value, nil
}

func recoverSwap(root string) error {
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
		return errors.New("refusing to recover an invalid Runtime-managed Mihomo directory")
	}
	if !currentExists {
		if !previousValid {
			if failedExists {
				return errors.New("Mihomo core swap has no verified current or previous version")
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
		return removeManagedCoreDir(root, failed)
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
	return true, verifyCoreDirectory(path) == nil, nil
}

func verifyCoreDirectory(path string) error {
	value, err := readMetadata(filepath.Join(path, "metadata.json"))
	if err != nil {
		return err
	}
	binaryPath := filepath.Join(path, binaryName())
	info, err := os.Lstat(binaryPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("verified Mihomo binary is unavailable")
	}
	file, err := os.Open(binaryPath)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if !stringsEqualFold(hex.EncodeToString(hash.Sum(nil)), value.BinaryDigest) {
		return errors.New("Mihomo binary SHA-256 does not match managed metadata")
	}
	return nil
}

func beginOperation(root string, value operation) error {
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
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func readOperation(root string) (operation, bool, error) {
	var value operation
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

func finishOperation(root string) error {
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

func restorePrevious(root string) error {
	current := filepath.Join(root, "current")
	previous := filepath.Join(root, "previous")
	failed := filepath.Join(root, "failed")
	if _, err := readMetadata(filepath.Join(previous, "metadata.json")); err != nil {
		return errors.New("no verified previous Mihomo version is available")
	}
	if err := removeManagedCoreDir(root, failed); err != nil {
		return err
	}
	if err := os.Rename(current, failed); err != nil {
		return err
	}
	if err := os.Rename(previous, current); err != nil {
		_ = os.Rename(failed, current)
		return err
	}
	if err := removeManagedCoreDir(root, previous); err != nil {
		return err
	}
	return os.Rename(failed, previous)
}

func removeManagedCoreDir(root, path string) error {
	if filepath.Dir(path) != root {
		return errors.New("refusing to remove a directory outside the Mihomo core root")
	}
	switch filepath.Base(path) {
	case "current", "staging", "previous", "failed":
		return os.RemoveAll(path)
	default:
		return errors.New("refusing to remove an unmanaged Mihomo core directory")
	}
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "mihomo.exe"
	}
	return "mihomo"
}

func writeFile(path string, value []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func reportsExactVersion(output, exactVersion string) bool {
	if exactVersion == "" {
		return false
	}
	pattern := `(?:^|[^0-9A-Za-z._+\-])` + regexp.QuoteMeta(exactVersion) + `(?:$|[^0-9A-Za-z._+\-])`
	return regexp.MustCompile(pattern).MatchString(output)
}

func stringsEqualFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		a := left[index]
		b := right[index]
		if a >= 'A' && a <= 'F' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'F' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}
