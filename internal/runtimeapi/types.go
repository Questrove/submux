package runtimeapi

import (
	"encoding/json"
	"time"
)

const ProtocolVersion = 1

type PeerIdentity struct {
	Platform string
	UID      uint32
	GID      uint32
	PID      uint32
	SID      string
}

func (p PeerIdentity) Key() string {
	if p.SID != "" {
		return p.Platform + ":sid:" + p.SID
	}
	return p.Platform + ":uid:" + uint32String(p.UID)
}

type Snapshot struct {
	ProtocolVersion   int             `json:"protocol_version"`
	Revision          uint64          `json:"revision"`
	Runtime           RuntimeStatus   `json:"runtime"`
	Mihomo            MihomoStatus    `json:"mihomo"`
	RunMode           string          `json:"run_mode"`
	Network           NetworkStatus   `json:"network"`
	Sources           SourceStatus    `json:"sources"`
	Resources         ResourceStatus  `json:"resources"`
	AdvancedOverride  OverrideStatus  `json:"advanced_override"`
	Operations        OperationStatus `json:"operations"`
	Updates           UpdateStatus    `json:"updates"`
	LatestEventCursor uint64          `json:"latest_event_cursor"`
	ObservedAt        time.Time       `json:"observed_at"`
}

type RuntimeStatus struct {
	Version      string `json:"version"`
	ServiceState string `json:"service_state"`
	Fault        *Fault `json:"fault,omitempty"`
}

type Fault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type MihomoStatus struct {
	Version       string     `json:"version,omitempty"`
	DesiredState  string     `json:"desired_state"`
	State         string     `json:"state"`
	Recovery      string     `json:"recovery"`
	CrashAttempts int        `json:"crash_attempts,omitempty"`
	NextRestartAt *time.Time `json:"next_restart_at,omitempty"`
	Fault         *Fault     `json:"fault,omitempty"`
}

type SourceStatus struct {
	Count             int             `json:"count"`
	CurrentSourceID   string          `json:"current_source_id,omitempty"`
	LastRefreshResult string          `json:"last_refresh_result,omitempty"`
	Items             []SourceSummary `json:"items,omitempty"`
}

type SourceSummary struct {
	ID                     string     `json:"id"`
	Type                   string     `json:"type"`
	Name                   string     `json:"name"`
	Current                bool       `json:"current"`
	RedactedTarget         string     `json:"redacted_target"`
	Route                  string     `json:"route"`
	RefreshIntervalSeconds int64      `json:"refresh_interval_seconds"`
	LastRefreshResult      string     `json:"last_refresh_result,omitempty"`
	LastRefreshAt          *time.Time `json:"last_refresh_at,omitempty"`
	NextRefreshAt          *time.Time `json:"next_refresh_at,omitempty"`
	LastRefreshRoute       string     `json:"last_refresh_route,omitempty"`
	FailureClass           string     `json:"failure_class,omitempty"`
	HighRiskSettings       []string   `json:"high_risk_settings,omitempty"`
	HasValidatedCandidate  bool       `json:"has_validated_candidate"`
}

type RemoteSourceDraft struct {
	Type                   string `json:"type,omitempty"`
	Name                   string `json:"name"`
	URL                    string `json:"url"`
	Route                  string `json:"route"`
	UserAgent              string `json:"user_agent,omitempty"`
	Username               string `json:"username,omitempty"`
	Password               string `json:"password,omitempty"`
	AuthorizedTarget       string `json:"authorized_target,omitempty"`
	AllowPrivate           bool   `json:"allow_private,omitempty"`
	AllowHTTP              bool   `json:"allow_http,omitempty"`
	CustomCAPEM            string `json:"custom_ca_pem,omitempty"`
	SkipTLSVerify          bool   `json:"skip_tls_verify,omitempty"`
	RefreshIntervalSeconds *int64 `json:"refresh_interval_seconds,omitempty"`
	TimeoutSeconds         int    `json:"timeout_seconds,omitempty"`
	MaxResponseBytes       int64  `json:"max_response_bytes,omitempty"`
}

type ResourceStatus struct {
	Count      int                      `json:"count"`
	TotalBytes int64                    `json:"total_bytes"`
	Items      []ManagedResourceSummary `json:"items,omitempty"`
}

type ManagedResourceSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

