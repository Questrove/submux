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
	Sources           SourceStatus    `json:"sources"`
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
	Version  string `json:"version,omitempty"`
	State    string `json:"state"`
	Recovery string `json:"recovery"`
}

type SourceStatus struct {
	Count             int    `json:"count"`
	CurrentSourceID   string `json:"current_source_id,omitempty"`
	LastRefreshResult string `json:"last_refresh_result,omitempty"`
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

type Action struct {
	Kind   string       `json:"kind"`
	Params ActionParams `json:"params"`
}

type ActionParams struct {
	ContentID string `json:"content_id,omitempty"`
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
	ClientVersion  string           `json:"client_version"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Error          *ProtocolError   `json:"error,omitempty"`
	Result         *OperationResult `json:"result,omitempty"`
}

type OperationResult struct {
	ConfigRevision string   `json:"config_revision,omitempty"`
	ProxyKind      string   `json:"proxy_kind,omitempty"`
	ProxyAddresses []string `json:"proxy_addresses,omitempty"`
	Verified       bool     `json:"verified,omitempty"`
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
	ErrorInvalidRequest      = "invalid_request"
	ErrorProtocolUnsupported = "protocol_unsupported"
	ErrorUnauthorized        = "permission_denied"
	ErrorRequestTooLarge     = "request_too_large"
	ErrorAlreadyRunning      = "instance_conflict"
	ErrorServiceUnavailable  = "service_unavailable"
	ErrorRevisionConflict    = "revision_conflict"
	ErrorRequestConflict     = "request_id_conflict"
	ErrorBusy                = "busy"
	ErrorNotFound            = "not_found"
	ErrorNotCancellable      = "not_cancellable"
	ErrorContentExpired      = "content_expired"
	ErrorContentConsumed     = "content_consumed"
	ErrorInternal            = "internal"
)

const (
	ActionApplyImportedConfig = "proxy.apply_import"
	ActionStartProxy          = "proxy.start"
	ActionStopProxy           = "proxy.stop"

	OperationQueued         = "queued"
	OperationRunning        = "running"
	OperationSucceeded      = "succeeded"
	OperationFailed         = "failed"
	OperationCancelled      = "cancelled"
	OperationOutcomeUnknown = "outcome_unknown"
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
