package mihomo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"submux/internal/safepath"
)

// CandidateBuilder composes a Runtime configuration source with the locally
// owned settings and reserved fields. Deployments cannot bypass this step.
type CandidateBuilder interface {
	BuildCandidate(source []byte) ([]byte, error)
}

type CandidateBuilderFunc func([]byte) ([]byte, error)

func (f CandidateBuilderFunc) BuildCandidate(source []byte) ([]byte, error) {
	return f(source)
}

type Validator interface {
	ValidateConfig(ctx context.Context, configPath string) error
}

type RuntimeService interface {
	ReloadOrRestart(ctx context.Context) error
	Stop(ctx context.Context) error
}

type RuntimeVerifier interface {
	VerifyRuntime(ctx context.Context, proxyAddr string) error
}

type Deployer struct {
	Root      string
	Builder   CandidateBuilder
	Validator Validator
	Service   RuntimeService
	Verifier  RuntimeVerifier

	mu sync.Mutex
}

type DeploymentResult struct {
	Revision         string   `json:"revision"`
	PreviousRevision string   `json:"previous_revision,omitempty"`
	SourceHash       string   `json:"source_hash"`
	CandidateHash    string   `json:"candidate_hash,omitempty"`
	Status           string   `json:"status"`
	Validation       string   `json:"validation,omitempty"`
	RolledBack       bool     `json:"rolled_back"`
	Error            string   `json:"error,omitempty"`
	ProxyPort        int      `json:"proxy_port,omitempty"`
	ProxyKind        string   `json:"proxy_kind,omitempty"`
	ProxyAddresses   []string `json:"proxy_addresses,omitempty"`
}

type deploymentMetadata struct {
	Revision       string   `json:"revision"`
	SourceHash     string   `json:"source_hash"`
	CandidateHash  string   `json:"candidate_hash"`
	AppliedAt      string   `json:"applied_at"`
	ProxyPort      int      `json:"proxy_port,omitempty"`
	ProxyKind      string   `json:"proxy_kind,omitempty"`
	ProxyAddresses []string `json:"proxy_addresses,omitempty"`
}

func ProxyEndpoint(config []byte) (int, string, error) {
	listeners, err := ProxyListeners(config)
	if err != nil {
		return 0, "", err
	}
	if len(listeners) == 0 {
		return 0, "", nil
	}
	return listeners[0].Port, listeners[0].Kind, nil
}

func (d *Deployer) Apply(ctx context.Context, revision, expectedSourceHash string, source []byte) (DeploymentResult, error) {
	return d.deploy(ctx, revision, expectedSourceHash, source, true)
}

func (d *Deployer) Prepare(ctx context.Context, revision, expectedSourceHash string, source []byte) (DeploymentResult, error) {
	return d.deploy(ctx, revision, expectedSourceHash, source, false)
}

