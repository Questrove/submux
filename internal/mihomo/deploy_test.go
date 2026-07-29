package mihomo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBuilder struct {
	candidate []byte
	err       error
}

func (b fakeBuilder) BuildCandidate([]byte) ([]byte, error) {
	return bytes.Clone(b.candidate), b.err
}

type fakeValidator struct{ err error }

func (v fakeValidator) ValidateConfig(context.Context, string) error { return v.err }

type fakeService struct {
	reloads int
	stops   int
}

func (s *fakeService) ReloadOrRestart(context.Context) error {
	s.reloads++
	return nil
}

func (s *fakeService) Stop(context.Context) error {
	s.stops++
	return nil
}

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

func sourceHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestDeployerRequiresRuntimeCandidateBuilder(t *testing.T) {
	source := []byte("mixed-port: 7890\n")
	deployer := &Deployer{
		Root:      filepath.Join(t.TempDir(), "configs"),
		Validator: fakeValidator{},
		Service:   &fakeService{},
		Verifier:  &sequenceVerifier{},
	}
	if _, err := deployer.Apply(context.Background(), "rev-1", sourceHash(source), source); err == nil {
		t.Fatal("deployment without the Runtime candidate builder succeeded")
	}
}

func TestPrepareValidatesAndStoresCandidateWithoutStartingRuntime(t *testing.T) {
	source := []byte("source: local\n")
	service := &fakeService{}
	verifier := &sequenceVerifier{}
	deployer := &Deployer{
		Root:      filepath.Join(t.TempDir(), "configs"),
		Builder:   fakeBuilder{candidate: []byte("mixed-port: 7890\n")},
		Validator: fakeValidator{},
		Service:   service,
		Verifier:  verifier,
	}

	result, err := deployer.Prepare(context.Background(), "rev-1", sourceHash(source), source)

	if err != nil || result.Status != "ready" || result.Validation != "passed" {
		t.Fatalf("prepared deployment: result=%#v err=%v", result, err)
	}
	if service.reloads != 0 || service.stops != 0 || verifier.calls != 0 {
		t.Fatalf("preparing candidate changed runtime: service=%#v verifier.calls=%d", service, verifier.calls)
	}
	if _, err := os.Stat(filepath.Join(deployer.Root, "current", "config.yaml")); err != nil {
		t.Fatalf("prepared candidate was not stored: %v", err)
	}
}

