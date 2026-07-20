package agentproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Version = 1

const (
	ResourceProxyDirect = "direct"
	ResourceProxyCustom = "custom"
)

// ResourceProxy controls only Agent-owned downloads from built-in official
// resource coordinates. It never changes the control-plane client, Mihomo
// traffic, subscription downloads, or another application's proxy settings.
type ResourceProxy struct {
	Mode string `json:"mode"`
	URL  string `json:"url,omitempty"`
}

func NormalizeResourceProxy(value ResourceProxy) ResourceProxy {
	value.Mode = strings.TrimSpace(value.Mode)
	value.URL = strings.TrimSpace(value.URL)
	if value.Mode == "" {
		value.Mode = ResourceProxyDirect
	}
	return value
}

func ValidateResourceProxy(value ResourceProxy) error {
	value = NormalizeResourceProxy(value)
	switch value.Mode {
	case ResourceProxyDirect:
		if value.URL != "" {
			return errors.New("direct download mode does not accept a proxy URL")
		}
		return nil
	case ResourceProxyCustom:
		if value.URL == "" {
			return errors.New("custom download proxy URL is required")
		}
	default:
		return errors.New("Agent resource proxy mode must be direct or custom")
	}
	parsed, err := url.Parse(value.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "socks5") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("custom Agent resource proxy must be an HTTP or SOCKS5 URL without credentials, path, query, or fragment")
	}
	host := parsed.Hostname()
	port, err := strconv.Atoi(parsed.Port())
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "\r\n\x00") || err != nil || port < 1 || port > 65535 {
		return errors.New("custom Agent resource proxy must include a valid port")
	}
	return nil
}

const (
	ActorAdminSession = "admin_session"
	ActorLocalCLI     = "local_cli"
	ActorAgent        = "agent"
)

var knownCapabilities = map[string]bool{
	"subscription.manage":     true,
	"mihomo.core.manage":      true,
	"mihomo.restart":          true,
	"mihomo.proxy.delay":      true,
	"mihomo.proxy.select":     true,
	"mihomo.connection.close": true,
	"mihomo.runtime.observe":  true,
	"agent.runtime.observe":   true,
	"mihomo.release.list":     true,
	"agent.resource.proxy":    true,
}

func ValidateCapabilities(values []string) error {
	if len(values) > 64 {
		return errors.New("too many capabilities")
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !knownCapabilities[value] {
			return fmt.Errorf("unknown capability %q", value)
		}
		if seen[value] {
			return fmt.Errorf("duplicate capability %q", value)
		}
		seen[value] = true
	}
	return nil
}

const (
	JobAddRuntimeSubscription      = "add_runtime_subscription"
	JobEditRuntimeSubscription     = "edit_runtime_subscription"
	JobDeleteRuntimeSubscription   = "delete_runtime_subscription"
	JobRefreshRuntimeSubscription  = "refresh_runtime_subscription"
	JobActivateRuntimeSubscription = "activate_runtime_subscription"
	JobConfigureResourceProxy      = "configure_resource_proxy"
	JobListCoreVersions            = "list_core_versions"
	JobInstallCore                 = "install_core"
	JobUninstallCore               = "uninstall_core"
	JobStartCore                   = "start_core"
	JobStopCore                    = "stop_core"
	JobRestartCore                 = "restart_core"
	JobRollbackCore                = "rollback_core"
	JobTestProxyDelay              = "test_proxy_delay"
	JobSelectProxy                 = "select_proxy"
	JobCloseConnection             = "close_connection"
)

const (
	JobQueued         = "queued"
	JobAccepted       = "accepted"
	JobRunning        = "running"
	JobSucceeded      = "succeeded"
	JobFailed         = "failed"
	JobOutcomeUnknown = "outcome_unknown"
	JobExpired        = "expired"
	JobCancelled      = "cancelled"
)

type Job struct {
	ID              string          `json:"id"`
	ProtocolVersion int             `json:"protocol_version"`
	InstanceID      int64           `json:"instance_id"`
	Type            string          `json:"type"`
	Params          json.RawMessage `json:"params"`
	Status          string          `json:"status"`
	ActorType       string          `json:"actor_type"`
	RequestID       string          `json:"request_id"`
	AuditReason     string          `json:"audit_reason,omitempty"`
	Deadline        string          `json:"deadline"`
}

