package runtimeupdate

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimecore"
)

type acceptingCoreVerifier struct{}

func (acceptingCoreVerifier) VerifyBinary(context.Context, string, string) error {
	return nil
}

type stoppedActivation struct{}

func (stoppedActivation) IsRunning(context.Context) (bool, error) { return false, nil }
func (stoppedActivation) Stop(context.Context) error              { return nil }
func (stoppedActivation) Start(context.Context) error             { return nil }

type acceptingCandidateVerifier struct {
	calls int
}

func (v *acceptingCandidateVerifier) VerifyCandidateCore(
	_ context.Context,
	path string,
	_ string,
) error {
	v.calls++
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("candidate core path is unsafe")
	}
	return nil
}

type fakeOfficialSource struct {
	releases map[string]runtimecore.ReleaseBinary
}

func (s fakeOfficialSource) FetchStable(
	_ context.Context,
	version string,
	_ string,
	_ string,
) (runtimecore.ReleaseBinary, error) {
	release, ok := s.releases[version]
	if !ok {
		return runtimecore.ReleaseBinary{}, errors.New("release not found")
	}
	return release, nil
}

func TestManagerOfflineInstallUpstreamConfirmationAndRollback(t *testing.T) {
	now := time.Now().UTC()
	firstArchive := gzipBody(t, []byte("first-mihomo-binary"))
	repository := newTestRepository(t, now.Add(24*time.Hour), 1)
	targetPath := addTestTarget(t, repository, firstArchive, "v1.2.3")
	root, entries := repository.signedEntries(t)
	bundlePath := writeBundle(t, entries, targetPath, firstArchive)
	bundleBody, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	bundleSum := sha256.Sum256(bundleBody)

	core := &runtimecore.Store{
		Root:       filepath.Join(t.TempDir(), "core"),
		Verifier:   acceptingCoreVerifier{},
		Activation: stoppedActivation{},
	}
	candidateVerifier := &acceptingCandidateVerifier{}
	secondArchive := gzipBody(t, []byte("second-mihomo-binary"))
	secondSum := sha256.Sum256(secondArchive)
	secondRelease, err := runtimecore.VerifyOfficialArchive(
		"v1.2.4",
		"linux",
		"amd64",
		"mihomo-linux-amd64-v1.2.4.gz",
		hex.EncodeToString(secondSum[:]),
		secondArchive,
	)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		Root:     filepath.Join(t.TempDir(), "updates"),
		Platform: "linux",
		Arch:     "amd64",
		Trust: &Verifier{
			InitialRoot: root,
			StateRoot:   filepath.Join(t.TempDir(), "trust"),
			Now:         func() time.Time { return now },
		},
		Official:          fakeOfficialSource{releases: map[string]runtimecore.ReleaseBinary{"v1.2.4": secondRelease}},
		Core:              core,
		CandidateVerifier: candidateVerifier,
		Now:               func() time.Time { return now },
	}
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	bundle, err := manager.UploadBundle(
		t.Context(),
		peer,
		int64(len(bundleBody)),
		hex.EncodeToString(bundleSum[:]),
		bytes.NewReader(bundleBody),
	)
	if err != nil {
		t.Fatalf("upload offline bundle: %v", err)
	}
	offlinePlan, err := manager.Preview(t.Context(), peer, runtimeapi.MihomoUpdatePreviewRequest{
		Source:   runtimeapi.MihomoUpdateSourceOfflineTUF,
		BundleID: bundle.ID,
	})
	if err != nil {
		t.Fatalf("preview offline update: %v", err)
	}
	if offlinePlan.Trust != runtimeapi.MihomoUpdateTrustTUF ||
		offlinePlan.Version != "v1.2.3" ||
		!offlinePlan.StaticConfigVerified {
		t.Fatalf("unexpected offline plan: %#v", offlinePlan)
	}
	if _, err := manager.Activate(t.Context(), runtimeapi.Operation{
		CallerIdentity: peer.Key(),
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionUpdateMihomo,
			Params: runtimeapi.ActionParams{
				PlanID: offlinePlan.PlanID,
				Trust:  offlinePlan.Trust,
			},
		},
	}, discardReporter); err == nil {
		t.Fatal("Mihomo update succeeded without explicit confirmation")
	}
	// The failed confirmation check does not consume the verified plan.
	firstResult, err := manager.Activate(t.Context(), runtimeapi.Operation{
		CallerIdentity: peer.Key(),
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionUpdateMihomo,
			Params: runtimeapi.ActionParams{
				PlanID:  offlinePlan.PlanID,
				Trust:   offlinePlan.Trust,
				Confirm: true,
			},
		},
	}, discardReporter)
	if err != nil || firstResult.CoreVersion != "v1.2.3" {
		t.Fatalf("activate offline update: result=%#v err=%v", firstResult, err)
	}

	upstreamPlan, err := manager.Preview(t.Context(), peer, runtimeapi.MihomoUpdatePreviewRequest{
		Source:  runtimeapi.MihomoUpdateSourceUpstreamOnly,
		Version: "v1.2.4",
	})
	if err != nil {
		t.Fatalf("preview upstream-only update: %v", err)
	}
	if upstreamPlan.Trust != runtimeapi.MihomoUpdateTrustUpstreamOnly || upstreamPlan.Warning == "" {
		t.Fatalf("upstream-only plan did not retain risk state: %#v", upstreamPlan)
	}
	secondResult, err := manager.Activate(t.Context(), runtimeapi.Operation{
		CallerIdentity: peer.Key(),
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionUpdateMihomo,
			Params: runtimeapi.ActionParams{
				PlanID:  upstreamPlan.PlanID,
				Trust:   upstreamPlan.Trust,
				Confirm: true,
			},
		},
	}, discardReporter)
	if err != nil || secondResult.CoreVersion != "v1.2.4" ||
		secondResult.Trust != runtimeapi.MihomoUpdateTrustUpstreamOnly {
		t.Fatalf("activate upstream-only update: result=%#v err=%v", secondResult, err)
	}
	status, err := core.Status()
	if err != nil || status.PreviousVersion != "v1.2.3" {
		t.Fatalf("core did not retain previous version: %#v err=%v", status, err)
	}

	rollback, err := manager.Rollback(t.Context(), runtimeapi.Operation{
		CallerIdentity: peer.Key(),
		Action: runtimeapi.Action{
			Kind:   runtimeapi.ActionRollbackMihomo,
			Params: runtimeapi.ActionParams{Confirm: true},
		},
	}, discardReporter)
	if err != nil || rollback.CoreVersion != "v1.2.3" {
		t.Fatalf("rollback core: result=%#v err=%v", rollback, err)
	}
	if candidateVerifier.calls < 4 {
		t.Fatalf("candidate core verification calls = %d, want at least 4", candidateVerifier.calls)
	}
}