func (d *Deployer) deploy(
	ctx context.Context,
	revision string,
	expectedSourceHash string,
	source []byte,
	activate bool,
) (DeploymentResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result := DeploymentResult{Revision: revision, Status: "failed"}
	if revision == "" || expectedSourceHash == "" || len(source) == 0 {
		return result, errors.New("revision, source hash and body are required")
	}
	sourceHash := sha256.Sum256(source)
	result.SourceHash = hex.EncodeToString(sourceHash[:])
	if !strings.EqualFold(expectedSourceHash, result.SourceHash) {
		return result, errors.New("source SHA-256 does not match its metadata")
	}
	if d.Builder == nil || d.Validator == nil || (activate && (d.Service == nil || d.Verifier == nil)) {
		return result, errors.New("deployer dependencies are incomplete")
	}
	root, err := d.safeRoot()
	if err != nil {
		return result, err
	}
	candidate, err := d.Builder.BuildCandidate(bytes.Clone(source))
	if err != nil {
		return result, fmt.Errorf("build candidate config: %w", err)
	}
	if len(candidate) == 0 {
		return result, errors.New("candidate config is empty")
	}
	candidateHash := sha256.Sum256(candidate)
	result.CandidateHash = hex.EncodeToString(candidateHash[:])
	listeners, err := ProxyListeners(candidate)
	if err != nil {
		return result, err
	}
	if len(listeners) == 0 {
		return result, errors.New("candidate configuration does not contain an explicit proxy listener")
	}
	result.ProxyPort = listeners[0].Port
	result.ProxyKind = listeners[0].Kind
	for _, listener := range listeners {
		result.ProxyAddresses = append(result.ProxyAddresses, listener.Address)
	}

	current := filepath.Join(root, "current")
	previousGood := filepath.Join(root, "previous-good")
	staging := filepath.Join(root, "staging")
	currentExists, err := verifyOptionalDeploymentDirectory(current)
	if err != nil {
		return result, err
	}
	if _, err := verifyOptionalDeploymentDirectory(previousGood); err != nil {
		return result, err
	}
	if err := removeManagedConfigDir(root, staging); err != nil {
		return result, err
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return result, err
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = removeManagedConfigDir(root, staging)
		}
	}()
	if err := writePrivateFile(filepath.Join(staging, "source.yaml"), source); err != nil {
		return result, err
	}
	configPath := filepath.Join(staging, "config.yaml")
	if err := writePrivateFile(configPath, candidate); err != nil {
		return result, err
	}
	metadata := deploymentMetadata{
		Revision:       revision,
		SourceHash:     result.SourceHash,
		CandidateHash:  result.CandidateHash,
		AppliedAt:      time.Now().UTC().Format(time.RFC3339),
		ProxyPort:      result.ProxyPort,
		ProxyKind:      result.ProxyKind,
		ProxyAddresses: append([]string(nil), result.ProxyAddresses...),
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return result, err
	}
	if err := writePrivateFile(filepath.Join(staging, "metadata.json"), metadataJSON); err != nil {
		return result, err
	}
	if err := d.Validator.ValidateConfig(ctx, configPath); err != nil {
		result.Validation = "rejected"
		result.Error = "Mihomo rejected the candidate configuration"
		return result, fmt.Errorf("validate candidate config: %w", err)
	}
	result.Validation = "passed"

	var previous deploymentMetadata
	if currentExists {
		previous, err = readDeploymentMetadata(filepath.Join(current, "metadata.json"))
		if err != nil {
			return result, err
		}
		result.PreviousRevision = previous.Revision
		if err := removeManagedConfigDir(root, previousGood); err != nil {
			return result, err
		}
		if err := os.Rename(current, previousGood); err != nil {
			return result, fmt.Errorf("preserve previous known-good config: %w", err)
		}
	}
	if err := os.Rename(staging, current); err != nil {
		if _, statErr := os.Stat(previousGood); statErr == nil {
			_ = os.Rename(previousGood, current)
		}
		return result, fmt.Errorf("activate candidate config: %w", err)
	}
	cleanupStaging = false

	if !activate {
		result.Status = "ready"
		return result, nil
	}

	activationErr := d.Service.ReloadOrRestart(ctx)
	if activationErr == nil {
		activationErr = verifyRuntimeAddresses(ctx, d.Verifier, result.ProxyAddresses)
	}
	if activationErr == nil {
		result.Status = "active"
		return result, nil
	}

	result.RolledBack = true
	failed := filepath.Join(root, "failed")
	if removeErr := removeManagedConfigDir(root, failed); removeErr != nil {
		return result, errors.Join(activationErr, removeErr)
	}
	if renameErr := os.Rename(current, failed); renameErr != nil {
		return result, errors.Join(activationErr, renameErr)
	}
	if _, statErr := os.Stat(previousGood); statErr == nil {
		if renameErr := os.Rename(previousGood, current); renameErr != nil {
			return result, errors.Join(activationErr, renameErr)
		}
		rollbackErr := d.Service.ReloadOrRestart(ctx)
		if rollbackErr == nil {
			rollbackErr = verifyRuntimeAddresses(ctx, d.Verifier, deploymentProxyAddresses(previous))
		}
		_ = removeManagedConfigDir(root, failed)
		if rollbackErr != nil {
			result.Error = "candidate failed and the previous known-good config could not be verified"
			return result, errors.Join(activationErr, rollbackErr)
		}
		result.Status = "rolled_back"
		result.Error = "candidate failed runtime verification; the previous known-good config was restored"
		return result, fmt.Errorf("candidate activation failed and was rolled back: %w", activationErr)
	}
	_ = d.Service.Stop(ctx)
	result.Error = "first candidate failed runtime verification; Mihomo stopped"
	return result, fmt.Errorf("first candidate activation failed: %w", activationErr)
}

