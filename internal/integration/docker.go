package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

const DockerDaemonType = "docker_daemon"

const maxSavedProxyValue = 32 << 10

var DefaultDockerNoProxy = []string{
	"localhost", "127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
}

type DockerDaemonConfig struct {
	Enabled              bool     `json:"enabled"`
	ProxyPort            int      `json:"proxy_port"`
	NoProxy              []string `json:"no_proxy,omitempty"`
	Revision             string   `json:"revision,omitempty"`
	ExpectedOriginalHash string   `json:"expected_original_hash,omitempty"`
}

func (c DockerDaemonConfig) Validate() error {
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return errors.New("Docker daemon proxy_port must be between 1 and 65535")
	}
	if len(c.NoProxy) > 64 || len(c.Revision) > 128 || len(c.ExpectedOriginalHash) > 64 {
		return errors.New("Docker daemon integration values exceed their limits")
	}
	for _, value := range c.NoProxy {
		if value == "" || len(value) > 255 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("Docker daemon no_proxy contains an invalid entry")
		}
	}
	return nil
}

type DockerPreview struct {
	State           string `json:"state"`
	Before          string `json:"before"`
	After           string `json:"after"`
	OriginalHash    string `json:"original_hash"`
	DesiredHash     string `json:"desired_hash"`
	RestartRequired bool   `json:"restart_required"`
	Warning         string `json:"warning"`
}