func TestProxyEndpointFollowsCandidateWithoutChangingIt(t *testing.T) {
	for _, test := range []struct {
		config string
		port   int
		kind   string
	}{
		{config: "mixed-port: 7891\nport: 7892\n", port: 7891, kind: "mixed"},
		{config: "port: 8080\n", port: 8080, kind: "http"},
		{config: "socks-port: 1080\n", port: 1080, kind: "socks5"},
		{config: "rules: []\n"},
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

func TestValidationFailurePreservesCurrentAndRuntimeFailureRollsBack(t *testing.T) {
	root := filepath.Join(t.TempDir(), "configs")
	service := &fakeService{}
	firstCandidate := []byte("mixed-port: 7890\nproxy-groups: []\nrules: []\n")
	firstDeployer := &Deployer{
		Root:      root,
		Builder:   fakeBuilder{candidate: firstCandidate},
		Validator: fakeValidator{},
		Service:   service,
		Verifier:  &sequenceVerifier{},
	}
	firstSource := []byte("source: first\n")
	result, err := firstDeployer.Apply(context.Background(), "rev-1", sourceHash(firstSource), firstSource)
	if err != nil || result.Status != "active" {
		t.Fatalf("first deployment: %#v, %v", result, err)
	}
	currentBefore, err := os.ReadFile(filepath.Join(root, "current", "source.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	secondSource := []byte("source: second\n")
	invalid := &Deployer{
		Root:      root,
		Builder:   fakeBuilder{candidate: []byte("mixed-port: 7891\n")},
		Validator: fakeValidator{err: errors.New("invalid")},
		Service:   service,
		Verifier:  &sequenceVerifier{},
	}
	if result, err := invalid.Apply(context.Background(), "rev-invalid", sourceHash(secondSource), secondSource); err == nil || result.Validation != "rejected" {
		t.Fatalf("invalid candidate: %#v, %v", result, err)
	}
	currentAfter, err := os.ReadFile(filepath.Join(root, "current", "source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(currentAfter, currentBefore) {
		t.Fatal("static validation failure changed the current source")
	}

	rollbackDeployer := &Deployer{
		Root:      root,
		Builder:   fakeBuilder{candidate: []byte("mixed-port: 7891\n")},
		Validator: fakeValidator{},
		Service:   service,
		Verifier:  &sequenceVerifier{errors: []error{errors.New("candidate unhealthy"), nil}},
	}
	result, err = rollbackDeployer.Apply(context.Background(), "rev-2", sourceHash(secondSource), secondSource)
	if err == nil || result.Status != "rolled_back" || !result.RolledBack {
		t.Fatalf("runtime failure did not roll back: %#v, %v", result, err)
	}
	metadata, err := readDeploymentMetadata(filepath.Join(root, "current", "metadata.json"))
	if err != nil || metadata.Revision != "rev-1" {
		t.Fatalf("current revision after rollback: %#v, %v", metadata, err)
	}
}

func TestSuccessfulConfigurationHistoryKeepsLatestThreeAndExcludesFailures(t *testing.T) {
	root := filepath.Join(t.TempDir(), "configs")
	service := &fakeService{}
	deployer := &Deployer{
		Root:      root,
		Validator: fakeValidator{},
		Service:   service,
		Verifier:  &sequenceVerifier{},
	}
	for index := 1; index <= 4; index++ {
		source := []byte("source: " + string(rune('0'+index)) + "\n")
		deployer.Builder = fakeBuilder{candidate: []byte("mixed-port: " + string(rune('0'+index)) + "890\n")}
		result, err := deployer.Apply(context.Background(), "rev-"+string(rune('0'+index)), sourceHash(source), source)
		if err != nil || result.Status != "active" || result.Error != "" {
			t.Fatalf("deployment %d: result=%#v err=%v", index, result, err)
		}
	}

	revisions := successfulHistoryRevisions(t, filepath.Join(root, "history"))
	if len(revisions) != successfulHistoryLimit || revisions["rev-1"] || !revisions["rev-2"] || !revisions["rev-3"] || !revisions["rev-4"] {
		t.Fatalf("successful configuration history=%v", revisions)
	}

	failedSource := []byte("source: failed\n")
	deployer.Builder = fakeBuilder{candidate: []byte("mixed-port: 8899\n")}
	deployer.Verifier = &sequenceVerifier{errors: []error{errors.New("candidate unhealthy"), nil}}
	if result, err := deployer.Apply(context.Background(), "rev-failed", sourceHash(failedSource), failedSource); err == nil || result.Status != "rolled_back" {
		t.Fatalf("failed deployment: result=%#v err=%v", result, err)
	}
	revisions = successfulHistoryRevisions(t, filepath.Join(root, "history"))
	if len(revisions) != successfulHistoryLimit || revisions["rev-failed"] {
		t.Fatalf("failed deployment entered successful history=%v", revisions)
	}
}

func TestHistoryEntryNameHandlesShortCandidateHash(t *testing.T) {
	for _, candidateHash := range []string{"short", strings.Repeat("../", 16)} {
		name := historyEntryName(deploymentMetadata{
			AppliedAt:     "2026-07-30T08:00:00Z",
			CandidateHash: candidateHash,
		})
		parts := strings.Split(name, "-")
		if len(parts) != 2 || len(parts[1]) != 16 || strings.ContainsAny(parts[1], `/\`) {
			t.Fatalf("history entry name=%q", name)
		}
	}
}

func successfulHistoryRevisions(t *testing.T, historyRoot string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(historyRoot)
	if err != nil {
		t.Fatalf("read successful history: %v", err)
	}
	revisions := make(map[string]bool, len(entries))
	for _, entry := range entries {
		metadata, err := readDeploymentMetadata(filepath.Join(historyRoot, entry.Name(), "metadata.json"))
		if err != nil {
			t.Fatalf("read successful history metadata: %v", err)
		}
		revisions[metadata.Revision] = true
	}
	return revisions
}

func TestExplicitKnownGoodRollbackSwapsVersionsAndRestoresCurrentOnFailure(t *testing.T) {
	for _, failRollback := range []bool{false, true} {
		name := "success"
		if failRollback {
			name = "failure-restores-current"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "configs")
			service := &fakeService{}
			deployer := &Deployer{
				Root:      root,
				Validator: fakeValidator{},
				Service:   service,
				Verifier:  &sequenceVerifier{},
			}
			firstSource := []byte("source: first\n")
			deployer.Builder = fakeBuilder{candidate: []byte("mixed-port: 7890\n")}
			if _, err := deployer.Apply(context.Background(), "rev-1", sourceHash(firstSource), firstSource); err != nil {
				t.Fatal(err)
			}
			secondSource := []byte("source: second\n")
			deployer.Builder = fakeBuilder{candidate: []byte("mixed-port: 7891\n")}
			if _, err := deployer.Apply(context.Background(), "rev-2", sourceHash(secondSource), secondSource); err != nil {
				t.Fatal(err)
			}
			if failRollback {
				deployer.Verifier = &sequenceVerifier{errors: []error{errors.New("known-good unhealthy"), nil}}
			} else {
				deployer.Verifier = &sequenceVerifier{}
			}
			result, err := deployer.Rollback(context.Background())
			current, currentErr := readDeploymentMetadata(filepath.Join(root, "current", "metadata.json"))
			previousGood, previousGoodErr := readDeploymentMetadata(filepath.Join(root, "previous-good", "metadata.json"))
			if currentErr != nil || previousGoodErr != nil {
				t.Fatalf("rollback metadata missing: current=%v previous-good=%v", currentErr, previousGoodErr)
			}
			if failRollback {
				if err == nil || current.Revision != "rev-2" || previousGood.Revision != "rev-1" {
					t.Fatalf("failed rollback did not restore original state: result=%#v err=%v current=%q previous-good=%q", result, err, current.Revision, previousGood.Revision)
				}
				return
			}
			if err != nil || result.Revision != "rev-1" || current.Revision != "rev-1" || previousGood.Revision != "rev-2" {
				t.Fatalf("successful rollback did not swap versions: result=%#v err=%v current=%q previous-good=%q", result, err, current.Revision, previousGood.Revision)
			}
		})
	}
}

func TestDeployerRejectsLinkedRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	source := []byte("source: local\n")
	deployer := &Deployer{
		Root:      link,
		Builder:   fakeBuilder{candidate: []byte("mixed-port: 7890\n")},
		Validator: fakeValidator{},
		Service:   &fakeService{},
		Verifier:  &sequenceVerifier{},
	}
	if _, err := deployer.Apply(context.Background(), "rev-1", sourceHash(source), source); err == nil {
		t.Fatal("linked deployment root was accepted")
	}
}

func TestDeployerRejectsTamperedCurrentConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "configs")
	deployer := &Deployer{
		Root:      root,
		Builder:   fakeBuilder{candidate: []byte("mixed-port: 7890\n")},
		Validator: fakeValidator{},
		Service:   &fakeService{},
		Verifier:  &sequenceVerifier{},
	}
	firstSource := []byte("source: first\n")
	if _, err := deployer.Apply(context.Background(), "rev-1", sourceHash(firstSource), firstSource); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "current", "config.yaml"), []byte("mixed-port: 7891\n"), 0600); err != nil {
		t.Fatal(err)
	}
	secondSource := []byte("source: second\n")
	if _, err := deployer.Apply(context.Background(), "rev-2", sourceHash(secondSource), secondSource); err == nil {
		t.Fatal("tampered current configuration was replaced")
	}
}

type serialBuilder struct {
	active atomic.Int32
	max    atomic.Int32
}

func (b *serialBuilder) BuildCandidate([]byte) ([]byte, error) {
	active := b.active.Add(1)
	for {
		currentMax := b.max.Load()
		if active <= currentMax || b.max.CompareAndSwap(currentMax, active) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	b.active.Add(-1)
	return []byte("mixed-port: 7890\n"), nil
}

func TestDeployerSerializesConcurrentChanges(t *testing.T) {
	builder := &serialBuilder{}
	deployer := &Deployer{
		Root:      filepath.Join(t.TempDir(), "configs"),
		Builder:   builder,
		Validator: fakeValidator{},
		Service:   &fakeService{},
		Verifier:  &sequenceVerifier{},
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	for index := 0; index < 2; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			source := []byte{byte('a' + index)}
			_, err := deployer.Apply(context.Background(), "rev-"+string(rune('1'+index)), sourceHash(source), source)
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if builder.max.Load() != 1 {
		t.Fatalf("concurrent candidate builders = %d", builder.max.Load())
	}
}
