package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"submux/internal/agentclient"
	"submux/internal/agentlocal"
	"submux/internal/agentproto"
	"submux/internal/store"
)

func (s *Server) handleCreateAgentEnrollment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.Name) == "" || len(body.Name) > 128 || strings.ContainsAny(body.Name, "\r\n\x00") {
		http.Error(w, "name is required and must not exceed 128 characters", http.StatusBadRequest)
		return
	}
	code := randomHex(24)
	digest := sha256.Sum256([]byte(code))
	now := time.Now().UTC()
	enrollment := store.AgentEnrollment{
		Digest: hex.EncodeToString(digest[:]), Name: strings.TrimSpace(body.Name),
		CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339),
	}
	if err := s.store.SaveAgentEnrollment(enrollment); err != nil {
		http.Error(w, "create pairing code failed", http.StatusInternalServerError)
		return
	}
	requestID := randomHex(16)
	s.audit(agentproto.ActorAdminSession, requestID, 0, "agent.enrollment.create", "", "succeeded", "short-lived one-time pairing code created")
	writeJSON(w, map[string]any{"code": code, "expires_at": enrollment.ExpiresAt, "request_id": requestID})
}

func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code         string   `json:"code"`
		PublicKey    string   `json:"public_key"`
		OS           string   `json:"os"`
		Arch         string   `json:"arch"`
		AgentVersion string   `json:"agent_version"`
		Capabilities []string `json:"capabilities"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid enrollment request", http.StatusBadRequest)
		return
	}
	if _, err := agentproto.DecodePublicKey(body.PublicKey); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(body.Code) {
		http.Error(w, "invalid pairing code", http.StatusUnauthorized)
		return
	}
	if !validRuntimePlatform(body.OS, body.Arch) || len(body.AgentVersion) > 128 || strings.ContainsAny(body.AgentVersion, "\r\n\x00") || agentproto.ValidateCapabilities(body.Capabilities) != nil {
		http.Error(w, "invalid host metadata", http.StatusBadRequest)
		return
	}
	digest := sha256.Sum256([]byte(body.Code))
	enrollment, err := s.store.ConsumeAgentEnrollment(hex.EncodeToString(digest[:]), time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	instance, err := s.store.CreateRuntimeInstance(store.RuntimeInstance{
		Name: enrollment.Name, DeviceKey: body.PublicKey, OS: body.OS, Arch: body.Arch,
		AgentVersion: body.AgentVersion, Capabilities: body.Capabilities, LastSeen: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		http.Error(w, "enrollment failed", http.StatusConflict)
		return
	}
	requestID := randomHex(16)
	s.audit(agentproto.ActorAgent, requestID, instance.ID, "agent.enroll", "", "succeeded", fmt.Sprintf("%s/%s device enrolled", instance.OS, instance.Arch))
	writeJSON(w, map[string]any{"instance_id": instance.ID, "protocol_version": agentproto.Version, "request_id": requestID})
}

type runtimeInstanceView struct {
	Instance    store.RuntimeInstance    `json:"instance"`
	Local       bool                     `json:"local"`
	Observation store.RuntimeObservation `json:"observation"`
}

func (s *Server) runtimeView(id int64) (runtimeInstanceView, error) {
	instance, err := s.store.GetRuntimeInstance(id)
	if err != nil {
		return runtimeInstanceView{}, err
	}
	if instance.Status != store.InstanceRevoked && instance.LastSeen != "" {
		if seen, err := time.Parse(time.RFC3339, instance.LastSeen); err == nil && time.Since(seen) > 90*time.Second {
			instance.Status = store.InstanceOffline
		}
	}
	observation, err := s.store.GetRuntimeObservation(id)
	if err != nil {
		return runtimeInstanceView{}, err
	}
	return runtimeInstanceView{Instance: instance, Observation: observation}, nil
}

func (s *Server) handleListRuntimeInstances(w http.ResponseWriter, _ *http.Request) {
	instances, err := s.store.ListRuntimeInstances()
	if err != nil {
		http.Error(w, "list runtime instances failed", http.StatusInternalServerError)
		return
	}
	views := make([]runtimeInstanceView, 0, len(instances))
	localInstanceID := s.detectLocalAgent()
	for _, instance := range instances {
		view, err := s.runtimeView(instance.ID)
		if err != nil {
			http.Error(w, "load runtime instance failed", http.StatusInternalServerError)
			return
		}
		view.Local = instance.ID == localInstanceID
		views = append(views, view)
	}
	writeJSON(w, views)
}

func (s *Server) handleGetRuntimeInstance(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	view, err := s.runtimeView(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	view.Local = id == s.detectLocalAgent()
	jobs, _ := s.store.ListAgentJobs(id)
	audit, _ := s.store.ListAuditEvents(id, 100)
	writeJSON(w, map[string]any{"runtime": view, "jobs": jobs, "audit": audit})
}

func (s *Server) detectLocalAgent() int64 {
	endpoint := ""
	switch runtime.GOOS {
	case "linux":
		endpoint = "/run/submux-agent/agent.sock"
		info, err := os.Lstat(endpoint)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return 0
		}
	case "windows":
		endpoint = `\\.\pipe\submux-agent`
	default:
		return 0
	}
	client, err := agentlocal.NewClient(endpoint)
	if err != nil {
		return 0
	}
	client.Timeout = 750 * time.Millisecond
	request, _ := http.NewRequest(http.MethodGet, "http://submux-agent.local/v1/status", nil)
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0
	}
	var status struct {
		Identity struct {
			InstanceID int64  `json:"instance_id"`
			PublicKey  string `json:"public_key"`
		} `json:"identity"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status); err != nil || status.Identity.InstanceID <= 0 || status.Identity.PublicKey == "" {
		return 0
	}
	instance, err := s.store.GetRuntimeInstance(status.Identity.InstanceID)
	if err != nil || instance.RevokedAt != "" || instance.DeviceKey != status.Identity.PublicKey {
		return 0
	}
	return instance.ID
}