type EmptyParams struct{}
type AddRuntimeSubscriptionParams struct {
	Name                   string `json:"name"`
	SecretRef              string `json:"secret_ref,omitempty"`
	PlatformSubscriptionID int64  `json:"platform_subscription_id,omitempty"`
}
type EditRuntimeSubscriptionParams struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	SecretRef              string `json:"secret_ref,omitempty"`
	PlatformSubscriptionID int64  `json:"platform_subscription_id,omitempty"`
}
type RuntimeSubscriptionIDParams struct {
	ID string `json:"id"`
}
type ConfigureResourceProxyParams struct {
	ResourceProxy ResourceProxy `json:"resource_proxy"`
}
type ListCoreVersionsParams struct {
	Channel string `json:"channel"`
}
type InstallCoreParams struct {
	Channel string `json:"channel"`
	Version string `json:"version"`
}
type TestProxyDelayParams struct {
	Group     string `json:"group"`
	Proxy     string `json:"proxy"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}
type SelectProxyParams struct {
	Group string `json:"group"`
	Proxy string `json:"proxy"`
}
type CloseConnectionParams struct {
	ConnectionID string `json:"connection_id"`
}
type RuntimeSubscriptionSummary struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	Host                   string `json:"host"`
	PlatformSubscriptionID int64  `json:"platform_subscription_id,omitempty"`
	Revision               string `json:"revision,omitempty"`
	UsedBytes              int64  `json:"used_bytes,omitempty"`
	TotalBytes             int64  `json:"total_bytes,omitempty"`
	ExpiresAt              string `json:"expires_at,omitempty"`
	LastUpdatedAt          string `json:"last_updated_at,omitempty"`
	LastError              string `json:"last_error,omitempty"`
	Active                 bool   `json:"active"`
}
type RuntimeSubscriptionResult struct {
	Subscription RuntimeSubscriptionSummary `json:"subscription"`
	Status       string                     `json:"status"`
}
type DeleteRuntimeSubscriptionResult struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}
type ConfigureResourceProxyResult struct {
	ResourceProxy ResourceProxy `json:"resource_proxy"`
}
type ListCoreVersionsResult struct {
	Channel  string   `json:"channel"`
	Versions []string `json:"versions"`
}
type CoreOperationResult struct {
	CoreStatus          string `json:"core_status"`
	CoreVersion         string `json:"core_version,omitempty"`
	PreviousCoreVersion string `json:"previous_core_version,omitempty"`
}
type RestartCoreResult struct {
	CoreStatus string `json:"core_status"`
}

var (
	stableCoreVersion     = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$`)
	alphaCoreVersion      = regexp.MustCompile(`^alpha-[0-9a-f]{7,40}$`)
	unstableCoreTag       = regexp.MustCompile(`(?i)(alpha|beta|rc|pre)`)
	runtimeSubscriptionID = regexp.MustCompile(`^[0-9a-f]{32}$`)
	runtimeSecretRef      = regexp.MustCompile(`^[0-9a-f]{48}$`)
)

type TestProxyDelayResult struct {
	Group   string `json:"group"`
	Proxy   string `json:"proxy"`
	DelayMS int    `json:"delay_ms"`
}
type SelectProxyResult struct {
	Group    string `json:"group"`
	Selected string `json:"selected"`
}
type CloseConnectionResult struct {
	ConnectionID string `json:"connection_id"`
	Closed       bool   `json:"closed"`
}

