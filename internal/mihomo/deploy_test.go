package mihomo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

type fakeValidator struct{ err error }

func (v fakeValidator) ValidateConfig(context.Context, string) error { return v.err }

type fakeService struct{ reloads, stops int }

func (s *fakeService) ReloadOrRestart(context.Context) error { s.reloads++; return nil }
func (s *fakeService) Stop(context.Context) error            { s.stops++; return nil }

type sequenceVerifier struct {
	errors []error
	calls  int
}

func (v *sequenceVerifier) VerifyRuntime(context.Context, string) error {
	index := v.calls
	v.calls++
	if index < len(v.errors) {
		return v.errors[index]
	}
	return nil
}

func artifactHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestAgentOverlayOwnsOnlyControllerFields(t *testing.T) {
	base := []byte("mixed-port: 7890\nallow-lan: false\nsecret: public\nexternal-controller: 0.0.0.0:9090\ntun:\n  enable: false\n")
	effective, err := ApplyAgentOverlay(base, "127.0.0.1:9090", "local-secret")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(effective, &root); err != nil {
		t.Fatal(err)
	}
	if root["external-controller"] != "127.0.0.1:9090" || root["secret"] != "local-secret" || root["mixed-port"] != 7890 || root["allow-lan"] != false {
		t.Fatalf("unexpected effective config: %#v", root)
	}
	tun, _ := root["tun"].(map[string]any)
	if tun["enable"] != false {
		t.Fatalf("overlay changed TUN config: %#v", tun)
	}
}

func TestProxyEndpointFollowsCandidateWithoutChangingIt(t *testing.T) {
	for _, test := range []struct {
		config string
		port   int
		kind   string
	}{
		{"mixed-port: 7891\nport: 7892\n", 7891, "mixed"},
		{"port: 8080\n", 8080, "http"},
		{"socks-port: 1080\n", 1080, "socks5"},
		{"rules: []\n", 0, ""},
	} {
		port, kind, err := ProxyEndpoint([]byte(test.config))
		if err != nil || port != test.port || kind != test.kind {
			t.Fatalf("ProxyEndpoint(%q) = %d/%q, %v", test.config, port, kind, err)
		}
	}
	if _, _, err := ProxyEndpoint([]byte("mixed-port: 70000\n")); err == nil {
		t.Fatal("invalid candidate proxy port was accepted")
	}
}

func TestDeployValidationFailurePreservesCurrentAndRuntimeFailureRollsBack(t *testing.T) {
	root := filepath.Join(t.TempDir(), "configs")
	service := &fakeService{}
	firstVerifier := &sequenceVerifier{}
	deployer := &Deployer{Root: root, ControllerAddr: "127.0.0.1:9090", Secret: "secret", Validator: fakeValidator{}, Service: service, Verifier: firstVerifier}
	first := []byte("mixed-port: 7890\nproxy-groups: []\nrules: []\n")
	result, err := deployer.Apply(context.Background(), "rev-1", artifactHash(first), first)
	if err != nil || result.Status != "active" {
		t.Fatalf("first deploy: %#v, %v", result, err)
	}
	currentBefore, _ := os.ReadFile(filepath.Join(root, "current", "base.yaml"))
	invalid := &Deployer{Root: root, ControllerAddr: "127.0.0.1:9090", Secret: "secret", Validator: fakeValidator{err: errors.New("invalid")}, Service: service, Verifier: firstVerifier}
	second := []byte("mixed-port: 7891\nproxy-groups: []\nrules: []\n")
	if result, err := invalid.Apply(context.Background(), "rev-invalid", artifactHash(second), second); err == nil || result.Validation != "rejected" {
		t.Fatalf("invalid candidate: %#v, %v", result, err)
	}
	currentAfter, _ := os.ReadFile(filepath.Join(root, "current", "base.yaml"))
	if string(currentAfter) != string(currentBefore) {
		t.Fatal("static validation failure changed current")
	}
	rollbackVerifier := &sequenceVerifier{errors: []error{errors.New("candidate unhealthy"), nil}}
	rollbackDeployer := &Deployer{Root: root, ControllerAddr: "127.0.0.1:9090", Secret: "secret", Validator: fakeValidator{}, Service: service, Verifier: rollbackVerifier}
	result, err = rollbackDeployer.Apply(context.Background(), "rev-2", artifactHash(second), second)
	if err == nil || result.Status != "rolled_back" || !result.RolledBack {
		t.Fatalf("runtime failure did not roll back: %#v, %v", result, err)
	}
	metadata, err := readMetadata(filepath.Join(root, "current", "metadata.json"))
	if err != nil || metadata.Revision != "rev-1" {
		t.Fatalf("current revision after rollback: %#v, %v", metadata, err)
	}
}

func TestExplicitLastGoodRollbackSwapsVersionsAndRestoresCurrentOnFailure(t *testing.T) {
	for _, failRollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure-restores-current"}[failRollback], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "configs")
			service := &fakeService{}
			deployer := &Deployer{Root: root, ControllerAddr: "127.0.0.1:9090", Secret: "secret", Validator: fakeValidator{}, Service: service, Verifier: &sequenceVerifier{}}
			first := []byte("mixed-port: 7890\nproxy-groups: []\nrules: []\n")
			second := []byte("mixed-port: 7891\nproxy-groups: []\nrules: []\n")
			if _, err := deployer.Apply(context.Background(), "rev-1", artifactHash(first), first); err != nil {
				t.Fatal(err)
			}
			if _, err := deployer.Apply(context.Background(), "rev-2", artifactHash(second), second); err != nil {
				t.Fatal(err)
			}
			if failRollback {
				deployer.Verifier = &sequenceVerifier{errors: []error{errors.New("last-good unhealthy"), nil}}
			} else {
				deployer.Verifier = &sequenceVerifier{}
			}
			result, err := deployer.Rollback(context.Background())
			current, currentErr := readMetadata(filepath.Join(root, "current", "metadata.json"))
			lastGood, lastGoodErr := readMetadata(filepath.Join(root, "last-good", "metadata.json"))
			if currentErr != nil || lastGoodErr != nil {
				t.Fatalf("rollback metadata missing: current=%v last-good=%v", currentErr, lastGoodErr)
			}
			if failRollback {
				if err == nil || current.Revision != "rev-2" || lastGood.Revision != "rev-1" {
					t.Fatalf("failed rollback did not restore original state: result=%#v err=%v current=%q last-good=%q", result, err, current.Revision, lastGood.Revision)
				}
				return
			}
			if err != nil || result.Revision != "rev-1" || current.Revision != "rev-1" || lastGood.Revision != "rev-2" {
				t.Fatalf("successful rollback did not swap versions: result=%#v err=%v current=%q last-good=%q", result, err, current.Revision, lastGood.Revision)
			}
		})
	}
}