func (s *Server) handleRevokeRuntimeInstance(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.RevokeRuntimeInstance(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.updates.notify(id, "instance_revoked")
	s.events.notify(id, "instance_revoked")
	requestID := randomHex(16)
	s.audit(agentproto.ActorAdminSession, requestID, id, "instance.revoke", "", "succeeded", "device access and pending work revoked")
	writeJSON(w, map[string]any{"ok": true, "request_id": requestID})
}

func (s *Server) handleCreateRuntimeJob(w http.ResponseWriter, r *http.Request) {
	instanceID, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	instance, err := s.store.GetRuntimeInstance(instanceID)
	if err != nil || instance.RevokedAt != "" {
		http.Error(w, "runtime instance does not exist or is revoked", http.StatusNotFound)
		return
	}
	var body struct {
		Type            string          `json:"type"`
		Params          json.RawMessage `json:"params"`
		DeadlineSeconds int             `json:"deadline_seconds"`
		Reason          string          `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid job", http.StatusBadRequest)
		return
	}
	if body.DeadlineSeconds == 0 {
		body.DeadlineSeconds = 300
	}
	if body.DeadlineSeconds < 10 || body.DeadlineSeconds > 3600 {
		http.Error(w, "deadline_seconds must be between 10 and 3600", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	job := store.AgentJob{Job: agentproto.Job{
		ID: randomHex(16), ProtocolVersion: agentproto.Version, InstanceID: instanceID,
		Type: body.Type, Params: body.Params, Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: randomHex(16), AuditReason: strings.TrimSpace(body.Reason),
		Deadline: now.Add(time.Duration(body.DeadlineSeconds) * time.Second).Format(time.RFC3339),
	}}
	if err := agentproto.ValidateJob(job.Job, instance.Capabilities, now); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Expire queued or accepted work before deciding whether this instance is
	// busy. Otherwise an offline Agent could leave a timed-out job blocking all
	// later one-shot operations indefinitely.
	if _, err := s.store.ListRunnableAgentJobs(instanceID, now); err != nil {
		http.Error(w, "expire inactive jobs failed", http.StatusInternalServerError)
		return
	}
	jobs, err := s.store.ListAgentJobs(instanceID)
	if err != nil {
		http.Error(w, "list active jobs failed", http.StatusInternalServerError)
		return
	}
	for _, existing := range jobs {
		if existing.Status == agentproto.JobQueued || existing.Status == agentproto.JobAccepted || existing.Status == agentproto.JobRunning {
			http.Error(w, "the Agent is already executing another operation", http.StatusConflict)
			return
		}
	}
	if err := s.store.CreateAgentJob(job); err != nil {
		http.Error(w, "create job failed", http.StatusInternalServerError)
		return
	}
	s.updates.notify(instanceID, "job_queued")
	s.events.notify(instanceID, "job_queued")
	s.audit(agentproto.ActorAdminSession, job.RequestID, instanceID, "job."+job.Type+".create", job.ID, "queued", job.AuditReason)
	writeJSON(w, job)
}

func (s *Server) handleListRuntimeJobs(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	jobs, err := s.store.ListAgentJobs(id)
	if err != nil {
		http.Error(w, "list jobs failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, jobs)
}

func (s *Server) handleListRuntimeAudit(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	events, err := s.store.ListAuditEvents(id, 200)
	if err != nil {
		http.Error(w, "list audit failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, events)
}

func (s *Server) handleAgentState(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	jobs, err := s.store.ListRunnableAgentJobs(instance.ID, time.Now().UTC())
	if err != nil {
		http.Error(w, "jobs unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"protocol_version": agentproto.Version, "jobs": jobs})
}

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	var body struct {
		AgentVersion string                   `json:"agent_version"`
		Capabilities []string                 `json:"capabilities"`
		Status       string                   `json:"status"`
		Observation  store.RuntimeObservation `json:"observation"`
	}
	if err := decodeJSON(r, &body); err != nil || len(body.AgentVersion) > 128 || strings.ContainsAny(body.AgentVersion, "\r\n\x00") || agentproto.ValidateCapabilities(body.Capabilities) != nil {
		http.Error(w, "invalid heartbeat", http.StatusBadRequest)
		return
	}
	body.Observation.InstanceID = instance.ID
	if len(body.Observation.RecentError) > 1024 {
		http.Error(w, "recent_error is too long", http.StatusBadRequest)
		return
	}
	body.Observation.RecentError = safeAgentError(body.Observation.RecentError)
	if body.Observation.Operation != nil {
		body.Observation.Operation.Error = safeAgentError(body.Observation.Operation.Error)
	}
	if body.Observation.AgentUptimeSeconds < 0 {
		http.Error(w, "invalid Agent uptime", http.StatusBadRequest)
		return
	}
	if len(body.Observation.SelectedProxies) > 256 || len(body.Observation.SelectionNotice) > 512 {
		http.Error(w, "runtime observation exceeds its limits", http.StatusBadRequest)
		return
	}
	if err := validateRuntimeObservation(body.Observation); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateRuntimeHeartbeat(instance.ID, body.AgentVersion, body.Capabilities, body.Status, time.Now().UTC()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := s.store.SaveRuntimeObservation(body.Observation); err != nil {
		http.Error(w, "save observation failed", http.StatusInternalServerError)
		return
	}
	s.events.notify(instance.ID, "heartbeat")
	s.handleAgentState(w, r)
}

func validRuntimePlatform(osName, arch string) bool {
	return (osName == "linux" || osName == "windows") && (arch == "amd64" || arch == "arm64")
}

func validateRuntimeObservation(value store.RuntimeObservation) error {
	for _, revision := range []string{value.RemoteRevision, value.AppliedRevision, value.RejectedRevision, value.LastGoodRevision} {
		if len(revision) > 256 || strings.ContainsAny(revision, "\r\n\x00") {
			return errors.New("runtime revision is invalid")
		}
	}
	if len(value.CoreVersion) > 128 || len(value.PreviousCoreVersion) > 128 || value.ProxyPort < 0 || value.ProxyPort > 65535 || value.ControllerPort < 0 || value.ControllerPort > 65535 {
		return errors.New("runtime core or port observation is invalid")
	}
	if len(value.Subscriptions) > 32 {
		return errors.New("runtime subscription observation exceeds its limits")
	}
	activeSeen := false
	seenSubscriptions := make(map[string]bool, len(value.Subscriptions))
	for _, subscription := range value.Subscriptions {
		if err := agentproto.ValidateRuntimeSubscriptionSummary(subscription); err != nil {
			return err
		}
		if seenSubscriptions[subscription.ID] {
			return errors.New("runtime subscription observation contains duplicate IDs")
		}
		seenSubscriptions[subscription.ID] = true
		if subscription.Active {
			if activeSeen || subscription.ID != value.ActiveSubscriptionID {
				return errors.New("runtime active subscription observation is invalid")
			}
			activeSeen = true
		}
	}
	if (value.ActiveSubscriptionID != "") != activeSeen {
		return errors.New("runtime active subscription observation is invalid")
	}
	if (value.ProxyPort == 0 && value.ProxyKind != "") || (value.ProxyPort > 0 && value.ProxyKind != "mixed" && value.ProxyKind != "http" && value.ProxyKind != "socks5") {
		return errors.New("runtime proxy listener kind is invalid")
	}
	if value.CoreStatus != store.RuntimeNotInstalled && value.CoreStatus != store.RuntimeStopped && value.CoreStatus != store.RuntimeStarting && value.CoreStatus != store.RuntimeRunning && value.CoreStatus != store.RuntimeFailed {
		return errors.New("runtime core status is invalid")
	}
	if value.ResourceProxyMode != "" && value.ResourceProxyMode != agentproto.ResourceProxyDirect && value.ResourceProxyMode != agentproto.ResourceProxyCustom {
		return errors.New("runtime resource proxy observation is invalid")
	}
	if value.ResourceProxyMode == "" && value.ResourceProxyURL != "" {
		return errors.New("runtime resource proxy observation is invalid")
	}
	if value.ResourceProxyMode != "" {
		if err := agentproto.ValidateResourceProxy(agentproto.ResourceProxy{Mode: value.ResourceProxyMode, URL: value.ResourceProxyURL}); err != nil {
			return errors.New("runtime resource proxy observation is invalid")
		}
	}
	if value.Operation != nil {
		operation := value.Operation
		validStatus := operation.Status == "running" || operation.Status == "succeeded" || operation.Status == "failed"
		validName := regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
		if !validStatus || !validName.MatchString(operation.Kind) || !validName.MatchString(operation.Phase) || operation.BytesCompleted < 0 || operation.BytesTotal < 0 || (operation.BytesTotal > 0 && operation.BytesCompleted > operation.BytesTotal) || len(operation.RequestID) > 128 || len(operation.JobID) > 128 || len(operation.Error) > 1024 {
			return errors.New("runtime operation observation is invalid")
		}
	}
	for group, proxy := range value.SelectedProxies {
		if group == "" || proxy == "" || len(group) > 256 || len(proxy) > 256 || strings.ContainsAny(group+proxy, "\r\n\x00") {
			return errors.New("selected proxy observation is invalid")
		}
	}
	return nil
}

func (s *Server) handleAgentJobStatus(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	jobID := chi.URLParam(r, "jobID")
	job, err := s.store.GetAgentJob(jobID)
	if err != nil || job.InstanceID != instance.ID {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	var body struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  string          `json:"error,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid job status", http.StatusBadRequest)
		return
	}
	if agentproto.TerminalJobStatus(body.Status) {
		if err := agentproto.ValidateJobResult(job.Type, body.Status, body.Result); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if len(body.Result) > 0 || body.Error != "" {
		http.Error(w, "non-terminal job status cannot contain a result", http.StatusBadRequest)
		return
	}
	updated, err := s.store.TransitionAgentJob(jobID, body.Status, body.Result, safeAgentError(body.Error))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.audit(agentproto.ActorAgent, updated.RequestID, instance.ID, "job."+updated.Type+"."+updated.Status, updated.ID, updated.Status, safeAgentError(body.Error))
	s.events.notify(instance.ID, "job_status_changed")
	writeJSON(w, updated)
}

var localAuditActions = map[string]bool{
	"mihomo.install": true, "mihomo.restart": true, "mihomo.rollback": true,
	"subscription.rollback": true,
}

func (s *Server) handleAgentLocalAudit(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	var body agentclient.LocalAudit
	if err := decodeJSON(r, &body); err != nil || !localAuditActions[body.Action] || len(body.RequestID) < 16 || len(body.RequestID) > 128 || len(body.Revision) > 256 || len(body.Summary) > 1024 || (body.Result != "succeeded" && body.Result != "failed") {
		http.Error(w, "invalid local audit event", http.StatusBadRequest)
		return
	}
	s.audit(agentproto.ActorLocalCLI, body.RequestID, instance.ID, body.Action, body.Revision, body.Result, body.Summary)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleAgentRevokeSelf(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	var body struct{}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	requestID := randomHex(16)
	s.audit(agentproto.ActorLocalCLI, requestID, instance.ID, "agent.revoke-self", "", "succeeded", "local administrator revoked this device registration")
	if err := s.store.RevokeRuntimeInstance(instance.ID); err != nil {
		http.Error(w, "device revocation failed", http.StatusConflict)
		return
	}
	s.updates.notify(instance.ID, "instance_revoked")
	writeJSON(w, map[string]any{"ok": true, "request_id": requestID})
}

func (s *Server) audit(actor, requestID string, instanceID int64, action, revision, result, summary string) {
	_, _ = s.store.AddAuditEvent(store.AuditEvent{
		ActorType: actor, RequestID: requestID, InstanceID: instanceID,
		Action: action, Revision: revision, Result: result, Summary: safeAgentError(summary),
	})
}

func safeAgentError(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
	lower := strings.ToLower(value)
	if strings.Contains(value, "://") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "private key") {
		return "sensitive details redacted"
	}
	if len(value) > 1024 {
		value = value[:1024]
	}
	return value
}
