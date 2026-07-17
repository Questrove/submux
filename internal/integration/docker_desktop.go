package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const DockerDesktopType = "docker_desktop"

type DockerDesktopConfig struct {
	Enabled               bool     `json:"enabled"`
	ProxyPort             int      `json:"proxy_port"`
	NoProxy               []string `json:"no_proxy,omitempty"`
	Revision              string   `json:"revision,omitempty"`
	ExpectedOriginalHash  string   `json:"expected_original_hash,omitempty"`
	BusinessAdminSettings bool     `json:"business_admin_settings"`
}

func (c DockerDesktopConfig) Validate() error {
	base := DockerDaemonConfig{Enabled: c.Enabled, ProxyPort: c.ProxyPort, NoProxy: c.NoProxy, Revision: c.Revision, ExpectedOriginalHash: c.ExpectedOriginalHash}
	if err := base.Validate(); err != nil {
		return err
	}
	if !c.BusinessAdminSettings {
		return errors.New("Docker Desktop proxy management requires explicit confirmation of Docker Business admin-settings prerequisites")
	}
	return nil
}

type DockerDesktopManager interface {
	Status(context.Context) (DockerStatus, error)
	Preview(context.Context, DockerDesktopConfig) (DockerPreview, error)
	Enable(context.Context, DockerDesktopConfig, string) (DockerStatus, error)
	Disable(context.Context) (DockerStatus, error)
}

type DockerDesktopOps interface {
	ReadConfig(context.Context) ([]byte, bool, error)
	WriteConfig(context.Context, []byte) error
	RemoveConfig(context.Context) error
	ReadOwnership(context.Context) ([]byte, bool, error)
	WriteOwnership(context.Context, []byte) error
	RemoveOwnership(context.Context) error
	RestartAndVerify(context.Context) error
}

type DesktopManager struct{ Ops DockerDesktopOps }

func (m *DesktopManager) Status(ctx context.Context) (DockerStatus, error) {
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
		status.Conflict = "Docker Desktop admin-settings.json changed outside submux-agent after the proxy integration was applied"
	}
	return status, nil
}

func (m *DesktopManager) Preview(ctx context.Context, config DockerDesktopConfig) (DockerPreview, error) {
	if m == nil || m.Ops == nil {
		return DockerPreview{}, errors.New("Docker Desktop integration is unavailable")
	}
	if err := config.Validate(); err != nil {
		return DockerPreview{}, err
	}
	current, _, err := m.Ops.ReadConfig(ctx)
	if err != nil {
		return DockerPreview{}, err
	}
	desired, _, _, err := mergeDockerDesktop(current, config)
	if err != nil {
		return DockerPreview{}, err
	}
	return DockerPreview{
		State: "previewed", Before: printableDocument(current), After: string(desired),
		OriginalHash: hashDocument(current), DesiredHash: hashDocument(desired), RestartRequired: !bytes.Equal(current, desired),
		Warning: "Docker Desktop Settings Management requires Docker Business, enforced organization sign-in, a full Desktop restart, and a signed-in user. Restarting may interrupt Docker operations.",
	}, nil
}

