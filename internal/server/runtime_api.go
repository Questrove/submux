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
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"submux/internal/agentclient"
	"submux/internal/agentlocal"
	"submux/internal/agentproto"
	"submux/internal/compiler"
	"submux/internal/integration"
	"submux/internal/store"
)

var exactCoreVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$`)

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
	s.audit(agentproto.ActorAgentReconcile, requestID, instance.ID, "agent.enroll", "", "succeeded", fmt.Sprintf("%s/%s device enrolled", instance.OS, instance.Arch))
	writeJSON(w, map[string]any{"instance_id": instance.ID, "protocol_version": agentproto.Version, "request_id": requestID})
}

type runtimeInstanceView struct {
	Instance    store.RuntimeInstance     `json:"instance"`
	Local       bool                      `json:"local"`
	Binding     *store.RuntimeBinding     `json:"binding,omitempty"`
	Desired     store.RuntimeDesiredState `json:"desired"`
	Observation store.RuntimeObservation  `json:"observation"`
	Risk        *compiler.RuntimeAnalysis `json:"runtime_analysis,omitempty"`
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
	desired, err := s.store.GetRuntimeDesiredState(id)
	if err != nil {
		return runtimeInstanceView{}, err
	}
	observation, err := s.store.GetRuntimeObservation(id)
	if err != nil {
		return runtimeInstanceView{}, err
	}
	view := runtimeInstanceView{Instance: instance, Desired: desired, Observation: observation}
	if binding, err := s.store.GetRuntimeBindingByInstance(id); err == nil {
		view.Binding = &binding
		if subscription, err := s.store.GetOutputSubscription(binding.OutputSubscriptionID); err == nil {
			if version, err := s.store.GetTemplateVersion(subscription.TemplateVersionID); err == nil {
				if analysis, err := compiler.AnalyzeMihomoRuntime(version.Content, version.RuntimeContract); err == nil {
					view.Risk = &analysis
				}
			}
		}
	}
	return view, nil
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
	deployments, _ := s.store.ListDeployments(id, 50)
	integrations, _ := s.store.ListIntegrationStates(id)
	writeJSON(w, map[string]any{"runtime": view, "jobs": jobs, "deployments": deployments, "integrations": integrations, "audit": audit})
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
	requestID := randomHex(16)
	s.audit(agentproto.ActorAdminSession, requestID, id, "instance.revoke", "", "succeeded", "device access and pending work revoked")
	writeJSON(w, map[string]any{"ok": true, "request_id": requestID})
}

func (s *Server) handlePutRuntimeBinding(w http.ResponseWriter, r *http.Request) {
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
		OutputSubscriptionID int64 `json:"output_subscription_id"`
		AutoUpdate           bool  `json:"auto_update"`
		CheckIntervalSec     int   `json:"check_interval_sec"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid binding", http.StatusBadRequest)
		return
	}
	subscription, err := s.store.GetOutputSubscription(body.OutputSubscriptionID)
	if err != nil || !subscription.Enabled || subscription.Engine != compiler.EngineMihomo {
		http.Error(w, "binding requires an enabled Mihomo output subscription", http.StatusBadRequest)
		return
	}
	version, err := s.store.GetTemplateVersion(subscription.TemplateVersionID)
	if err != nil || version.RuntimeContract != compiler.RuntimeContractMihomoAgentV1 {
		http.Error(w, "template version does not publish mihomo-agent/v1", http.StatusBadRequest)
		return
	}
	if _, err := s.compiler.Preview(subscription); err != nil {
		http.Error(w, "bound output subscription is not currently valid: "+err.Error(), http.StatusConflict)
		return
	}
	if body.CheckIntervalSec == 0 {
		body.CheckIntervalSec = 300
	}
	binding, err := s.store.SaveRuntimeBinding(store.RuntimeBinding{
		InstanceID: instanceID, OutputSubscriptionID: subscription.ID,
		RuntimeContract: version.RuntimeContract, AutoUpdate: body.AutoUpdate, CheckIntervalSec: body.CheckIntervalSec,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.updates.notify(instanceID, "binding_changed")
	requestID := randomHex(16)
	s.audit(agentproto.ActorAdminSession, requestID, instanceID, "binding.update", "", "succeeded", fmt.Sprintf("bound output subscription %d", subscription.ID))
	analysis, _ := compiler.AnalyzeMihomoRuntime(version.Content, version.RuntimeContract)
	writeJSON(w, map[string]any{"binding": binding, "runtime_analysis": analysis, "request_id": requestID})
}

func (s *Server) handleDeleteRuntimeBinding(w http.ResponseWriter, r *http.Request) {
	instanceID, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteRuntimeBinding(instanceID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.updates.notify(instanceID, "binding_changed")
	requestID := randomHex(16)
	s.audit(agentproto.ActorAdminSession, requestID, instanceID, "binding.delete", "", "succeeded", "runtime binding removed")
	writeJSON(w, map[string]any{"ok": true, "request_id": requestID})
}

func (s *Server) handlePutRuntimeDesiredState(w http.ResponseWriter, r *http.Request) {
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
		ExpectedGeneration    int64                      `json:"expected_generation"`
		CoreInstalled         bool                       `json:"desired_core_installed"`
		CoreChannel           string                     `json:"core_channel"`
		CoreVersionConstraint string                     `json:"core_version_constraint"`
		CoreVersion           string                     `json:"desired_core_version"`
		RuntimeState          string                     `json:"desired_runtime_state"`
		Integrations          map[string]json.RawMessage `json:"desired_integrations"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid desired state", http.StatusBadRequest)
		return
	}
	if body.RuntimeState != store.RuntimeRunning && body.RuntimeState != store.RuntimeStopped {
		http.Error(w, "desired_runtime_state must be running or stopped", http.StatusBadRequest)
		return
	}
	if body.CoreInstalled {
		if !exactCoreVersion.MatchString(body.CoreVersion) {
			http.Error(w, "desired_core_version must be an exact version", http.StatusBadRequest)
			return
		}
		if body.CoreChannel != "stable" && body.CoreChannel != "alpha" {
			http.Error(w, "core_channel must be stable or alpha", http.StatusBadRequest)
			return
		}
		if body.CoreChannel == "stable" && regexp.MustCompile(`(?i)(alpha|beta|rc|pre)`).MatchString(body.CoreVersion) {
			http.Error(w, "pre-release core versions require the alpha channel", http.StatusBadRequest)
			return
		}
	} else if body.CoreVersion != "" || body.RuntimeState != store.RuntimeStopped {
		http.Error(w, "an uninstalled core must have no version and remain stopped", http.StatusBadRequest)
		return
	}
	if len(body.CoreVersionConstraint) > 128 {
		http.Error(w, "core_version_constraint is too long", http.StatusBadRequest)
		return
	}
	if err := validateDesiredIntegrations(body.Integrations); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateDesiredIntegrationCapabilities(body.Integrations, instance.Capabilities); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if enabled, proxyPort, err := desiredProxyIntegration(body.Integrations); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if enabled {
		observation, observationErr := s.store.GetRuntimeObservation(instanceID)
		if !body.CoreInstalled || body.RuntimeState != store.RuntimeRunning || !currentProxyObservation(observation, observationErr, proxyPort, time.Now().UTC()) {
			http.Error(w, "proxy integration requires the currently running Agent-managed HTTP or mixed listener", http.StatusConflict)
			return
		}
	}
	requestID := randomHex(16)
	updated, err := s.store.UpdateRuntimeDesiredState(store.RuntimeDesiredState{
		InstanceID: instanceID, CoreInstalled: body.CoreInstalled, CoreChannel: body.CoreChannel,
		CoreVersionConstraint: body.CoreVersionConstraint, CoreVersion: body.CoreVersion,
		RuntimeState: body.RuntimeState, Integrations: body.Integrations,
		ActorType: agentproto.ActorAdminSession, RequestID: requestID,
	}, body.ExpectedGeneration)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.updates.notify(instanceID, "desired_state_changed")
	s.audit(agentproto.ActorAdminSession, requestID, instanceID, "desired.update", strconv.FormatInt(updated.Generation, 10), "succeeded", "runtime desired state updated")
	writeJSON(w, map[string]any{"desired": updated, "request_id": requestID})
}

func currentProxyObservation(observation store.RuntimeObservation, observationErr error, proxyPort int, now time.Time) bool {
	if observationErr != nil || observation.CoreStatus != store.RuntimeRunning || !observation.ProxyListening || observation.ProxyPort != proxyPort || (observation.ProxyKind != "mixed" && observation.ProxyKind != "http") {
		return false
	}
	observedAt, err := time.Parse(time.RFC3339, observation.ObservedAt)
	return err == nil && !observedAt.After(now.Add(5*time.Second)) && now.Sub(observedAt) <= 90*time.Second
}

func validateDesiredIntegrations(values map[string]json.RawMessage) error {
	for name, raw := range values {
		switch name {
		case integration.DockerDaemonType:
			var config integration.DockerDaemonConfig
			if err := decodeStrictIntegration(raw, &config); err != nil || config.Validate() != nil || config.Revision == "" {
				return fmt.Errorf("invalid docker_daemon integration")
			}
			if config.Enabled && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(config.ExpectedOriginalHash) {
				return fmt.Errorf("enabling docker_daemon requires the exact confirmed preview hash")
			}
		case integration.DockerDesktopType:
			var config integration.DockerDesktopConfig
			if err := decodeStrictIntegration(raw, &config); err != nil || config.Validate() != nil || config.Revision == "" {
				return fmt.Errorf("invalid docker_desktop integration or missing Business prerequisite confirmation")
			}
			if config.Enabled && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(config.ExpectedOriginalHash) {
				return fmt.Errorf("enabling docker_desktop requires the exact confirmed preview hash")
			}
		default:
			return fmt.Errorf("unknown desired integration %q", name)
		}
	}
	return nil
}

func validateDesiredIntegrationCapabilities(values map[string]json.RawMessage, capabilities []string) error {
	required := map[string]string{
		integration.DockerDaemonType:  "integration.docker_daemon",
		integration.DockerDesktopType: "integration.docker_desktop",
	}
	for name := range values {
		capability := required[name]
		found := false
		for _, value := range capabilities {
			if value == capability {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("runtime instance does not advertise capability %q", capability)
		}
	}
	return nil
}

func desiredProxyIntegration(values map[string]json.RawMessage) (bool, int, error) {
	enabled, port := false, 0
	for _, raw := range values {
		var config struct {
			Enabled   bool `json:"enabled"`
			ProxyPort int  `json:"proxy_port"`
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			return false, 0, errors.New("invalid proxy integration")
		}
		if !config.Enabled {
			continue
		}
		if enabled && port != config.ProxyPort {
			return false, 0, errors.New("enabled proxy integrations must use the same Agent-managed listener")
		}
		enabled, port = true, config.ProxyPort
	}
	return enabled, port, nil
}

func decodeStrictIntegration(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("integration contains trailing JSON")
	}
	return nil
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
	if err := s.store.CreateAgentJob(job); err != nil {
		http.Error(w, "create job failed", http.StatusInternalServerError)
		return
	}
	s.updates.notify(instanceID, "job_queued")
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
	desired, err := s.store.GetRuntimeDesiredState(instance.ID)
	if err != nil {
		http.Error(w, "desired state unavailable", http.StatusInternalServerError)
		return
	}
	jobs, err := s.store.ListRunnableAgentJobs(instance.ID, time.Now().UTC())
	if err != nil {
		http.Error(w, "jobs unavailable", http.StatusInternalServerError)
		return
	}
	response := map[string]any{"protocol_version": agentproto.Version, "desired": desired, "jobs": jobs}
	if binding, err := s.store.GetRuntimeBindingByInstance(instance.ID); err == nil {
		response["binding"] = binding
	}
	writeJSON(w, response)
}

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	var body struct {
		AgentVersion string                   `json:"agent_version"`
		Capabilities []string                 `json:"capabilities"`
		Status       string                   `json:"status"`
		Observation  store.RuntimeObservation `json:"observation"`
		Deployment   *store.Deployment        `json:"deployment,omitempty"`
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
	desired, err := s.store.GetRuntimeDesiredState(instance.ID)
	if err != nil {
		http.Error(w, "desired state unavailable", http.StatusInternalServerError)
		return
	}
	if body.Observation.ObservedGeneration < 0 || body.Observation.ObservedGeneration > desired.Generation || body.Observation.AgentUptimeSeconds < 0 {
		http.Error(w, "invalid observed generation or uptime", http.StatusBadRequest)
		return
	}
	if len(body.Observation.SelectedProxies) > 256 || len(body.Observation.SelectionNotice) > 512 || len(body.Observation.Integrations) > 32 {
		http.Error(w, "runtime observation exceeds its limits", http.StatusBadRequest)
		return
	}
	if err := validateRuntimeObservation(body.Observation); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	previousObservation, _ := s.store.GetRuntimeObservation(instance.ID)
	if err := s.store.UpdateRuntimeHeartbeat(instance.ID, body.AgentVersion, body.Capabilities, body.Status, time.Now().UTC()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := s.store.SaveRuntimeObservation(body.Observation); err != nil {
		http.Error(w, "save observation failed", http.StatusInternalServerError)
		return
	}
	if err := s.saveIntegrationStates(instance.ID, desired, body.Observation); err != nil {
		http.Error(w, "save integration state failed", http.StatusBadRequest)
		return
	}
	if desired.RequestID != "" && body.Observation.ObservedGeneration == desired.Generation && previousObservation.ObservedGeneration < desired.Generation {
		s.audit(agentproto.ActorAgentReconcile, desired.RequestID, instance.ID, "desired.reconciled", strconv.FormatInt(desired.Generation, 10), "succeeded", "runtime desired generation reconciled")
	}
	if body.Deployment != nil {
		body.Deployment.InstanceID = instance.ID
		body.Deployment.Error = safeAgentError(body.Deployment.Error)
		if body.Deployment.ActorType == "" {
			body.Deployment.ActorType = agentproto.ActorScheduler
		}
		if body.Deployment.RequestID == "" || !validAuditActor(body.Deployment.ActorType) {
			http.Error(w, "deployment audit identity is invalid", http.StatusBadRequest)
			return
		}
		if err := validateDeployment(*body.Deployment); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		deployment, err := s.store.AddDeployment(*body.Deployment)
		if err != nil {
			http.Error(w, "save deployment failed", http.StatusInternalServerError)
			return
		}
		s.audit(agentproto.ActorAgentReconcile, deployment.RequestID, instance.ID, "deployment."+deployment.Status, deployment.RemoteRevision, deployment.Status, deployment.Error)
	}
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
	if (value.ProxyPort == 0 && value.ProxyKind != "") || (value.ProxyPort > 0 && value.ProxyKind != "mixed" && value.ProxyKind != "http" && value.ProxyKind != "socks5") {
		return errors.New("runtime proxy listener kind is invalid")
	}
	if value.CoreStatus != store.RuntimeNotInstalled && value.CoreStatus != store.RuntimeStopped && value.CoreStatus != store.RuntimeStarting && value.CoreStatus != store.RuntimeRunning && value.CoreStatus != store.RuntimeFailed {
		return errors.New("runtime core status is invalid")
	}
	for group, proxy := range value.SelectedProxies {
		if group == "" || proxy == "" || len(group) > 256 || len(proxy) > 256 || strings.ContainsAny(group+proxy, "\r\n\x00") {
			return errors.New("selected proxy observation is invalid")
		}
	}
	return nil
}

func validateDeployment(value store.Deployment) error {
	validStatus := value.Status == "pending" || value.Status == "validating" || value.Status == "applying" || value.Status == "active" || value.Status == "rolled_back" || value.Status == "failed"
	if !validStatus || len(value.RemoteRevision) > 256 || len(value.PreviousRevision) > 256 || len(value.MihomoVersion) > 128 || len(value.Validation) > 128 || len(value.Error) > 1024 {
		return errors.New("deployment record is invalid")
	}
	for _, digest := range []string{value.ArtifactHash, value.EffectiveHash} {
		if digest != "" && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
			return errors.New("deployment digest is invalid")
		}
	}
	return nil
}

func (s *Server) saveIntegrationStates(instanceID int64, desired store.RuntimeDesiredState, observation store.RuntimeObservation) error {
	types := map[string]string{
		integration.DockerDaemonType:  "system/rootful-docker-engine",
		integration.DockerDesktopType: "system/docker-desktop-business",
	}
	for name := range observation.Integrations {
		if _, ok := types[name]; !ok {
			return fmt.Errorf("unknown observed integration %q", name)
		}
	}
	for name, scope := range types {
		desiredRaw, desiredExists := desired.Integrations[name]
		observedRaw, observedExists := observation.Integrations[name]
		if !desiredExists && !observedExists {
			continue
		}
		desiredState, revision := "disabled", ""
		if desiredExists {
			var config struct {
				Enabled  bool   `json:"enabled"`
				Revision string `json:"revision"`
			}
			if err := json.Unmarshal(desiredRaw, &config); err != nil {
				return err
			}
			if config.Enabled {
				desiredState = "active"
			}
			revision = config.Revision
		}
		status := integration.DockerStatus{State: "disabled"}
		if observedExists {
			if err := decodeStrictIntegration(observedRaw, &status); err != nil {
				return fmt.Errorf("invalid observed integration %q", name)
			}
		}
		validation := "pending"
		if status.State == desiredState {
			validation = "verified"
		} else if status.State == "failed" || status.State == "conflict" {
			validation = "failed"
		}
		_, err := s.store.UpsertIntegrationState(store.IntegrationState{
			InstanceID: instanceID, Type: name, Scope: scope,
			DesiredState: desiredState, ObservedState: status.State, ConfigRevision: revision,
			OriginalHash: status.OriginalHash, Conflict: safeAgentError(status.Conflict), Validation: validation,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func validAuditActor(value string) bool {
	return value == agentproto.ActorAdminSession || value == agentproto.ActorLocalCLI || value == agentproto.ActorAgentReconcile || value == agentproto.ActorScheduler
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
	s.audit(agentproto.ActorAgentReconcile, updated.RequestID, instance.ID, "job."+updated.Type+"."+updated.Status, updated.ID, updated.Status, safeAgentError(body.Error))
	writeJSON(w, updated)
}

var localAuditActions = map[string]bool{
	"mihomo.install": true, "mihomo.restart": true, "mihomo.rollback": true,
	"subscription.update": true, "subscription.rollback": true,
	"integration.docker_daemon.enable": true, "integration.docker_daemon.disable": true,
	"integration.docker_desktop.enable": true, "integration.docker_desktop.disable": true,
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

func (s *Server) handleAgentArtifact(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	bindingID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad binding id", http.StatusBadRequest)
		return
	}
	binding, err := s.store.GetRuntimeBinding(bindingID)
	if err != nil || binding.InstanceID != instance.ID {
		http.Error(w, "binding not found", http.StatusNotFound)
		return
	}
	subscription, err := s.store.GetOutputSubscription(binding.OutputSubscriptionID)
	if err != nil || !subscription.Enabled {
		http.Error(w, "bound subscription unavailable", http.StatusGone)
		return
	}
	version, err := s.store.GetTemplateVersion(subscription.TemplateVersionID)
	if err != nil || version.RuntimeContract != binding.RuntimeContract || binding.RuntimeContract != compiler.RuntimeContractMihomoAgentV1 {
		http.Error(w, "runtime contract mismatch", http.StatusConflict)
		return
	}
	artifact, err := s.store.GetSubscriptionArtifact(subscription.ID)
	if err != nil || len(artifact.Body) == 0 || artifact.BlockedReason != "" {
		http.Error(w, "bound artifact unavailable", http.StatusServiceUnavailable)
		return
	}
	etag := `"` + artifact.Revision + `"`
	hash := sha256.Sum256(artifact.Body)
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Submux-Revision", artifact.Revision)
	w.Header().Set("X-Submux-SHA256", hex.EncodeToString(hash[:]))
	w.Header().Set("Content-Type", artifact.ContentType)
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(artifact.Body)
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