func TestManagerPlanIsBoundToCallerAndTrust(t *testing.T) {
	releaseArchive := gzipBody(t, []byte("mihomo-binary"))
	sum := sha256.Sum256(releaseArchive)
	release, err := runtimecore.VerifyOfficialArchive(
		"v1.2.3",
		"linux",
		"amd64",
		"mihomo-linux-amd64-v1.2.3.gz",
		hex.EncodeToString(sum[:]),
		releaseArchive,
	)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		Root:              filepath.Join(t.TempDir(), "updates"),
		Platform:          "linux",
		Arch:              "amd64",
		Official:          fakeOfficialSource{releases: map[string]runtimecore.ReleaseBinary{"v1.2.3": release}},
		Core:              &runtimecore.Store{Root: filepath.Join(t.TempDir(), "core"), Verifier: acceptingCoreVerifier{}, Activation: stoppedActivation{}},
		CandidateVerifier: &acceptingCandidateVerifier{},
	}
	owner := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	plan, err := manager.Preview(t.Context(), owner, runtimeapi.MihomoUpdatePreviewRequest{
		Source:  runtimeapi.MihomoUpdateSourceUpstreamOnly,
		Version: "v1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []runtimeapi.Operation{
		{
			CallerIdentity: runtimeapi.PeerIdentity{Platform: "linux", UID: 1001}.Key(),
			Action: runtimeapi.Action{Kind: runtimeapi.ActionUpdateMihomo, Params: runtimeapi.ActionParams{
				PlanID: plan.PlanID, Trust: plan.Trust, Confirm: true,
			}},
		},
		{
			CallerIdentity: owner.Key(),
			Action: runtimeapi.Action{Kind: runtimeapi.ActionUpdateMihomo, Params: runtimeapi.ActionParams{
				PlanID: plan.PlanID, Trust: runtimeapi.MihomoUpdateTrustTUF, Confirm: true,
			}},
		},
	} {
		if _, err := manager.Activate(t.Context(), operation, discardReporter); err == nil {
			t.Fatal("plan accepted a different caller or trust mode")
		}
	}
}

func gzipBody(t *testing.T, body []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func discardReporter(string, int, bool) error { return nil }
