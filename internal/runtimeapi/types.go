package runtimeapi

import "time"

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

type UpdateStatus struct {
	RuntimeAvailable bool `json:"runtime_available"`
	MihomoAvailable  bool `json:"mihomo_available"`
}

type ErrorEnvelope struct {
	ProtocolVersion   int           `json:"protocol_version"`
	RequestID         string        `json:"request_id,omitempty"`
	Error             ProtocolError `json:"error"`
	SupportedVersions []int         `json:"supported_versions,omitempty"`
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
	ErrorInternal            = "internal"
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
