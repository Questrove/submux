package agentproto

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestResourceProxyAcceptsExplicitHTTPAndSOCKS5Endpoints(t *testing.T) {
	for _, value := range []ResourceProxy{
		{Mode: ResourceProxyDirect},
		{Mode: ResourceProxyCustom, URL: "http://127.0.0.1:1080"},
		{Mode: ResourceProxyCustom, URL: "socks5://proxy.example:7890"},
	} {
		if err := ValidateResourceProxy(value); err != nil {
			t.Fatalf("proxy %#v was rejected: %v", value, err)
		}
	}
	for _, value := range []ResourceProxy{
		{Mode: ResourceProxyCustom, URL: ""},
		{Mode: ResourceProxyCustom, URL: "https://proxy.example:443"},
		{Mode: ResourceProxyCustom, URL: "http://user:pass@127.0.0.1:1080"},
		{Mode: ResourceProxyCustom, URL: "http://127.0.0.1:1080/path"},
		{Mode: ResourceProxyDirect, URL: "http://127.0.0.1:1080"},
		{Mode: "current_mihomo"},
	} {
		if err := ValidateResourceProxy(value); err == nil {
			t.Fatalf("unsafe proxy %#v was accepted", value)
		}
	}
}

func TestValidateJobRejectsUnknownFieldsAndMissingCapability(t *testing.T) {
	now := time.Now().UTC()
	job := Job{
		ID: "job", RequestID: "request", InstanceID: 1, ProtocolVersion: Version,
		Type: JobSelectProxy, Status: JobQueued, ActorType: ActorAdminSession,
		Deadline: now.Add(time.Minute).Format(time.RFC3339),
		Params:   json.RawMessage(`{"group":"PROXY","proxy":"A","command":"rm"}`),
	}
	if err := ValidateJob(job, []string{"mihomo.proxy.select"}, now); err == nil {
		t.Fatal("unknown job parameter was accepted")
	}
	job.Params = json.RawMessage(`{"group":"PROXY","proxy":"A"}`)
	if err := ValidateJob(job, nil, now); err == nil {
		t.Fatal("missing capability was accepted")
	}
	if err := ValidateJob(job, []string{"mihomo.proxy.select"}, now); err != nil {
		t.Fatalf("valid job rejected: %v", err)
	}
}

