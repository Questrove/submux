package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"submux/internal/agentproto"
	"submux/internal/compiler"
	"submux/internal/node"
	"submux/internal/store"
)

func TestDockerDesiredStateRequiresConfirmedPreviewHash(t *testing.T) {
	missingHash := map[string]json.RawMessage{"docker_daemon": json.RawMessage(`{"enabled":true,"proxy_port":7890,"revision":"web-1"}`)}
	if err := validateDesiredIntegrations(missingHash); err == nil {
		t.Fatal("Docker enable without a confirmed preview hash was accepted")
	}
	confirmed := map[string]json.RawMessage{"docker_daemon": json.RawMessage(`{"enabled":true,"proxy_port":7890,"revision":"web-1","expected_original_hash":"` + strings.Repeat("a", 64) + `"}`)}
	if err := validateDesiredIntegrations(confirmed); err != nil {
		t.Fatalf("valid confirmed Docker preview was rejected: %v", err)
	}
}

func TestCurrentProxyObservationRejectsStaleOrNonListeningState(t *testing.T) {
	now := time.Now().UTC()
	valid := store.RuntimeObservation{
		CoreStatus: store.RuntimeRunning, ProxyListening: true, ProxyPort: 7890, ProxyKind: "mixed",
		ObservedAt: now.Format(time.RFC3339),
	}
	if !currentProxyObservation(valid, nil, 7890, now) {
		t.Fatal("fresh running proxy observation was rejected")
	}
	for name, mutate := range map[string]func(*store.RuntimeObservation){
		"stale": func(value *store.RuntimeObservation) {
			value.ObservedAt = now.Add(-91 * time.Second).Format(time.RFC3339)
		},
		"not-listening": func(value *store.RuntimeObservation) { value.ProxyListening = false },
		"wrong-port":    func(value *store.RuntimeObservation) { value.ProxyPort = 7891 },
		"socks-only":    func(value *store.RuntimeObservation) { value.ProxyKind = "socks5" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if currentProxyObservation(candidate, nil, 7890, now) {
				t.Fatal("unsafe proxy observation was accepted")
			}
		})
	}
}

