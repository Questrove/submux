package agentproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const Version = 1

const (
	ActorAdminSession   = "admin_session"
	ActorLocalCLI       = "local_cli"
	ActorAgentReconcile = "agent_reconcile"
	ActorScheduler      = "system_scheduler"
)

var knownCapabilities = map[string]bool{
	"subscription.update":        true,
	"mihomo.restart":             true,
	"mihomo.proxy.delay":         true,
	"mihomo.proxy.select":        true,
	"mihomo.connection.close":    true,
	"mihomo.runtime.observe":     true,
	"diagnostics.collect":        true,
	"integration.docker_daemon":  true,
	"integration.docker_desktop": true,
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
	JobUpdateSubscription = "update_subscription"
	JobRestartCore        = "restart_core"
	JobTestProxyDelay     = "test_proxy_delay"
	JobSelectProxy        = "select_proxy"
	JobCloseConnection    = "close_connection"
	JobCollectDiagnostics = "collect_diagnostics"
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
type UpdateSubscriptionParams struct {
	RetryRejected bool `json:"retry_rejected,omitempty"`
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
type CollectDiagnosticsParams struct {
	IncludeRecentLogs bool `json:"include_recent_logs,omitempty"`
}

type UpdateSubscriptionResult struct {
	RemoteRevision   string `json:"remote_revision,omitempty"`
	AppliedRevision  string `json:"applied_revision,omitempty"`
	RejectedRevision string `json:"rejected_revision,omitempty"`
	Status           string `json:"status"`
}
type RestartCoreResult struct {
	CoreStatus string `json:"core_status"`
}
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
type DiagnosticCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}
type CollectDiagnosticsResult struct {
	Checks []DiagnosticCheck `json:"checks"`
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
	case JobUpdateSubscription:
		var params UpdateSubscriptionParams
		return decodeStrict(job.Params, &params)
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
	case JobCollectDiagnostics:
		var params CollectDiagnosticsParams
		return decodeStrict(job.Params, &params)
	default:
		return fmt.Errorf("unknown job type %q", job.Type)
	}
}

func RequiredCapability(jobType string) string {
	switch jobType {
	case JobUpdateSubscription:
		return "subscription.update"
	case JobRestartCore:
		return "mihomo.restart"
	case JobTestProxyDelay:
		return "mihomo.proxy.delay"
	case JobSelectProxy:
		return "mihomo.proxy.select"
	case JobCloseConnection:
		return "mihomo.connection.close"
	case JobCollectDiagnostics:
		return "diagnostics.collect"
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
	case JobUpdateSubscription:
		var result UpdateSubscriptionResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if result.Status == "" {
			return errors.New("subscription result status is required")
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
	case JobCollectDiagnostics:
		var result CollectDiagnosticsResult
		if err := decodeStrict(raw, &result); err != nil {
			return err
		}
		if len(result.Checks) > 64 {
			return errors.New("too many diagnostic checks")
		}
		for _, check := range result.Checks {
			if check.Name == "" || len(check.Name) > 128 || len(check.Message) > 1024 || (check.Status != "ok" && check.Status != "warning" && check.Status != "failed") {
				return errors.New("invalid diagnostic check")
			}
		}
	default:
		return fmt.Errorf("unknown job type %q", jobType)
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
	return value == ActorAdminSession || value == ActorLocalCLI || value == ActorAgentReconcile || value == ActorScheduler
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
