package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"submux/internal/agentclient"
	"submux/internal/agentproto"
	"submux/internal/compiler"
	"submux/internal/store"
)

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

func TestRuntimeSubscriptionURLUsesOneTimeNonPersistentRelay(t *testing.T) {
	st := newTestStore(t)
	app := New(st, nil)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	admin := initAndClient(t, srv)
	publicKey, privateKey, err := agentproto.GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateRuntimeInstance(store.RuntimeInstance{
		Name: "edge-subscriptions", DeviceKey: agentproto.EncodePublicKey(publicKey), OS: "linux", Arch: "amd64",
		Capabilities: []string{"subscription.manage"}, AgentVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	secretURL := "https://provider.example/subscription?token=private-value"
	staged := mustPost(t, admin, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(instance.ID, 10)+"/secrets", `{"kind":"subscription_url","value":"`+secretURL+`"}`)
	if staged.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(staged.Body)
		t.Fatalf("stage runtime secret status %d: %s", staged.StatusCode, message)
	}
	if staged.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("runtime secret staging is cacheable: %q", staged.Header.Get("Cache-Control"))
	}
	var stagedBody struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(staged.Body).Decode(&stagedBody); err != nil {
		t.Fatal(err)
	}
	staged.Body.Close()
	if len(stagedBody.Ref) != 48 {
		t.Fatalf("runtime secret ref = %q", stagedBody.Ref)
	}
	jobResponse := mustPost(t, admin, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(instance.ID, 10)+"/jobs", `{"type":"add_runtime_subscription","params":{"name":"临时机场","secret_ref":"`+stagedBody.Ref+`"},"deadline_seconds":300}`)
	if jobResponse.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(jobResponse.Body)
		t.Fatalf("create runtime subscription job status %d: %s", jobResponse.StatusCode, message)
	}
	var job store.AgentJob
	if err := json.NewDecoder(jobResponse.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	jobResponse.Body.Close()
	persisted, err := st.GetAgentJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedJSON, _ := json.Marshal(persisted)
	if bytes.Contains(persistedJSON, []byte(secretURL)) || bytes.Contains(persistedJSON, []byte("private-value")) {
		t.Fatalf("runtime subscription URL was persisted in the control plane: %s", persistedJSON)
	}

	consumed := signedDeviceRequest(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/agent/secrets/"+stagedBody.Ref, instance.ID, privateKey, []byte(`{}`))
	if consumed.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(consumed.Body)
		t.Fatalf("consume runtime secret status %d: %s", consumed.StatusCode, message)
	}
	if consumed.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("runtime secret response is cacheable: %q", consumed.Header.Get("Cache-Control"))
	}
	var secret agentclient.RuntimeSecret
	if err := json.NewDecoder(consumed.Body).Decode(&secret); err != nil {
		t.Fatal(err)
	}
	consumed.Body.Close()
	if secret.Kind != runtimeSecretSubscriptionURL || secret.Value != secretURL {
		t.Fatalf("consumed runtime secret = %#v", secret)
	}
	second := signedDeviceRequest(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/agent/secrets/"+stagedBody.Ref, instance.ID, privateKey, []byte(`{}`))
	second.Body.Close()
	if second.StatusCode != http.StatusGone {
		t.Fatalf("consumed runtime secret remained readable: %d", second.StatusCode)
	}
}