func ValidateJob(job Job, capabilities []string, now time.Time) error {
	if job.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocol version %d", job.ProtocolVersion)
	}
	if job.ID == "" || job.RequestID == "" || job.InstanceID <= 0 {
		return errors.New("id, request_id and instance_id are required")
	}
	if job.Status != JobQueued {
		return fmt.Errorf("new job status must be %q", JobQueued)
	}
	if !validActor(job.ActorType) {
		return fmt.Errorf("unknown actor_type %q", job.ActorType)
	}
	deadline, err := time.Parse(time.RFC3339, job.Deadline)
	if err != nil {
		return errors.New("deadline must be RFC3339")
	}
	if !now.Before(deadline) {
		return errors.New("job has expired")
	}
	if len(job.AuditReason) > 512 {
		return errors.New("audit_reason is too long")
	}
	if capability := RequiredCapability(job.Type); capability != "" && !contains(capabilities, capability) {
		return fmt.Errorf("job %q requires capability %q", job.Type, capability)
	}
	switch job.Type {
	case JobAddRuntimeSubscription:
		var params AddRuntimeSubscriptionParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		return validateRuntimeSubscriptionSource(params.Name, params.SecretRef, params.PlatformSubscriptionID, true)
	case JobEditRuntimeSubscription:
		var params EditRuntimeSubscriptionParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		if !runtimeSubscriptionID.MatchString(params.ID) {
			return errors.New("runtime subscription id is invalid")
		}
		return validateRuntimeSubscriptionSource(params.Name, params.SecretRef, params.PlatformSubscriptionID, false)
	case JobDeleteRuntimeSubscription, JobRefreshRuntimeSubscription, JobActivateRuntimeSubscription:
		var params RuntimeSubscriptionIDParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		if !runtimeSubscriptionID.MatchString(params.ID) {
			return errors.New("runtime subscription id is invalid")
		}
		return nil
	case JobConfigureResourceProxy:
		var params ConfigureResourceProxyParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		return ValidateResourceProxy(params.ResourceProxy)
	case JobListCoreVersions:
		var params ListCoreVersionsParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		if params.Channel != "stable" && params.Channel != "alpha" {
			return errors.New("channel must be stable or alpha")
		}
		return nil
	case JobInstallCore:
		var params InstallCoreParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		if params.Channel != "stable" && params.Channel != "alpha" {
			return errors.New("channel must be stable or alpha")
		}
		if params.Channel == "stable" && (!stableCoreVersion.MatchString(params.Version) || unstableCoreTag.MatchString(params.Version)) {
			return errors.New("stable channel requires an exact stable vX.Y.Z version")
		}
		if params.Channel == "alpha" && !alphaCoreVersion.MatchString(params.Version) {
			return errors.New("alpha channel requires an exact alpha-<commit> version")
		}
		return nil
	case JobUninstallCore, JobStartCore, JobStopCore, JobRollbackCore:
		return decodeStrict(job.Params, &EmptyParams{})
	case JobRestartCore:
		return decodeStrict(job.Params, &EmptyParams{})
	case JobTestProxyDelay:
		var params TestProxyDelayParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		if err := validateObservedNames(params.Group, params.Proxy); err != nil {
			return err
		}
		if params.TimeoutMS != 0 && (params.TimeoutMS < 1000 || params.TimeoutMS > 30000) {
			return errors.New("timeout_ms must be between 1000 and 30000")
		}
		return nil
	case JobSelectProxy:
		var params SelectProxyParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		return validateObservedNames(params.Group, params.Proxy)
	case JobCloseConnection:
		var params CloseConnectionParams
		if err := decodeStrict(job.Params, &params); err != nil {
			return err
		}
		if strings.TrimSpace(params.ConnectionID) == "" || len(params.ConnectionID) > 256 {
			return errors.New("connection_id is required and must not exceed 256 characters")
		}
		return nil
	default:
		return fmt.Errorf("unknown job type %q", job.Type)
	}
}

func RequiredCapability(jobType string) string {
	switch jobType {
	case JobAddRuntimeSubscription, JobEditRuntimeSubscription, JobDeleteRuntimeSubscription, JobRefreshRuntimeSubscription, JobActivateRuntimeSubscription:
		return "subscription.manage"
	case JobConfigureResourceProxy:
		return "agent.resource.proxy"
	case JobListCoreVersions:
		return "mihomo.release.list"
	case JobInstallCore, JobUninstallCore, JobStartCore, JobStopCore, JobRollbackCore:
		return "mihomo.core.manage"
	case JobRestartCore:
		return "mihomo.restart"
	case JobTestProxyDelay:
		return "mihomo.proxy.delay"
	case JobSelectProxy:
		return "mihomo.proxy.select"
	case JobCloseConnection:
		return "mihomo.connection.close"
	default:
		return ""
	}
}