type OverrideStatus struct {
	Present   bool       `json:"present"`
	Size      int64      `json:"size,omitempty"`
	SHA256    string     `json:"sha256,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type AdvancedOverrideDocument struct {
	YAML   string `json:"yaml"`
	SHA256 string `json:"sha256,omitempty"`
}

type OperationStatus struct {
	CurrentOperationID string `json:"current_operation_id,omitempty"`
	Queued             int    `json:"queued"`
}

type ImportContent struct {
	ID          string    `json:"content_id"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type PreviewCandidateRequest struct {
	ContentID         string `json:"content_id,omitempty"`
	SourceID          string `json:"source_id,omitempty"`
	OverrideContentID string `json:"override_content_id,omitempty"`
}

type CandidatePreview struct {
	ContentID           string                 `json:"content_id"`
	CandidateYAML       string                 `json:"candidate_yaml"`
	CandidateSHA256     string                 `json:"candidate_sha256"`
	ProxyKind           string                 `json:"proxy_kind"`
	ProxyAddresses      []string               `json:"proxy_addresses"`
	RuntimeOwnedFields  []string               `json:"runtime_owned_fields"`
	FieldOrigins        []CandidateFieldOrigin `json:"field_origins"`
	ReferencedResources []string               `json:"referenced_resources,omitempty"`
	SourceSHA256        string                 `json:"source_sha256"`
	OverrideSHA256      string                 `json:"override_sha256,omitempty"`
	Validated           bool                   `json:"validated"`
}

type CandidateFieldOrigin struct {
	Path           string `json:"path"`
	Origin         string `json:"origin"`
	Status         string `json:"status"`
	ReplacedOrigin string `json:"replaced_origin,omitempty"`
}

type Action struct {
	Kind   string       `json:"kind"`
	Params ActionParams `json:"params"`
}

type ActionParams struct {
	ContentID    string `json:"content_id,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
	SourceName   string `json:"source_name,omitempty"`
	Route        string `json:"route,omitempty"`
	UseCached    bool   `json:"use_cached,omitempty"`
	Confirm      bool   `json:"confirm,omitempty"`
	ResourceKind string `json:"resource_kind,omitempty"`
	ResourceName string `json:"resource_name,omitempty"`
	PlanID       string `json:"plan_id,omitempty"`
}

type CreateOperationRequest struct {
	RequestID  string `json:"request_id"`
	IfRevision uint64 `json:"if_revision"`
	Action     Action `json:"action"`
}

type CancelOperationRequest struct {
	RequestID  string `json:"request_id"`
	IfRevision uint64 `json:"if_revision"`
}

type Operation struct {
	ID             string           `json:"id"`
	RequestID      string           `json:"request_id"`
	Action         Action           `json:"action"`
	State          string           `json:"state"`
	Stage          string           `json:"stage"`
	Progress       int              `json:"progress"`
	Cancellable    bool             `json:"cancellable"`
	CallerIdentity string           `json:"caller_identity"`
	CancelledBy    string           `json:"cancelled_by,omitempty"`
	ClientType     string           `json:"client_type"`
	ClientVersion  string           `json:"client_version"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Error          *ProtocolError   `json:"error,omitempty"`
	Result         *OperationResult `json:"result,omitempty"`
}

type AuditRecord struct {
	ID            string         `json:"id"`
	RequestID     string         `json:"request_id,omitempty"`
	OperationID   string         `json:"operation_id,omitempty"`
	Actor         string         `json:"actor"`
	ClientType    string         `json:"client_type"`
	ClientVersion string         `json:"client_version"`
	Action        string         `json:"action"`
	ObjectID      string         `json:"object_id,omitempty"`
	Stage         string         `json:"stage"`
	Result        string         `json:"result"`
	At            time.Time      `json:"at"`
	Error         *ProtocolError `json:"error,omitempty"`
}

type OperationResult struct {
	ConfigRevision         string         `json:"config_revision,omitempty"`
	CandidateSHA256        string         `json:"candidate_sha256,omitempty"`
	ProxyKind              string         `json:"proxy_kind,omitempty"`
	ProxyAddresses         []string       `json:"proxy_addresses,omitempty"`
	Verified               bool           `json:"verified,omitempty"`
	SourceID               string         `json:"source_id,omitempty"`
	PreviousSourceID       string         `json:"previous_source_id,omitempty"`
	UsedCachedSource       bool           `json:"used_cached_source,omitempty"`
	Deleted                bool           `json:"deleted,omitempty"`
	RefreshResult          string         `json:"refresh_result,omitempty"`
	RefreshRoute           string         `json:"refresh_route,omitempty"`
	NextRefreshAt          *time.Time     `json:"next_refresh_at,omitempty"`
	NotModified            bool           `json:"not_modified,omitempty"`
	ResourceID             string         `json:"resource_id,omitempty"`
	ResourceKind           string         `json:"resource_kind,omitempty"`
	AdvancedOverrideSHA256 string         `json:"advanced_override_sha256,omitempty"`
	RunMode                string         `json:"run_mode,omitempty"`
	Network                *NetworkStatus `json:"network,omitempty"`
}

type OperationResponse struct {
	Operation Operation `json:"operation"`
}

type ProxyVerification struct {
	Available bool      `json:"available"`
	Kind      string    `json:"kind,omitempty"`
	Addresses []string  `json:"addresses,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     *Fault    `json:"error,omitempty"`
}

type RevealSourceURLRequest struct {
	SourceID string `json:"source_id"`
	Confirm  bool   `json:"confirm"`
}

type RevealSourceURLResponse struct {
	SourceID string `json:"source_id"`
	URL      string `json:"url"`
}

type DiagnosticsRequest struct {
	IncludeRawConfig   bool `json:"include_raw_config,omitempty"`
	IncludeFullLogs    bool `json:"include_full_logs,omitempty"`
	IncludeNetworkInfo bool `json:"include_network_info,omitempty"`
	ConfirmSensitive   bool `json:"confirm_sensitive,omitempty"`
}

type DiagnosticItem struct {
	Name      string `json:"name"`
	Included  bool   `json:"included"`
	Sensitive bool   `json:"sensitive"`
	Size      int64  `json:"size,omitempty"`
}

type DiagnosticsPreview struct {
	Items   []DiagnosticItem `json:"items"`
	Warning string           `json:"warning"`
}

type DiagnosticsResult struct {
	FileName  string    `json:"file_name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	Cursor            uint64          `json:"cursor"`
	Type              string          `json:"type"`
	At                time.Time       `json:"at"`
	OperationID       string          `json:"operation_id,omitempty"`
	SnapshotRevision  uint64          `json:"snapshot_revision"`
	AdditionalPayload json.RawMessage `json:"payload,omitempty"`
}

type UpdateStatus struct {
	RuntimeAvailable bool `json:"runtime_available"`
	MihomoAvailable  bool `json:"mihomo_available"`
}

type ErrorEnvelope struct {
	ProtocolVersion   int           `json:"protocol_version"`
	RequestID         string        `json:"request_id,omitempty"`
	Error             ProtocolError `json:"error"`
	SupportedVersions []int         `json:"supported_versions,omitempty"`
	CurrentRevision   uint64        `json:"current_revision,omitempty"`
	EarliestCursor    uint64        `json:"earliest_cursor,omitempty"`
}

type ProtocolError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

const (
	ErrorInvalidRequest       = "invalid_request"
	ErrorProtocolUnsupported  = "protocol_unsupported"
	ErrorUnauthorized         = "permission_denied"
	ErrorRequestTooLarge      = "request_too_large"
	ErrorAlreadyRunning       = "instance_conflict"
	ErrorServiceUnavailable   = "service_unavailable"
	ErrorRevisionConflict     = "revision_conflict"
	ErrorRequestConflict      = "request_id_conflict"
	ErrorBusy                 = "busy"
	ErrorNotFound             = "not_found"
	ErrorNotCancellable       = "not_cancellable"
	ErrorCursorExpired        = "cursor_expired"
	ErrorContentExpired       = "content_expired"
	ErrorContentConsumed      = "content_consumed"
	ErrorSourceAuthentication = "source_authentication_failed"
	ErrorInternal             = "internal"
)

const SensitiveDataWarning = "敏感内容可能包含访问凭据、配置正文、完整日志或本机信息；仅在确认当前显示与保存环境安全时继续。"

const (
	ActionApplyImportedConfig = "proxy.apply_import"
	ActionStartProxy          = "proxy.start"
	ActionStopProxy           = "proxy.stop"
	ActionAddRemoteSource     = "source.add_remote"
	ActionAddImportedSource   = "source.add_imported"
	ActionRefreshSource       = "source.refresh"
	ActionApplySource         = "source.apply"
	ActionSwitchSource        = "source.switch"
	ActionDeleteSource        = "source.delete"
	ActionAddManagedResource  = "resource.add"
	ActionSetAdvancedOverride = "override.set"
	ActionEnableTUN           = "network.enable_tun"
	ActionDisableTUN          = "network.disable_tun"

	OperationQueued         = "queued"
	OperationRunning        = "running"
	OperationSucceeded      = "succeeded"
	OperationFailed         = "failed"
	OperationCancelled      = "cancelled"
	OperationOutcomeUnknown = "outcome_unknown"
)

const (
	MihomoDesiredUnset   = "unset"
	MihomoDesiredRunning = "running"
	MihomoDesiredStopped = "stopped"

	MihomoRecoveryIdle            = "idle"
	MihomoRecoveryStartup         = "startup_recovery"
	MihomoRecoveryWaiting         = "waiting_to_restart"
	MihomoRecoveryRestarting      = "restarting"
	MihomoRecoveryMonitoring      = "monitoring_stability"
	MihomoRecoveryNeedsAttention  = "needs_attention"
	MihomoRecoveryFailOpenUnknown = "fail_open_unknown"
)

const (
	SourceDraftContentType = "application/vnd.submux.runtime-source+json"

	SourceTypeRemoteHTTP   = "remote_http"
	SourceTypeSubmuxOutput = "submux_output"
	SourceTypeLocalImport  = "local_import"

	SourceRouteDirect = "direct"
	SourceRouteMihomo = "mihomo"

	ManagedResourceContentType = "application/vnd.submux.managed-resource"

	ResourceKindProxyProvider = "proxy-provider-yaml"
	ResourceKindRuleProvider  = "rule-provider-yaml"
	ResourceKindCertificate   = "certificate-pem"
	ResourceKindPrivateKey    = "private-key-pem"

	FieldOriginSource   = "source"
	FieldOriginOverride = "advanced_override"
	FieldOriginRuntime  = "runtime"

	FieldStatusKept       = "kept"
	FieldStatusAdded      = "added"
	FieldStatusOverridden = "overridden"
	FieldStatusReplaced   = "replaced"
	FieldStatusRemoved    = "removed"
)

func uint32String(value uint32) string {
	if value == 0 {
		return "0"
	}
	var buffer [10]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