func (m *DesktopManager) Enable(ctx context.Context, config DockerDesktopConfig, expectedOriginalHash string) (DockerStatus, error) {
	if existing, exists, err := m.readOwnership(ctx); err != nil {
		return DockerStatus{}, err
	} else if exists {
		if expectedOriginalHash == "" || expectedOriginalHash != existing.OriginalHash {
			return DockerStatus{State: "conflict", Conflict: "confirmed preview does not match the active Docker Desktop ownership state"}, errors.New("Docker Desktop settings no longer match the confirmed preview")
		}
		current, _, readErr := m.Ops.ReadConfig(ctx)
		if readErr != nil {
			return DockerStatus{}, readErr
		}
		desired, _, _, mergeErr := mergeDockerDesktop(current, config)
		if mergeErr != nil || hashDocument(desired) != existing.AppliedHash {
			return DockerStatus{State: "conflict", Conflict: "disable the existing Docker Desktop integration before changing settings"}, errors.New("Docker Desktop ownership does not match the requested settings")
		}
		currentHash := hashDocument(current)
		if existing.Phase == "applying" {
			return m.resumeApply(ctx, config, current, existing)
		}
		if currentHash != existing.AppliedHash {
			return DockerStatus{State: "conflict", Conflict: "Docker Desktop settings have external changes"}, errors.New("refusing to overwrite externally modified Docker Desktop settings")
		}
		return ownershipStatus(existing), nil
	}
	preview, err := m.Preview(ctx, config)
	if err != nil {
		return DockerStatus{}, err
	}
	if expectedOriginalHash == "" || expectedOriginalHash != preview.OriginalHash {
		return DockerStatus{State: "conflict", Conflict: "Docker Desktop settings changed after preview"}, errors.New("Docker Desktop settings no longer match the confirmed preview")
	}
	current, existed, err := m.Ops.ReadConfig(ctx)
	if err != nil {
		return DockerStatus{}, err
	}
	desired, original, originalSet, err := mergeDockerDesktop(current, config)
	if err != nil {
		return DockerStatus{}, err
	}
	if len(original) > maxSavedProxyValue {
		return DockerStatus{}, errors.New("existing Docker Desktop containersProxy value is too large for bounded recovery state")
	}
	ownership := dockerOwnership{
		Phase: "applying", OriginalHash: hashDocument(current), AppliedHash: hashDocument(desired), OriginalExisted: existed,
		OriginalProxySet: originalSet, OriginalProxies: original, ProxyURL: proxyURL(config.ProxyPort), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.writeOwnership(ctx, ownership); err != nil {
		return DockerStatus{}, err
	}
	if err := m.Ops.WriteConfig(ctx, desired); err != nil {
		_ = m.Ops.RemoveOwnership(ctx)
		return DockerStatus{}, err
	}
	if err := m.Ops.RestartAndVerify(ctx); err != nil {
		_ = m.restoreDocument(ctx, current, existed)
		_ = m.Ops.RestartAndVerify(ctx)
		_ = m.Ops.RemoveOwnership(ctx)
		return DockerStatus{State: "failed"}, fmt.Errorf("Docker Desktop proxy activation failed and settings were restored: %w", err)
	}
	ownership.Phase, ownership.UpdatedAt = "active", time.Now().UTC().Format(time.RFC3339)
	if err := m.writeOwnership(ctx, ownership); err != nil {
		return DockerStatus{}, err
	}
	return ownershipStatus(ownership), nil
}

func (m *DesktopManager) resumeApply(ctx context.Context, config DockerDesktopConfig, current []byte, ownership dockerOwnership) (DockerStatus, error) {
	currentHash := hashDocument(current)
	if currentHash == ownership.OriginalHash {
		desired, _, _, err := mergeDockerDesktop(current, config)
		if err != nil || hashDocument(desired) != ownership.AppliedHash {
			return DockerStatus{State: "conflict", Conflict: "interrupted Docker Desktop ownership state does not match the confirmed candidate"}, errors.New("interrupted Docker Desktop candidate is inconsistent")
		}
		if err := m.Ops.WriteConfig(ctx, desired); err != nil {
			return DockerStatus{State: "applying"}, err
		}
		current = desired
		currentHash = ownership.AppliedHash
	}
	if currentHash != ownership.AppliedHash {
		return DockerStatus{State: "conflict", Conflict: "Docker Desktop settings changed during an interrupted apply"}, errors.New("refusing to resume over externally modified Docker Desktop settings")
	}
	if err := m.Ops.RestartAndVerify(ctx); err != nil {
		restored, restoreErr := restoreDockerDesktop(current, ownership)
		if restoreErr == nil {
			restoreErr = m.Ops.WriteConfig(ctx, restored)
		}
		if restoreErr == nil {
			restoreErr = m.Ops.RestartAndVerify(ctx)
		}
		if restoreErr == nil && !ownership.OriginalExisted && isEmptyJSONObject(restored) {
			restoreErr = m.Ops.RemoveConfig(ctx)
		}
		if restoreErr == nil {
			restoreErr = m.Ops.RemoveOwnership(ctx)
		}
		return DockerStatus{State: "failed"}, errors.Join(errors.New("interrupted Docker Desktop activation failed and was restored"), err, restoreErr)
	}
	ownership.Phase, ownership.UpdatedAt = "active", time.Now().UTC().Format(time.RFC3339)
	if err := m.writeOwnership(ctx, ownership); err != nil {
		return DockerStatus{State: "applying"}, err
	}
	return ownershipStatus(ownership), nil
}

func (m *DesktopManager) Disable(ctx context.Context) (DockerStatus, error) {
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
		return DockerStatus{State: "conflict", Conflict: "Docker Desktop settings have external changes"}, errors.New("refusing to restore an old value over externally modified Docker Desktop settings")
	}
	restored, err := restoreDockerDesktop(current, ownership)
	if err != nil {
		return DockerStatus{}, err
	}
	if err := m.Ops.WriteConfig(ctx, restored); err != nil {
		return DockerStatus{}, err
	}
	if err := m.Ops.RestartAndVerify(ctx); err != nil {
		_ = m.Ops.WriteConfig(ctx, current)
		_ = m.Ops.RestartAndVerify(ctx)
		return DockerStatus{State: "failed"}, fmt.Errorf("Docker Desktop proxy disable failed and the managed value was restored: %w", err)
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

func (m *DesktopManager) readOwnership(ctx context.Context) (dockerOwnership, bool, error) {
	if m == nil || m.Ops == nil {
		return dockerOwnership{}, false, errors.New("Docker Desktop integration is unavailable")
	}
	raw, exists, err := m.Ops.ReadOwnership(ctx)
	if err != nil || !exists {
		return dockerOwnership{}, exists, err
	}
	var value dockerOwnership
	if err := strictJSON(raw, &value); err != nil || value.OriginalHash == "" || value.AppliedHash == "" || (value.Phase != "applying" && value.Phase != "active") {
		return dockerOwnership{}, false, errors.New("Docker Desktop integration ownership state is invalid")
	}
	return value, true, nil
}

func (m *DesktopManager) writeOwnership(ctx context.Context, value dockerOwnership) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return m.Ops.WriteOwnership(ctx, raw)
}

func (m *DesktopManager) restoreDocument(ctx context.Context, value []byte, existed bool) error {
	if !existed {
		return m.Ops.RemoveConfig(ctx)
	}
	return m.Ops.WriteConfig(ctx, value)
}

func mergeDockerDesktop(raw []byte, config DockerDesktopConfig) ([]byte, json.RawMessage, bool, error) {
	document, err := dockerObject(raw)
	if err != nil {
		return nil, nil, false, err
	}
	if versionRaw, exists := document["configurationFileVersion"]; exists {
		var version int
		if err := json.Unmarshal(versionRaw, &version); err != nil || version < 2 {
			return nil, nil, false, errors.New("Docker Desktop admin-settings.json must use configurationFileVersion 2 or later")
		}
	} else {
		document["configurationFileVersion"] = json.RawMessage("2")
	}
	original, originalSet := document["containersProxy"]
	noProxy := append([]string(nil), config.NoProxy...)
	if len(noProxy) == 0 {
		noProxy = append(noProxy, DefaultDockerNoProxy...)
	}
	proxy := proxyURL(config.ProxyPort)
	value, _ := json.Marshal(map[string]any{
		"locked": true, "mode": "manual", "http": proxy, "https": proxy, "exclude": uniqueSorted(noProxy),
		"pac": "", "embeddedPac": "", "transparentPorts": "",
	})
	document["containersProxy"] = value
	result, err := json.MarshalIndent(document, "", "  ")
	return append(result, '\n'), append(json.RawMessage(nil), original...), originalSet, err
}

func restoreDockerDesktop(raw []byte, ownership dockerOwnership) ([]byte, error) {
	document, err := dockerObject(raw)
	if err != nil {
		return nil, err
	}
	if ownership.OriginalProxySet {
		if !json.Valid(ownership.OriginalProxies) {
			return nil, errors.New("saved Docker Desktop containersProxy value is invalid")
		}
		document["containersProxy"] = append(json.RawMessage(nil), ownership.OriginalProxies...)
	} else {
		delete(document, "containersProxy")
		if len(document) == 1 && strings.TrimSpace(string(document["configurationFileVersion"])) == "2" && !ownership.OriginalExisted {
			delete(document, "configurationFileVersion")
		}
	}
	result, err := json.MarshalIndent(document, "", "  ")
	return append(result, '\n'), err
}
