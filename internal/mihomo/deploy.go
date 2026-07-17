package mihomo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"submux/internal/safepath"
)

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
	Root           string
	ControllerAddr string
	Secret         string
	Validator      Validator
	Service        RuntimeService
	Verifier       RuntimeVerifier
}

type DeploymentResult struct {
	Revision         string `json:"revision"`
	PreviousRevision string `json:"previous_revision,omitempty"`
	ArtifactHash     string `json:"artifact_hash"`
	EffectiveHash    string `json:"effective_hash,omitempty"`
	Status           string `json:"status"`
	Validation       string `json:"validation,omitempty"`
	RolledBack       bool   `json:"rolled_back"`
	Error            string `json:"error,omitempty"`
	ProxyPort        int    `json:"proxy_port,omitempty"`
	ProxyKind        string `json:"proxy_kind,omitempty"`
}

type deploymentMetadata struct {
	Revision      string `json:"revision"`
	ArtifactHash  string `json:"artifact_hash"`
	EffectiveHash string `json:"effective_hash"`
	AppliedAt     string `json:"applied_at"`
	ProxyPort     int    `json:"proxy_port,omitempty"`
	ProxyKind     string `json:"proxy_kind,omitempty"`
}

func ProxyEndpoint(config []byte) (int, string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(config, &document); err != nil {
		return 0, "", fmt.Errorf("parse Mihomo proxy listener: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return 0, "", errors.New("Mihomo config must be a YAML mapping")
	}
	root := document.Content[0]
	for _, candidate := range []struct{ key, kind string }{{"mixed-port", "mixed"}, {"port", "http"}, {"socks-port", "socks5"}} {
		for index := 0; index+1 < len(root.Content); index += 2 {
			if root.Content[index].Value != candidate.key {
				continue
			}
			value := root.Content[index+1]
			if value.Kind != yaml.ScalarNode {
				return 0, "", fmt.Errorf("%s must be an integer port", candidate.key)
			}
			port, err := strconv.Atoi(value.Value)
			if err != nil || port < 0 || port > 65535 {
				return 0, "", fmt.Errorf("%s must be between 0 and 65535", candidate.key)
			}
			if port > 0 {
				return port, candidate.kind, nil
			}
		}
	}
	return 0, "", nil
}

func ApplyAgentOverlay(base []byte, controllerAddr, secret string) ([]byte, error) {
	host, _, err := net.SplitHostPort(controllerAddr)
	if err != nil {
		return nil, errors.New("controller address must contain a port")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() || secret == "" {
		return nil, errors.New("controller address must be loopback and secret must be non-empty")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(base, &document); err != nil {
		return nil, fmt.Errorf("parse base artifact: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("Mihomo artifact must be a YAML mapping")
	}
	root := document.Content[0]
	setScalar(root, "external-controller", controllerAddr)
	setScalar(root, "secret", secret)
	out, err := yaml.Marshal(&document)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func setScalar(mapping *yaml.Node, key, value string) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func (d *Deployer) Apply(ctx context.Context, revision, expectedHash string, artifact []byte) (DeploymentResult, error) {
	result := DeploymentResult{Revision: revision, Status: "failed"}
	if revision == "" || expectedHash == "" || len(artifact) == 0 {
		return result, errors.New("revision, artifact hash and body are required")
	}
	artifactHash := sha256.Sum256(artifact)
	result.ArtifactHash = hex.EncodeToString(artifactHash[:])
	if !strings.EqualFold(expectedHash, result.ArtifactHash) {
		return result, errors.New("artifact SHA-256 does not match the control plane metadata")
	}
	if d.Validator == nil || d.Service == nil || d.Verifier == nil {
		return result, errors.New("deployer dependencies are incomplete")
	}
	root, err := d.safeRoot()
	if err != nil {
		return result, err
	}
	effective, err := ApplyAgentOverlay(artifact, d.ControllerAddr, d.Secret)
	if err != nil {
		return result, err
	}
	effectiveHash := sha256.Sum256(effective)
	result.EffectiveHash = hex.EncodeToString(effectiveHash[:])
	result.ProxyPort, result.ProxyKind, err = ProxyEndpoint(effective)
	if err != nil {
		return result, err
	}
	current := filepath.Join(root, "current")
	lastGood := filepath.Join(root, "last-good")
	staging := filepath.Join(root, "staging")
	if err := removeManagedDir(root, staging); err != nil {
		return result, err
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return result, err
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = removeManagedDir(root, staging)
		}
	}()
	if err := os.WriteFile(filepath.Join(staging, "base.yaml"), artifact, 0600); err != nil {
		return result, err
	}
	configPath := filepath.Join(staging, "config.yaml")
	if err := os.WriteFile(configPath, effective, 0600); err != nil {
		return result, err
	}
	metadata := deploymentMetadata{Revision: revision, ArtifactHash: result.ArtifactHash, EffectiveHash: result.EffectiveHash, AppliedAt: time.Now().UTC().Format(time.RFC3339), ProxyPort: result.ProxyPort, ProxyKind: result.ProxyKind}
	metadataJSON, _ := json.Marshal(metadata)
	if err := os.WriteFile(filepath.Join(staging, "metadata.json"), metadataJSON, 0600); err != nil {
		return result, err
	}
	if err := d.Validator.ValidateConfig(ctx, configPath); err != nil {
		result.Validation, result.Error = "rejected", "Mihomo rejected the staged configuration"
		return result, fmt.Errorf("validate staged config: %w", err)
	}
	result.Validation = "passed"
	previous, _ := readMetadata(filepath.Join(current, "metadata.json"))
	result.PreviousRevision = previous.Revision
	if _, err := os.Stat(current); err == nil {
		if err := removeManagedDir(root, lastGood); err != nil {
			return result, err
		}
		if err := os.Rename(current, lastGood); err != nil {
			return result, fmt.Errorf("preserve last-good: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return result, err
	}
	if err := os.Rename(staging, current); err != nil {
		if _, statErr := os.Stat(lastGood); statErr == nil {
			_ = os.Rename(lastGood, current)
		}
		return result, fmt.Errorf("activate staged config: %w", err)
	}
	cleanupStaging = false
	activationError := d.Service.ReloadOrRestart(ctx)
	if activationError == nil {
		activationError = d.Verifier.VerifyRuntime(ctx, loopbackProxyAddress(result.ProxyPort))
	}
	if activationError == nil {
		result.Status = "active"
		return result, nil
	}
	result.RolledBack = true
	failed := filepath.Join(root, "failed")
	if removeErr := removeManagedDir(root, failed); removeErr != nil {
		return result, errors.Join(activationError, removeErr)
	}
	if renameErr := os.Rename(current, failed); renameErr != nil {
		return result, errors.Join(activationError, renameErr)
	}
	if _, statErr := os.Stat(lastGood); statErr == nil {
		if renameErr := os.Rename(lastGood, current); renameErr != nil {
			return result, errors.Join(activationError, renameErr)
		}
		rollbackErr := d.Service.ReloadOrRestart(ctx)
		if rollbackErr == nil {
			rollbackErr = d.Verifier.VerifyRuntime(ctx, loopbackProxyAddress(previous.ProxyPort))
		}
		_ = removeManagedDir(root, failed)
		if rollbackErr != nil {
			result.Status, result.Error = "failed", "candidate failed and last-good could not be verified"
			return result, errors.Join(activationError, rollbackErr)
		}
		result.Status, result.Error = "rolled_back", "candidate failed runtime verification; last-good restored"
		return result, fmt.Errorf("candidate activation failed and was rolled back: %w", activationError)
	}
	_ = d.Service.Stop(ctx)
	result.Status, result.Error = "failed", "first deployment failed runtime verification; Mihomo stopped"
	return result, fmt.Errorf("first candidate activation failed: %w", activationError)
}

func (d *Deployer) Rollback(ctx context.Context) (DeploymentResult, error) {
	result := DeploymentResult{Status: "failed", RolledBack: true}
	root, err := d.safeRoot()
	if err != nil {
		return result, err
	}
	current, lastGood := filepath.Join(root, "current"), filepath.Join(root, "last-good")
	if _, err := os.Stat(lastGood); err != nil {
		return result, errors.New("no last-good configuration is available")
	}
	currentMetadata, err := readMetadata(filepath.Join(current, "metadata.json"))
	if err != nil {
		return result, errors.New("current configuration metadata is unavailable")
	}
	targetMetadata, err := readMetadata(filepath.Join(lastGood, "metadata.json"))
	if err != nil {
		return result, errors.New("last-good configuration metadata is unavailable")
	}
	result.Revision, result.PreviousRevision = targetMetadata.Revision, currentMetadata.Revision
	result.ArtifactHash, result.EffectiveHash = targetMetadata.ArtifactHash, targetMetadata.EffectiveHash
	result.ProxyPort, result.ProxyKind = targetMetadata.ProxyPort, targetMetadata.ProxyKind
	result.Validation = "passed"
	failed := filepath.Join(root, "failed")
	if err := removeManagedDir(root, failed); err != nil {
		return result, err
	}
	if err := os.Rename(current, failed); err != nil {
		return result, err
	}
	if err := os.Rename(lastGood, current); err != nil {
		_ = os.Rename(failed, current)
		return result, err
	}
	activationErr := d.Service.ReloadOrRestart(ctx)
	if activationErr == nil {
		activationErr = d.Verifier.VerifyRuntime(ctx, loopbackProxyAddress(result.ProxyPort))
	}
	if activationErr != nil {
		badTarget := filepath.Join(root, "last-good")
		if err := os.Rename(current, badTarget); err != nil {
			return result, errors.Join(activationErr, err)
		}
		if err := os.Rename(failed, current); err != nil {
			return result, errors.Join(activationErr, err)
		}
		restoreErr := d.Service.ReloadOrRestart(ctx)
		if restoreErr == nil {
			restoreErr = d.Verifier.VerifyRuntime(ctx, loopbackProxyAddress(currentMetadata.ProxyPort))
		}
		if restoreErr != nil {
			return result, errors.Join(activationErr, restoreErr)
		}
		result.Error = "last-good rollback failed runtime verification; original current restored"
		return result, fmt.Errorf("last-good activation failed and current was restored: %w", activationErr)
	}
	if err := os.Rename(failed, lastGood); err != nil {
		return result, err
	}
	result.Status, result.Error = "active", ""
	return result, nil
}

func loopbackProxyAddress(port int) string {
	if port <= 0 {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
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

func removeManagedDir(root, path string) error {
	if filepath.Dir(path) != root {
		return errors.New("refusing to remove a path outside the deployment root")
	}
	base := filepath.Base(path)
	if base != "staging" && base != "last-good" && base != "failed" {
		return fmt.Errorf("refusing to remove unmanaged directory %q", base)
	}
	return os.RemoveAll(path)
}

func readMetadata(path string) (deploymentMetadata, error) {
	var value deploymentMetadata
	raw, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	err = json.Unmarshal(raw, &value)
	return value, err
}