func (d *Deployer) Rollback(ctx context.Context) (DeploymentResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result := DeploymentResult{Status: "failed", RolledBack: true}
	root, err := d.safeRoot()
	if err != nil {
		return result, err
	}
	current := filepath.Join(root, "current")
	previousGood := filepath.Join(root, "previous-good")
	currentExists, err := verifyOptionalDeploymentDirectory(current)
	if err != nil {
		return result, err
	}
	previousExists, err := verifyOptionalDeploymentDirectory(previousGood)
	if err != nil {
		return result, err
	}
	if !currentExists {
		return result, errors.New("current configuration is unavailable")
	}
	if !previousExists {
		return result, errors.New("no previous known-good configuration is available")
	}
	currentMetadata, err := readDeploymentMetadata(filepath.Join(current, "metadata.json"))
	if err != nil {
		return result, errors.New("current configuration metadata is unavailable")
	}
	targetMetadata, err := readDeploymentMetadata(filepath.Join(previousGood, "metadata.json"))
	if err != nil {
		return result, errors.New("previous known-good configuration metadata is unavailable")
	}
	result.Revision = targetMetadata.Revision
	result.PreviousRevision = currentMetadata.Revision
	result.SourceHash = targetMetadata.SourceHash
	result.CandidateHash = targetMetadata.CandidateHash
	result.ProxyPort = targetMetadata.ProxyPort
	result.ProxyKind = targetMetadata.ProxyKind
	result.ProxyAddresses = deploymentProxyAddresses(targetMetadata)
	result.Validation = "passed"

	failed := filepath.Join(root, "failed")
	if err := removeManagedConfigDir(root, failed); err != nil {
		return result, err
	}
	if err := os.Rename(current, failed); err != nil {
		return result, err
	}
	if err := os.Rename(previousGood, current); err != nil {
		_ = os.Rename(failed, current)
		return result, err
	}
	activationErr := d.Service.ReloadOrRestart(ctx)
	if activationErr == nil {
		activationErr = verifyRuntimeAddresses(ctx, d.Verifier, result.ProxyAddresses)
	}
	if activationErr != nil {
		if err := os.Rename(current, previousGood); err != nil {
			return result, errors.Join(activationErr, err)
		}
		if err := os.Rename(failed, current); err != nil {
			return result, errors.Join(activationErr, err)
		}
		restoreErr := d.Service.ReloadOrRestart(ctx)
		if restoreErr == nil {
			restoreErr = verifyRuntimeAddresses(ctx, d.Verifier, deploymentProxyAddresses(currentMetadata))
		}
		if restoreErr != nil {
			return result, errors.Join(activationErr, restoreErr)
		}
		result.Error = "known-good rollback failed verification; the original current config was restored"
		return result, fmt.Errorf("known-good activation failed and current was restored: %w", activationErr)
	}
	if err := os.Rename(failed, previousGood); err != nil {
		return result, err
	}
	result.Status = "active"
	result.Error = ""
	return result, nil
}