func TestAgentFetchesPublishedMihomoPlatformSubscription(t *testing.T) {
	st := newTestStore(t)
	app := New(st, nil)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	publicKey, privateKey, err := agentproto.GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateRuntimeInstance(store.RuntimeInstance{
		Name: "platform-reader", DeviceKey: agentproto.EncodePublicKey(publicKey), OS: "linux", Arch: "amd64",
		Capabilities: []string{"subscription.manage"}, AgentVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("mixed-port: 7890\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\n")
	subscription := savePublishedSubscription(t, st, store.OutputSubscription{
		Name: "服务器配置", Engine: compiler.EngineMihomo, Token: "platform-agent-token", Enabled: true,
	}, &store.SubscriptionArtifact{Body: body, ContentType: "text/yaml", Revision: "platform-revision"})
	response := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/platform-subscriptions/"+strconv.FormatInt(subscription.ID, 10), instance.ID, privateKey, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("fetch platform subscription status %d: %s", response.StatusCode, message)
	}
	got, _ := io.ReadAll(response.Body)
	if !bytes.Equal(got, body) || response.Header.Get("X-Submux-Revision") != "platform-revision" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("platform subscription response body=%q headers=%v", got, response.Header)
	}
	singBox := savePublishedSubscription(t, st, store.OutputSubscription{
		Name: "sing-box 配置", Engine: compiler.EngineSingBox, Token: "sing-box-agent-token", Enabled: true,
	}, &store.SubscriptionArtifact{Body: []byte(`{"outbounds":[]}`), ContentType: "application/json", Revision: "sing-box-revision"})
	wrongEngine := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/platform-subscriptions/"+strconv.FormatInt(singBox.ID, 10), instance.ID, privateKey, nil)
	wrongEngine.Body.Close()
	if wrongEngine.StatusCode != http.StatusNotFound {
		t.Fatalf("Agent fetched non-Mihomo platform subscription: %d", wrongEngine.StatusCode)
	}
	limitedPublic, limitedPrivate, _ := agentproto.GenerateDeviceKey()
	limited, err := st.CreateRuntimeInstance(store.RuntimeInstance{
		Name: "platform-reader-without-capability", DeviceKey: agentproto.EncodePublicKey(limitedPublic), OS: "linux", Arch: "amd64", AgentVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	forbidden := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/platform-subscriptions/"+strconv.FormatInt(subscription.ID, 10), limited.ID, limitedPrivate, nil)
	forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("Agent without subscription capability fetched platform subscription: %d", forbidden.StatusCode)
	}
}

func TestRuntimeEnrollmentOneShotJobsObservationAndRevocation(t *testing.T) {
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
		"capabilities": []string{"mihomo.core.manage", "mihomo.restart", "subscription.manage", "mihomo.runtime.observe", "agent.resource.proxy"},
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

	deprecatedRequest, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/desired", bytes.NewBufferString(`{}`))
	deprecatedRequest.Header.Set("Content-Type", "application/json")
	deprecatedResponse := mustDo(t, admin, deprecatedRequest)
	deprecatedResponse.Body.Close()
	if deprecatedResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("removed desired endpoint status %d", deprecatedResponse.StatusCode)
	}
	expiredJob := store.AgentJob{Job: agentproto.Job{
		ID: "expired-before-new-operation", ProtocolVersion: agentproto.Version, InstanceID: identity.InstanceID,
		Type: agentproto.JobRestartCore, Params: json.RawMessage(`{}`), Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: "expired-request", Deadline: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}}
	if err := st.CreateAgentJob(expiredJob); err != nil {
		t.Fatal(err)
	}

	job := mustPost(t, admin, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/jobs", `{"type":"restart_core","params":{},"deadline_seconds":120,"reason":"operator request"}`)
	if job.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(job.Body)
		t.Fatalf("job status %d: %s", job.StatusCode, message)
	}
	var jobValue store.AgentJob
	_ = json.NewDecoder(job.Body).Decode(&jobValue)
	job.Body.Close()
	if expired, err := st.GetAgentJob(expiredJob.ID); err != nil || expired.Status != agentproto.JobExpired {
		t.Fatalf("expired job still blocked later work: %#v, %v", expired, err)
	}

	state := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/state", identity.InstanceID, privateKey, nil)
	if state.StatusCode != http.StatusOK {
		t.Fatalf("agent state status %d", state.StatusCode)
	}
	var stateBody struct {
		ProtocolVersion int              `json:"protocol_version"`
		Jobs            []store.AgentJob `json:"jobs"`
	}
	_ = json.NewDecoder(state.Body).Decode(&stateBody)
	state.Body.Close()
	if stateBody.ProtocolVersion != agentproto.Version || len(stateBody.Jobs) != 1 || stateBody.Jobs[0].ID != jobValue.ID {
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

	bindingRequest, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/binding", bytes.NewBufferString(`{}`))
	bindingRequest.Header.Set("Content-Type", "application/json")
	bindingResponse := mustDo(t, admin, bindingRequest)
	bindingResponse.Body.Close()
	if bindingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("removed binding endpoint status %d", bindingResponse.StatusCode)
	}
	for _, removedType := range []string{"update_subscription", "collect_diagnostics"} {
		response := mustPost(t, admin, srv.URL+"/api/runtime/instances/"+strconv.FormatInt(identity.InstanceID, 10)+"/jobs", `{"type":"`+removedType+`","params":{},"deadline_seconds":120}`)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("removed job type %s returned %d", removedType, response.StatusCode)
		}
	}
	artifact := signedDeviceRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/agent/bindings/1/artifact", identity.InstanceID, privateKey, nil)
	artifact.Body.Close()
	if artifact.StatusCode != http.StatusNotFound {
		t.Fatalf("removed artifact endpoint status %d", artifact.StatusCode)
	}

	heartbeatBody, _ := json.Marshal(map[string]any{
		"agent_version": "test", "capabilities": []string{"mihomo.core.manage", "mihomo.restart", "subscription.manage", "mihomo.runtime.observe", "agent.runtime.observe", "agent.resource.proxy"}, "status": "online",
		"observation": map[string]any{
			"remote_revision": "revision", "applied_revision": "revision",
			"core_version": "v1.19.10", "previous_core_version": "v1.19.9", "core_status": "running",
			"agent_uptime_seconds": 61, "proxy_listening": true, "proxy_port": 7890, "proxy_kind": "mixed", "controller_port": 9090,
			"selected_proxies": map[string]string{"PROXY": "Node"}, "last_good_revision": "previous", "resource_proxy_mode": "custom", "resource_proxy_url": "socks5://127.0.0.1:1080",
			"operation": map[string]any{"request_id": jobValue.RequestID, "job_id": jobValue.ID, "kind": "restart_core", "phase": "completed", "status": "succeeded", "started_at": time.Now().UTC().Format(time.RFC3339), "updated_at": time.Now().UTC().Format(time.RFC3339), "finished_at": time.Now().UTC().Format(time.RFC3339)},
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
		Runtime runtimeInstanceView `json:"runtime"`
		Audit   []store.AuditEvent  `json:"audit"`
	}
	if err := json.NewDecoder(detail.Body).Decode(&detailBody); err != nil {
		t.Fatal(err)
	}
	detail.Body.Close()
	if detailBody.Runtime.Observation.Operation == nil || detailBody.Runtime.Observation.Operation.JobID != jobValue.ID || detailBody.Runtime.Observation.ResourceProxyMode != "custom" || detailBody.Runtime.Observation.ResourceProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("heartbeat records were not closed into the runtime view: %#v", detailBody)
	}
	foundLocal, foundJobResult := false, false
	for _, event := range detailBody.Audit {
		foundLocal = foundLocal || event.ActorType == agentproto.ActorLocalCLI && event.RequestID == "local-audit-request-1"
		foundJobResult = foundJobResult || event.ActorType == agentproto.ActorAgent && event.Action == "job.restart_core.succeeded" && event.RequestID == jobValue.RequestID
	}
	if !foundLocal || !foundJobResult {
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