type DockerStatus struct {
	State        string `json:"state"`
	OriginalHash string `json:"original_hash,omitempty"`
	AppliedHash  string `json:"applied_hash,omitempty"`
	Conflict     string `json:"conflict,omitempty"`
	ProxyURL     string `json:"proxy_url,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

type dockerOwnership struct {
	Phase            string          `json:"phase"`
	OriginalHash     string          `json:"original_hash"`
	AppliedHash      string          `json:"applied_hash"`
	OriginalExisted  bool            `json:"original_existed"`
	OriginalProxySet bool            `json:"original_proxy_set"`
	OriginalProxies  json.RawMessage `json:"original_proxies,omitempty"`
	ProxyURL         string          `json:"proxy_url"`
	UpdatedAt        string          `json:"updated_at"`
}

type DockerFileOps interface {
	ReadConfig(context.Context) ([]byte, bool, error)
	WriteConfig(context.Context, []byte) error
	RemoveConfig(context.Context) error
	ReadOwnership(context.Context) ([]byte, bool, error)
	WriteOwnership(context.Context, []byte) error
	RemoveOwnership(context.Context) error
	Validate(context.Context) error
	Restart(context.Context) error
	InspectProxy(context.Context) (string, error)
}

type DockerDaemonManager interface {
	Status(context.Context) (DockerStatus, error)
	Preview(context.Context, DockerDaemonConfig) (DockerPreview, error)
	Enable(context.Context, DockerDaemonConfig, string) (DockerStatus, error)
	Disable(context.Context) (DockerStatus, error)
}

type DockerManager struct{ Ops DockerFileOps }

func (m *DockerManager) Status(ctx context.Context) (DockerStatus, error) {
	ownership, exists, err := m.readOwnership(ctx)
	if err != nil {
		return DockerStatus{}, err
	}
	if !exists {
		return DockerStatus{State: "disabled"}, nil
	}
	current, _, err := m.Ops.ReadConfig(ctx)
	if err != nil {
		return DockerStatus{}, err
	}
	status := ownershipStatus(ownership)
	currentHash := hashDocument(current)
	if currentHash != ownership.AppliedHash && !(ownership.Phase == "applying" && currentHash == ownership.OriginalHash) {
		status.State = "conflict"
		status.Conflict = "Docker daemon.json changed outside submux-agent after the proxy integration was applied"
	}
	return status, nil
}

func (m *DockerManager) Preview(ctx context.Context, config DockerDaemonConfig) (DockerPreview, error) {
	if m == nil || m.Ops == nil {
		return DockerPreview{}, errors.New("Docker daemon integration is unavailable")
	}
	if err := config.Validate(); err != nil {
		return DockerPreview{}, err
	}
	current, _, err := m.Ops.ReadConfig(ctx)
	if err != nil {
		return DockerPreview{}, err
	}
	desired, _, _, err := mergeDockerDaemon(current, config)
	if err != nil {
		return DockerPreview{}, err
	}
	return DockerPreview{
		State: "previewed", Before: printableDocument(current), After: string(desired),
		OriginalHash: hashDocument(current), DesiredHash: hashDocument(desired), RestartRequired: !bytes.Equal(current, desired),
		Warning: "Applying this change restarts Docker Engine and may interrupt running Docker operations.",
	}, nil
}

func (m *DockerManager) Enable(ctx context.Context, config DockerDaemonConfig, expectedOriginalHash string) (DockerStatus, error) {
	if existing, exists, err := m.readOwnership(ctx); err != nil {
		return DockerStatus{}, err
	} else if exists {
		if expectedOriginalHash == "" || expectedOriginalHash != existing.OriginalHash {
			return DockerStatus{State: "conflict", Conflict: "confirmed preview does not match the active Docker ownership state"}, errors.New("Docker daemon configuration no longer matches the confirmed preview")
		}
		current, _, readErr := m.Ops.ReadConfig(ctx)
		if readErr != nil {
			return DockerStatus{}, readErr
		}
		desired, _, _, mergeErr := mergeDockerDaemon(current, config)
		if mergeErr != nil || hashDocument(desired) != existing.AppliedHash {
			return DockerStatus{State: "conflict", Conflict: "disable the existing Docker integration before changing settings"}, errors.New("Docker integration ownership does not match the requested settings")
		}
		currentHash := hashDocument(current)
		if existing.Phase == "applying" {
			return m.resumeApply(ctx, config, current, existing)
		}
		if currentHash != existing.AppliedHash {
			return DockerStatus{State: "conflict", Conflict: "Docker daemon.json has external changes"}, errors.New("refusing to overwrite externally modified Docker daemon configuration")
		}
		return ownershipStatus(existing), nil
	}
	preview, err := m.Preview(ctx, config)
	if err != nil {
		return DockerStatus{}, err
	}
	if expectedOriginalHash == "" || expectedOriginalHash != preview.OriginalHash {
		return DockerStatus{State: "conflict", Conflict: "Docker daemon.json changed after preview"}, errors.New("Docker daemon configuration no longer matches the confirmed preview")
	}
	current, existed, err := m.Ops.ReadConfig(ctx)
	if err != nil {
		return DockerStatus{}, err
	}
	desired, originalProxies, originalProxySet, err := mergeDockerDaemon(current, config)
	if err != nil {
		return DockerStatus{}, err
	}
	if len(originalProxies) > maxSavedProxyValue {
		return DockerStatus{}, errors.New("existing Docker proxies value is too large for bounded recovery state")
	}
	proxyURL := proxyURL(config.ProxyPort)
	ownership := dockerOwnership{
		Phase: "applying", OriginalHash: hashDocument(current), AppliedHash: hashDocument(desired), OriginalExisted: existed,
		OriginalProxySet: originalProxySet, OriginalProxies: originalProxies, ProxyURL: proxyURL, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.writeOwnership(ctx, ownership); err != nil {
		return DockerStatus{}, err
	}
	if err := m.Ops.WriteConfig(ctx, desired); err != nil {
		_ = m.Ops.RemoveOwnership(ctx)
		return DockerStatus{}, err
	}
	if err := m.validateRestartVerify(ctx, proxyURL); err != nil {
		_ = m.restoreDocument(ctx, current, existed)
		_ = m.Ops.Restart(ctx)
		_ = m.Ops.RemoveOwnership(ctx)
		return DockerStatus{State: "failed"}, fmt.Errorf("Docker daemon proxy activation failed and was restored: %w", err)
	}
	ownership.Phase = "active"
	ownership.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := m.writeOwnership(ctx, ownership); err != nil {
		return DockerStatus{}, err
	}
	return ownershipStatus(ownership), nil
}

func (m *DockerManager) resumeApply(ctx context.Context, config DockerDaemonConfig, current []byte, ownership dockerOwnership) (DockerStatus, error) {
	currentHash := hashDocument(current)
	if currentHash == ownership.OriginalHash {
		desired, _, _, err := mergeDockerDaemon(current, config)
		if err != nil || hashDocument(desired) != ownership.AppliedHash {
			return DockerStatus{State: "conflict", Conflict: "interrupted Docker ownership state does not match the confirmed candidate"}, errors.New("interrupted Docker candidate is inconsistent")
		}
		if err := m.Ops.WriteConfig(ctx, desired); err != nil {
			return DockerStatus{State: "applying"}, err
		}
		current = desired
		currentHash = ownership.AppliedHash
	}
	if currentHash != ownership.AppliedHash {
		return DockerStatus{State: "conflict", Conflict: "Docker daemon.json changed during an interrupted apply"}, errors.New("refusing to resume over externally modified Docker configuration")
	}
	if err := m.validateRestartVerify(ctx, ownership.ProxyURL); err != nil {
		restored, restoreErr := restoreDockerProxies(current, ownership)
		if restoreErr == nil {
			restoreErr = m.Ops.WriteConfig(ctx, restored)
		}
		if restoreErr == nil {
			restoreErr = m.Ops.Restart(ctx)
		}
		if restoreErr == nil && !ownership.OriginalExisted && isEmptyJSONObject(restored) {
			restoreErr = m.Ops.RemoveConfig(ctx)
		}
		if restoreErr == nil {
			restoreErr = m.Ops.RemoveOwnership(ctx)
		}
		return DockerStatus{State: "failed"}, errors.Join(errors.New("interrupted Docker activation failed and was restored"), err, restoreErr)
	}
	ownership.Phase, ownership.UpdatedAt = "active", time.Now().UTC().Format(time.RFC3339)
	if err := m.writeOwnership(ctx, ownership); err != nil {
		return DockerStatus{State: "applying"}, err
	}
	return ownershipStatus(ownership), nil
}

func (m *DockerManager) Disable(ctx context.Context) (DockerStatus, error) {
	ownership, exists, err := m.readOwnership(ctx)
	if err != nil || !exists {
		return DockerStatus{State: "disabled"}, err
	}
	current, _, err := m.Ops.ReadConfig(ctx)
	if err != nil {
		return DockerStatus{}, err
	}
	currentHash := hashDocument(current)
	if ownership.Phase == "applying" && currentHash == ownership.OriginalHash {
		if err := m.Ops.RemoveOwnership(ctx); err != nil {
			return DockerStatus{}, err
		}
		return DockerStatus{State: "disabled", UpdatedAt: time.Now().UTC().Format(time.RFC3339)}, nil
	}
	if currentHash != ownership.AppliedHash {
		return DockerStatus{State: "conflict", Conflict: "Docker daemon.json has external changes"}, errors.New("refusing to restore an old proxy value over externally modified Docker configuration")
	}
	restored, err := restoreDockerProxies(current, ownership)
	if err != nil {
		return DockerStatus{}, err
	}
	if err := m.Ops.WriteConfig(ctx, restored); err != nil {
		return DockerStatus{}, err
	}
	expectedProxy, err := dockerHTTPProxy(restored)
	if err != nil {
		return DockerStatus{}, err
	}
	if err := m.validateRestartVerify(ctx, expectedProxy); err != nil {
		_ = m.Ops.WriteConfig(ctx, current)
		_ = m.Ops.Restart(ctx)
		return DockerStatus{State: "failed"}, fmt.Errorf("Docker daemon proxy disable failed and the managed value was restored: %w", err)
	}
	if !ownership.OriginalExisted && isEmptyJSONObject(restored) {
		if err := m.Ops.RemoveConfig(ctx); err != nil {
			return DockerStatus{}, err
		}
	}
	if err := m.Ops.RemoveOwnership(ctx); err != nil {
		return DockerStatus{}, err
	}
	return DockerStatus{State: "disabled", UpdatedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

func (m *DockerManager) validateRestartVerify(ctx context.Context, expectedProxy string) error {
	if err := m.Ops.Validate(ctx); err != nil {
		return errors.New("Docker rejected the candidate daemon configuration")
	}
	if err := m.Ops.Restart(ctx); err != nil {
		return errors.New("Docker service restart failed")
	}
	observed, err := m.Ops.InspectProxy(ctx)
	if err != nil {
		return errors.New("Docker proxy status verification failed")
	}
	if observed != expectedProxy {
		return fmt.Errorf("Docker reported proxy %q instead of the expected value", observed)
	}
	return nil
}

func (m *DockerManager) restoreDocument(ctx context.Context, value []byte, existed bool) error {
	if !existed {
		return m.Ops.RemoveConfig(ctx)
	}
	return m.Ops.WriteConfig(ctx, value)
}

func (m *DockerManager) readOwnership(ctx context.Context) (dockerOwnership, bool, error) {
	if m == nil || m.Ops == nil {
		return dockerOwnership{}, false, errors.New("Docker daemon integration is unavailable")
	}
	raw, exists, err := m.Ops.ReadOwnership(ctx)
	if err != nil || !exists {
		return dockerOwnership{}, exists, err
	}
	var value dockerOwnership
	if err := strictJSON(raw, &value); err != nil || value.OriginalHash == "" || value.AppliedHash == "" || (value.Phase != "applying" && value.Phase != "active") {
		return dockerOwnership{}, false, errors.New("Docker integration ownership state is invalid")
	}
	return value, true, nil
}

func (m *DockerManager) writeOwnership(ctx context.Context, value dockerOwnership) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return m.Ops.WriteOwnership(ctx, raw)
}

func mergeDockerDaemon(raw []byte, config DockerDaemonConfig) ([]byte, json.RawMessage, bool, error) {
	document, err := dockerObject(raw)
	if err != nil {
		return nil, nil, false, err
	}
	original, originalSet := document["proxies"]
	noProxy := append([]string(nil), config.NoProxy...)
	if len(noProxy) == 0 {
		noProxy = append(noProxy, DefaultDockerNoProxy...)
	}
	noProxy = uniqueSorted(noProxy)
	proxy := proxyURL(config.ProxyPort)
	proxies, _ := json.Marshal(map[string]string{"http-proxy": proxy, "https-proxy": proxy, "no-proxy": strings.Join(noProxy, ",")})
	document["proxies"] = proxies
	result, err := json.MarshalIndent(document, "", "  ")
	return append(result, '\n'), append(json.RawMessage(nil), original...), originalSet, err
}

func restoreDockerProxies(raw []byte, ownership dockerOwnership) ([]byte, error) {
	document, err := dockerObject(raw)
	if err != nil {
		return nil, err
	}
	if ownership.OriginalProxySet {
		if !json.Valid(ownership.OriginalProxies) {
			return nil, errors.New("saved Docker proxy value is invalid")
		}
		document["proxies"] = append(json.RawMessage(nil), ownership.OriginalProxies...)
	} else {
		delete(document, "proxies")
	}
	result, err := json.MarshalIndent(document, "", "  ")
	return append(result, '\n'), err
}

func dockerObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var document map[string]json.RawMessage
	if err := strictJSON(raw, &document); err != nil || document == nil {
		return nil, errors.New("Docker daemon.json must contain one valid JSON object")
	}
	return document, nil
}

func dockerHTTPProxy(raw []byte) (string, error) {
	document, err := dockerObject(raw)
	if err != nil {
		return "", err
	}
	proxiesRaw, exists := document["proxies"]
	if !exists {
		return "", nil
	}
	var proxies map[string]json.RawMessage
	if err := strictJSON(proxiesRaw, &proxies); err != nil {
		return "", errors.New("Docker proxies setting must be an object")
	}
	valueRaw, exists := proxies["http-proxy"]
	if !exists {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(valueRaw, &value); err != nil {
		return "", errors.New("Docker http-proxy setting must be a string")
	}
	return normalizeProxyURL(value), nil
}

func strictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return nil
}

func proxyURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

func hashDocument(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func printableDocument(value []byte) string {
	if len(bytes.TrimSpace(value)) == 0 {
		return "{}\n"
	}
	document, err := dockerObject(value)
	if err != nil {
		return string(value)
	}
	raw, _ := json.MarshalIndent(document, "", "  ")
	return string(append(raw, '\n'))
}

func ownershipStatus(value dockerOwnership) DockerStatus {
	return DockerStatus{State: value.Phase, OriginalHash: value.OriginalHash, AppliedHash: value.AppliedHash, ProxyURL: value.ProxyURL, UpdatedAt: value.UpdatedAt}
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func isEmptyJSONObject(value []byte) bool {
	document, err := dockerObject(value)
	return err == nil && len(document) == 0
}

func normalizeProxyURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	return strings.TrimRight(parsed.String(), "/")
}