func verifyRuntimeAddresses(ctx context.Context, verifier RuntimeVerifier, addresses []string) error {
	if len(addresses) == 0 {
		return errors.New("Mihomo explicit proxy listener is unavailable")
	}
	for _, address := range addresses {
		if err := verifier.VerifyRuntime(ctx, address); err != nil {
			return err
		}
	}
	return nil
}

func deploymentProxyAddresses(metadata deploymentMetadata) []string {
	if len(metadata.ProxyAddresses) > 0 {
		return append([]string(nil), metadata.ProxyAddresses...)
	}
	if metadata.ProxyPort <= 0 {
		return nil
	}
	return []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(metadata.ProxyPort))}
}

func (d *Deployer) safeRoot() (string, error) {
	root, err := filepath.Abs(d.Root)
	if err != nil || d.Root == "" || root == filepath.VolumeName(root)+string(filepath.Separator) {
		return "", errors.New("invalid deployment root")
	}
	linked, err := safepath.ContainsLinkInExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("could not inspect deployment root ancestors: %w", err)
	}
	if linked {
		return "", errors.New("deployment root must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("deployment root must be a real directory")
	}
	linked, err = safepath.ContainsLink(root)
	if err != nil {
		return "", fmt.Errorf("could not inspect deployment root: %w", err)
	}
	if linked {
		return "", errors.New("deployment root must not contain symbolic or reparse links")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	return root, nil
}

func removeManagedConfigDir(root, path string) error {
	if filepath.Dir(path) != root {
		return errors.New("refusing to remove a path outside the deployment root")
	}
	switch filepath.Base(path) {
	case "staging", "previous-good", "failed":
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		linked, err := safepath.ContainsLink(path)
		if err != nil {
			return fmt.Errorf("inspect managed deployment directory: %w", err)
		}
		if linked {
			return errors.New("refusing to remove a linked managed deployment directory")
		}
		return os.RemoveAll(path)
	default:
		return fmt.Errorf("refusing to remove unmanaged directory %q", filepath.Base(path))
	}
}

func verifyOptionalDeploymentDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true, errors.New("managed deployment path must be a real directory")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return true, fmt.Errorf("inspect managed deployment directory: %w", err)
	}
	if linked {
		return true, errors.New("managed deployment directory must not contain symbolic or reparse links")
	}
	metadata, err := readDeploymentMetadata(filepath.Join(path, "metadata.json"))
	if err != nil {
		return true, err
	}
	source, err := os.ReadFile(filepath.Join(path, "source.yaml"))
	if err != nil {
		return true, errors.New("managed Runtime configuration source is unavailable")
	}
	candidate, err := os.ReadFile(filepath.Join(path, "config.yaml"))
	if err != nil {
		return true, errors.New("managed Runtime candidate configuration is unavailable")
	}
	sourceHash := sha256.Sum256(source)
	candidateHash := sha256.Sum256(candidate)
	if !strings.EqualFold(hex.EncodeToString(sourceHash[:]), metadata.SourceHash) ||
		!strings.EqualFold(hex.EncodeToString(candidateHash[:]), metadata.CandidateHash) {
		return true, errors.New("managed Runtime configuration digest does not match metadata")
	}
	return true, nil
}

func readDeploymentMetadata(path string) (deploymentMetadata, error) {
	var value deploymentMetadata
	raw, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, errors.New("Mihomo deployment metadata is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, errors.New("Mihomo deployment metadata is invalid")
	}
	if value.Revision == "" || len(value.SourceHash) != 64 || len(value.CandidateHash) != 64 {
		return value, errors.New("Mihomo deployment metadata is invalid")
	}
	if _, err := hex.DecodeString(value.SourceHash); err != nil {
		return value, errors.New("Mihomo deployment metadata is invalid")
	}
	if _, err := hex.DecodeString(value.CandidateHash); err != nil {
		return value, errors.New("Mihomo deployment metadata is invalid")
	}
	return value, nil
}

func writePrivateFile(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
