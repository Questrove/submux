package runtimeapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

func TestMihomoExecutorBuildsCandidateWithPersistedTrafficPolicy(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.SetTrafficPolicy(runtimeapi.TrafficPolicyDirect, "op-policy", time.Now()); err != nil {
		t.Fatal(err)
	}
	executor := &MihomoExecutor{
		State: state, ControlEndpoint: filepath.Join(t.TempDir(), "mihomo.sock"), Platform: "linux",
	}
	detailed, err := executor.buildDetailedCandidate(
		[]byte("mode: rule\nproxy-groups:\n  - name: PROXY\n    type: select\n    proxies: [DIRECT]\nrules: [MATCH,PROXY]\n"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	if err := yaml.Unmarshal(detailed.YAML, &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate["mode"] != runtimeapi.TrafficPolicyDirect || detailed.TrafficPolicy != runtimeapi.TrafficPolicyDirect {
		t.Fatalf("candidate traffic policy=%#v detail=%q", candidate["mode"], detailed.TrafficPolicy)
	}
}

func TestMihomoExecutorObservesAppliedTrafficPolicyFromCurrentConfig(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current")
	if err := os.MkdirAll(current, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "config.yaml"), []byte("mode: global\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executor := &MihomoExecutor{ConfigRoot: root}
	if applied, err := executor.AppliedTrafficPolicy(t.Context()); err != nil || applied != runtimeapi.TrafficPolicyGlobal {
		t.Fatalf("applied traffic policy=%q err=%v", applied, err)
	}
}

func TestCoordinatorAcceptsOnlyTrafficPolicySettingParameter(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	coordinator := &Coordinator{
		State: state,
		Executor: executorFunc{execute: func(context.Context, runtimeapi.Operation, StageReporter) (*runtimeapi.OperationResult, error) {
			return &runtimeapi.OperationResult{}, nil
		}},
	}
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	snapshot, err := coordinator.Observe(t.Context(), peer)
	if err != nil {
		t.Fatal(err)
	}
	valid := runtimeapi.CreateOperationRequest{
		RequestID: "traffic-policy-rule", IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{Kind: runtimeapi.ActionSetTrafficPolicy, Params: runtimeapi.ActionParams{TrafficPolicy: runtimeapi.TrafficPolicyRule}},
	}
	if _, _, err := coordinator.Execute(t.Context(), peer, "tui", "test", valid); err != nil {
		t.Fatalf("valid traffic policy: %v", err)
	}
	invalid := valid
	invalid.RequestID = "traffic-policy-invalid"
	invalid.Action.Params.TrafficPolicy = "automatic"
	if _, _, err := coordinator.Execute(t.Context(), peer, "tui", "test", invalid); err == nil || !strings.Contains(err.Error(), "traffic_policy") {
		t.Fatalf("invalid traffic policy error=%v", err)
	}
}