func TestOneShotMutationJobsRequireBoundedTypedParameters(t *testing.T) {
	now := time.Now().UTC()
	makeJob := func(jobType string, params string) Job {
		return Job{
			ID: "job", RequestID: "request", InstanceID: 1, ProtocolVersion: Version,
			Type: jobType, Params: json.RawMessage(params), Status: JobQueued, ActorType: ActorAdminSession,
			Deadline: now.Add(time.Minute).Format(time.RFC3339),
		}
	}
	for _, test := range []struct {
		job        Job
		capability string
	}{
		{makeJob(JobAddRuntimeSubscription, `{"name":"测试机场","secret_ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), "subscription.manage"},
		{makeJob(JobAddRuntimeSubscription, `{"name":"平台配置","platform_subscription_id":12}`), "subscription.manage"},
		{makeJob(JobEditRuntimeSubscription, `{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","name":"备用机场"}`), "subscription.manage"},
		{makeJob(JobRefreshRuntimeSubscription, `{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`), "subscription.manage"},
		{makeJob(JobActivateRuntimeSubscription, `{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`), "subscription.manage"},
		{makeJob(JobDeleteRuntimeSubscription, `{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`), "subscription.manage"},
		{makeJob(JobConfigureResourceProxy, `{"resource_proxy":{"mode":"custom","url":"socks5://127.0.0.1:1080"}}`), "agent.resource.proxy"},
		{makeJob(JobListCoreVersions, `{"channel":"stable"}`), "mihomo.release.list"},
		{makeJob(JobInstallCore, `{"channel":"stable","version":"v1.19.28"}`), "mihomo.core.manage"},
		{makeJob(JobInstallCore, `{"channel":"alpha","version":"alpha-e911985"}`), "mihomo.core.manage"},
		{makeJob(JobStartCore, `{}`), "mihomo.core.manage"},
	} {
		if err := ValidateJob(test.job, []string{test.capability}, now); err != nil {
			t.Fatalf("valid %s job rejected: %v", test.job.Type, err)
		}
	}
	for _, job := range []Job{
		makeJob(JobAddRuntimeSubscription, `{"name":"测试机场","secret_ref":"https://secret.example"}`),
		makeJob(JobAddRuntimeSubscription, `{"name":"没有来源"}`),
		makeJob(JobAddRuntimeSubscription, `{"name":"来源冲突","secret_ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","platform_subscription_id":12}`),
		makeJob(JobRefreshRuntimeSubscription, `{"id":"../config"}`),
		makeJob(JobConfigureResourceProxy, `{"resource_proxy":{"mode":"custom","url":"https://proxy.example:443"}}`),
		makeJob(JobListCoreVersions, `{"channel":"nightly"}`),
		makeJob(JobInstallCore, `{"channel":"stable","version":"latest"}`),
		makeJob(JobInstallCore, `{"channel":"stable","version":"v1.20.0-rc1"}`),
	} {
		if err := ValidateJob(job, []string{RequiredCapability(job.Type)}, now); err == nil {
			t.Fatalf("invalid %s job was accepted", job.Type)
		}
	}
}

func TestOneShotMutationResultsAreValidated(t *testing.T) {
	for jobType, raw := range map[string]string{
		JobAddRuntimeSubscription:    `{"subscription":{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","name":"测试机场","host":"provider.example","revision":"abc","active":false},"status":"saved"}`,
		JobDeleteRuntimeSubscription: `{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","deleted":true}`,
		JobConfigureResourceProxy:    `{"resource_proxy":{"mode":"direct"}}`,
		JobListCoreVersions:          `{"channel":"stable","versions":["v1.19.28","v1.19.27"]}`,
		JobInstallCore:               `{"core_status":"stopped","core_version":"v1.19.28"}`,
		JobStopCore:                  `{"core_status":"stopped"}`,
	} {
		if err := ValidateJobResult(jobType, JobSucceeded, json.RawMessage(raw)); err != nil {
			t.Fatalf("valid %s result rejected: %v", jobType, err)
		}
	}
	if err := ValidateJobResult(JobInstallCore, JobSucceeded, json.RawMessage(`{"core_status":"unknown"}`)); err == nil {
		t.Fatal("invalid core result was accepted")
	}
	for _, raw := range []string{
		`{"channel":"stable","versions":["v1.19.28","alpha-e911985"]}`,
		`{"channel":"alpha","versions":["alpha-e911985","alpha-e911985"]}`,
	} {
		if err := ValidateJobResult(JobListCoreVersions, JobSucceeded, json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid release list result was accepted: %s", raw)
		}
	}
}

func TestJobTransitionPreventsReplay(t *testing.T) {
	if !ValidTransition(JobQueued, JobRunning) || !ValidTransition(JobRunning, JobSucceeded) {
		t.Fatal("valid job transition rejected")
	}
	if ValidTransition(JobSucceeded, JobRunning) || ValidTransition(JobRunning, JobQueued) {
		t.Fatal("terminal or running job was made replayable")
	}
}

func TestSignedRequestVerificationAndNonceReplay(t *testing.T) {
	publicKey, privateKey, err := GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body := []byte(`{"status":"running"}`)
	request, _ := http.NewRequest(http.MethodPost, "https://example.test/api/agent/heartbeat?full=1", bytes.NewReader(body))
	if err := SignRequest(request, 42, privateKey, body, now); err != nil {
		t.Fatal(err)
	}
	used := false
	verify := func() error {
		request.Body = io.NopCloser(bytes.NewReader(body))
		_, _, err := VerifyRequest(request, VerifyOptions{Now: now, PublicKey: publicKey, UseNonce: func(int64, string, time.Time) bool {
			if used {
				return false
			}
			used = true
			return true
		}})
		return err
	}
	if err := verify(); err != nil {
		t.Fatalf("signed request rejected: %v", err)
	}
	if err := verify(); err == nil {
		t.Fatal("replayed nonce was accepted")
	}
}