func ValidateJobResult(jobType, status string, raw json.RawMessage) error {
	if !TerminalJobStatus(status) {
		return errors.New("job result is only valid for a terminal status")
	}
	if status != JobSucceeded {
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil
		}
		return decodeStrict(raw, &EmptyParams{})
	}
	switch jobType {
	case JobAddRuntimeSubscription, JobEditRuntimeSubscription, JobRefreshRuntimeSubscription, JobActivateRuntimeSubscription:
		var result RuntimeSubscriptionResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if result.Status != "saved" && result.Status != "refreshed" && result.Status != "active" {
			return errors.New("runtime subscription result status is invalid")
		}
		if err := ValidateRuntimeSubscriptionSummary(result.Subscription); err != nil {
			return err
		}
	case JobDeleteRuntimeSubscription:
		var result DeleteRuntimeSubscriptionResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if !runtimeSubscriptionID.MatchString(result.ID) || !result.Deleted {
			return errors.New("runtime subscription delete result is invalid")
		}
	case JobConfigureResourceProxy:
		var result ConfigureResourceProxyResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if err := ValidateResourceProxy(result.ResourceProxy); err != nil {
			return err
		}
	case JobListCoreVersions:
		var result ListCoreVersionsResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if result.Channel != "stable" && result.Channel != "alpha" {
			return errors.New("release result channel is invalid")
		}
		if len(result.Versions) > 50 {
			return errors.New("too many Mihomo release versions")
		}
		seen := make(map[string]bool, len(result.Versions))
		for _, version := range result.Versions {
			valid := result.Channel == "stable" && stableCoreVersion.MatchString(version) && !unstableCoreTag.MatchString(version)
			valid = valid || result.Channel == "alpha" && alphaCoreVersion.MatchString(version)
			if !valid || seen[version] {
				return errors.New("Mihomo release version result is invalid")
			}
			seen[version] = true
		}
	case JobInstallCore, JobUninstallCore, JobStartCore, JobStopCore, JobRollbackCore:
		var result CoreOperationResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if result.CoreStatus != "not_installed" && result.CoreStatus != "stopped" && result.CoreStatus != "starting" && result.CoreStatus != "running" && result.CoreStatus != "failed" {
			return errors.New("core_status is invalid")
		}
		if len(result.CoreVersion) > 128 || len(result.PreviousCoreVersion) > 128 {
			return errors.New("core version result is too long")
		}
	case JobRestartCore:
		var result RestartCoreResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if result.CoreStatus == "" {
			return errors.New("core_status is required")
		}
	case JobTestProxyDelay:
		var result TestProxyDelayResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if err := validateObservedNames(result.Group, result.Proxy); err != nil {
			return err
		}
		if result.DelayMS < 0 || result.DelayMS > 120000 {
			return errors.New("delay_ms is out of range")
		}
	case JobSelectProxy:
		var result SelectProxyResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if err := validateObservedNames(result.Group, result.Selected); err != nil {
			return err
		}
	case JobCloseConnection:
		var result CloseConnectionResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if strings.TrimSpace(result.ConnectionID) == "" || len(result.ConnectionID) > 256 {
			return errors.New("connection_id is required")
		}
	default:
		return fmt.Errorf("unknown job type %q", jobType)
	}
	return nil
}

func validateRuntimeSubscriptionSource(name, secretRef string, platformSubscriptionID int64, requireSource bool) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return errors.New("runtime subscription name is required and must not exceed 80 characters")
	}
	if platformSubscriptionID < 0 {
		return errors.New("platform subscription id is invalid")
	}
	hasSecret, hasPlatform := secretRef != "", platformSubscriptionID > 0
	if hasSecret && hasPlatform {
		return errors.New("choose either an external subscription or a platform subscription")
	}
	if requireSource && !hasSecret && !hasPlatform {
		return errors.New("a subscription source is required")
	}
	if secretRef != "" && !runtimeSecretRef.MatchString(secretRef) {
		return errors.New("subscription URL secret_ref is invalid")
	}
	return nil
}

func ValidateRuntimeSubscriptionSummary(value RuntimeSubscriptionSummary) error {
	if !runtimeSubscriptionID.MatchString(value.ID) || strings.TrimSpace(value.Name) == "" || len(value.Name) > 80 || value.Host == "" || len(value.Host) > 253 || strings.ContainsAny(value.Name+value.Host, "\r\n\x00") {
		return errors.New("runtime subscription summary is invalid")
	}
	if len(value.Revision) > 128 || len(value.LastError) > 512 || strings.ContainsAny(value.Revision+value.LastError, "\r\n\x00") || value.UsedBytes < 0 || value.TotalBytes < 0 {
		return errors.New("runtime subscription summary exceeds its limits")
	}
	if value.PlatformSubscriptionID < 0 {
		return errors.New("runtime subscription platform id is invalid")
	}
	for _, timestamp := range []string{value.ExpiresAt, value.LastUpdatedAt} {
		if timestamp != "" {
			if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
				return errors.New("runtime subscription summary time is invalid")
			}
		}
	}
	return nil
}

func ValidTransition(from, to string) bool {
	switch from {
	case JobQueued:
		return to == JobAccepted || to == JobRunning || to == JobFailed || to == JobOutcomeUnknown || to == JobExpired || to == JobCancelled
	case JobAccepted:
		return to == JobRunning || to == JobFailed || to == JobOutcomeUnknown || to == JobExpired || to == JobCancelled
	case JobRunning:
		return to == JobSucceeded || to == JobFailed || to == JobOutcomeUnknown
	default:
		return false
	}
}

func TerminalJobStatus(status string) bool {
	return status == JobSucceeded || status == JobFailed || status == JobOutcomeUnknown || status == JobExpired || status == JobCancelled
}

func decodeStrict(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("invalid params: multiple JSON values")
		}
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}

func validateObservedNames(values ...string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("group and proxy names are required and must be bounded single-line values")
		}
	}
	return nil
}

func validActor(value string) bool {
	return value == ActorAdminSession || value == ActorLocalCLI || value == ActorAgent
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
