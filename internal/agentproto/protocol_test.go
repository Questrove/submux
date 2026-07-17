package agentproto

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

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