func signedDeviceRequest(t *testing.T, client *http.Client, method, url string, instanceID int64, key ed25519.PrivateKey, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if err := agentproto.SignRequest(request, instanceID, key, body, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestRuntimeEnrollmentDesiredJobsArtifactAndRevocation(t *testing.T) {
	st := newTestStore(t)
	app := New(st, nil)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	admin := initAndClient(t, srv)

	created := mustPost(t, admin, srv.URL+"/api/runtime/enrollments", `{"name":"edge-1"}`)
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create enrollment status %d", created.StatusCode)
	}
	var enrollment struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(created.Body).Decode(&enrollment); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	publicKey, privateKey, err := agentproto.GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	enrollBody, _ := json.Marshal(map[string]any{
		"code": enrollment.Code, "public_key": agentproto.EncodePublicKey(publicKey),
		"os": "linux", "arch": "amd64", "agent_version": "test",
		"capabilities": []string{"mihomo.restart", "subscription.update", "mihomo.runtime.observe", "integration.docker_daemon"},
	})
	enrolled := mustPost(t, http.DefaultClient, srv.URL+"/api/agent/enroll", string(enrollBody))
	if enrolled.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(enrolled.Body)
		t.Fatalf("enroll status %d: %s", enrolled.StatusCode, message)
	}
	var identity struct {
		InstanceID int64 `json:"instance_id"`
	}
	_ = json.NewDecoder(enrolled.Body).Decode(&identity)
	enrolled.Body.Close()
	if identity.InstanceID == 0 {
		t.Fatal("enrollment returned no instance ID")
	}
	reused := mustPost(t, http.DefaultClient, srv.URL+"/api/agent/enroll", string(enrollBody))
	reused.Body.Close()
	if reused.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pairing code reuse status %d", reused.StatusCode)
	}

	desiredBody := `{"expected_generation":1,"desired_core_installed":true,"core_channel":"stable","core_version_constraint":"1.19.x","desired_core_version":"v1.19.10","desired_runtime_state":"running","desired_integrations":{"docker_daemon":{"enabled":false,"proxy_port":7890,"revision":"docker-off-1"}}}`
	desiredRequest, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/desired", bytes.NewBufferString(desiredBody))
	desiredRequest.Header.Set("Content-Type", "application/json")
	desiredResponse := mustDo(t, admin, desiredRequest)
	if desiredResponse.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(desiredResponse.Body)
		t.Fatalf("desired status %d: %s", desiredResponse.StatusCode, message)
	}
	desiredResponse.Body.Close()

	job := mustPost(t, admin, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/jobs", `{"type":"restart_core","params":{},"deadline_seconds":120,"reason":"operator request"}`)
	if job.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(job.Body)
		t.Fatalf("job status %d: %s", job.StatusCode, message)
	}
	var jobValue store.AgentJob
	_ = json.NewDecoder(job.Body).Decode(&jobValue)
	job.Body.Close()

	state := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/state", identity.InstanceID, privateKey, nil)
	if state.StatusCode != http.StatusOK {
		t.Fatalf("agent state status %d", state.StatusCode)
	}
	var stateBody struct {
		Desired store.RuntimeDesiredState `json:"desired"`
		Jobs    []store.AgentJob          `json:"jobs"`
	}
	_ = json.NewDecoder(state.Body).Decode(&stateBody)
	state.Body.Close()
	if stateBody.Desired.Generation != 2 || len(stateBody.Jobs) != 1 || stateBody.Jobs[0].ID != jobValue.ID {
		t.Fatalf("unexpected agent state: %#v", stateBody)
	}

	for _, transition := range []struct {
		status string
		body   string
	}{
		{agentproto.JobRunning, `{"status":"running"}`},
		{agentproto.JobSucceeded, `{"status":"succeeded","result":{"core_status":"running"}}`},
	} {
		response := signedDeviceRequest(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/agent/jobs/"+jobValue.ID+"/status", identity.InstanceID, privateKey, []byte(transition.body))
		if response.StatusCode != http.StatusOK {
			message, _ := io.ReadAll(response.Body)
			t.Fatalf("job transition %s status %d: %s", transition.status, response.StatusCode, message)
		}
		response.Body.Close()
	}

	// Prepare a valid Agent-compatible output and bind it through the admin API.
	sourceID, err := st.CreateSource(store.Source{Name: "manual", Kind: store.SourceKindManual})
	if err != nil {
		t.Fatal(err)
	}
	records, err := node.Import(sourceID, store.SourceKindManual, "trojan://password@example.com:443#Node")
	if err != nil {
		t.Fatal(err)
	}
	nodeIDs, err := st.CreateManualNodes(records)
	if err != nil {
		t.Fatal(err)
	}
	templates, _ := st.ListTemplates()
	var versionID int64
	for _, template := range templates {
		if template.Name == "Mihomo 服务器 Sidecar（推荐）" {
			versionID = template.CurrentVersionID
		}
	}
	subscriptionID, err := st.SaveOutputSubscription(store.OutputSubscription{
		Name: "runtime", Engine: compiler.EngineMihomo, TemplateVersionID: versionID,
		Bindings: []store.SubscriptionBinding{{Slot: "primary", NodeIDs: nodeIDs}}, Token: "runtime-token", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := app.compiler.CompileAndStore(subscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	bindingRequest, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/binding", bytes.NewBufferString(`{"output_subscription_id":`+strconv.FormatInt(subscriptionID, 10)+`,"auto_update":false,"check_interval_sec":300}`))
	bindingRequest.Header.Set("Content-Type", "application/json")
	bindingResponse := mustDo(t, admin, bindingRequest)
	if bindingResponse.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(bindingResponse.Body)
		t.Fatalf("binding status %d: %s", bindingResponse.StatusCode, message)
	}
	var bindingBody struct {
		Binding store.RuntimeBinding `json:"binding"`
	}
	_ = json.NewDecoder(bindingResponse.Body).Decode(&bindingBody)
	bindingResponse.Body.Close()

	artifact := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/bindings/"+strconv.FormatInt(bindingBody.Binding.ID, 10)+"/artifact", identity.InstanceID, privateKey, nil)
	artifactBody, _ := io.ReadAll(artifact.Body)
	artifact.Body.Close()
	if artifact.StatusCode != http.StatusOK || artifact.Header.Get("X-Submux-Revision") != compiled.Revision || !bytes.Equal(artifactBody, compiled.Body) {
		t.Fatalf("unexpected internal artifact: status=%d revision=%q", artifact.StatusCode, artifact.Header.Get("X-Submux-Revision"))
	}

	heartbeatBody, _ := json.Marshal(map[string]any{
		"agent_version": "test", "capabilities": []string{"mihomo.restart", "subscription.update", "mihomo.runtime.observe", "integration.docker_daemon"}, "status": "online",
		"observation": map[string]any{
			"observed_generation": 2, "remote_revision": compiled.Revision, "applied_revision": compiled.Revision,
			"core_version": "v1.19.10", "previous_core_version": "v1.19.9", "core_status": "running",
			"agent_uptime_seconds": 61, "proxy_listening": true, "proxy_port": 7890, "proxy_kind": "mixed", "controller_port": 9090,
			"selected_proxies": map[string]string{"PROXY": "Node"}, "last_good_revision": "previous",
			"integrations": map[string]any{"docker_daemon": map[string]any{"state": "disabled"}},
		},
		"deployment": map[string]any{
			"actor_type": "system_scheduler", "request_id": "deployment-request-1", "remote_revision": compiled.Revision,
			"artifact_hash": strings.Repeat("a", 64), "effective_hash": strings.Repeat("b", 64),
			"mihomo_version": "v1.19.10", "status": "active", "validation": "passed",
		},
	})
	heartbeat := signedDeviceRequest(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/agent/heartbeat", identity.InstanceID, privateKey, heartbeatBody)
	if heartbeat.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(heartbeat.Body)
		t.Fatalf("heartbeat status %d: %s", heartbeat.StatusCode, message)
	}
	heartbeat.Body.Close()
	localAudit := []byte(`{"request_id":"local-audit-request-1","action":"mihomo.restart","result":"succeeded","summary":"local restart"}`)
	auditResponse := signedDeviceRequest(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/agent/local-audit", identity.InstanceID, privateKey, localAudit)
	if auditResponse.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(auditResponse.Body)
		t.Fatalf("local audit status %d: %s", auditResponse.StatusCode, message)
	}
	auditResponse.Body.Close()
	detail := mustGet(t, admin, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10))
	var detailBody struct {
		Deployments  []store.Deployment       `json:"deployments"`
		Integrations []store.IntegrationState `json:"integrations"`
		Audit        []store.AuditEvent       `json:"audit"`
	}
	if err := json.NewDecoder(detail.Body).Decode(&detailBody); err != nil {
		t.Fatal(err)
	}
	detail.Body.Close()
	if len(detailBody.Deployments) != 1 || len(detailBody.Integrations) != 1 || detailBody.Integrations[0].Validation != "verified" {
		t.Fatalf("heartbeat records were not closed into the runtime view: %#v", detailBody)
	}
	foundLocal, foundReconcile := false, false
	for _, event := range detailBody.Audit {
		foundLocal = foundLocal || event.ActorType == agentproto.ActorLocalCLI && event.RequestID == "local-audit-request-1"
		foundReconcile = foundReconcile || event.Action == "desired.reconciled"
	}
	if !foundLocal || !foundReconcile {
		t.Fatalf("audit chain is incomplete: %#v", detailBody.Audit)
	}

	revoke := mustPost(t, admin, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/revoke", "")
	revoke.Body.Close()
	if revoke.StatusCode != http.StatusOK {
		t.Fatalf("revoke status %d", revoke.StatusCode)
	}
	denied := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/state", identity.InstanceID, privateKey, nil)
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device status %d", denied.StatusCode)
	}
}

func TestDeviceCanRevokeItsOwnRegistration(t *testing.T) {
	st := newTestStore(t)
	app := New(st, nil)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	publicKey, privateKey, err := agentproto.GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateRuntimeInstance(store.RuntimeInstance{Name: "local", DeviceKey: agentproto.EncodePublicKey(publicKey), OS: "linux", Arch: "amd64", Capabilities: []string{"mihomo.restart"}})
	if err != nil {
		t.Fatal(err)
	}
	response := signedDeviceRequest(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/agent/revoke-self", instance.ID, privateKey, []byte(`{}`))
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("self revoke status %d: %s", response.StatusCode, message)
	}
	response.Body.Close()
	updated, err := st.GetRuntimeInstance(instance.ID)
	if err != nil || updated.Status != store.InstanceRevoked || updated.RevokedAt == "" {
		t.Fatalf("instance was not revoked: %#v, %v", updated, err)
	}
	denied := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/state", instance.ID, privateKey, nil)
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("self-revoked device status %d", denied.StatusCode)
	}
}
