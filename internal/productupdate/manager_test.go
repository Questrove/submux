package productupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
	"submux/internal/runtimeupdate"
)

type updateRecorder struct {
	steps       []string
	replaceErr  error
	current     string
	previous    string
	rollbackErr error
	stopErr     error
}

type fakeInstaller struct {
	recorder *updateRecorder
}

func (installer *fakeInstaller) Status(context.Context) (InstallStatus, error) {
	return InstallStatus{
		Available:       true,
		CurrentVersion:  installer.recorder.current,
		PreviousVersion: installer.recorder.previous,
	}, nil
}

func (installer *fakeInstaller) PrepareRollback(
	_ context.Context,
	plan runtimeapi.ProductUpdatePlan,
	_ Package,
	database runtimestate.DatabaseBackup,
) (RollbackPoint, error) {
	installer.recorder.steps = append(installer.recorder.steps, "prepare")
	if database.Size <= 0 || database.SHA256 == "" {
		return RollbackPoint{}, errors.New("database rollback point is unavailable")
	}
	return RollbackPoint{
		ID:       "rollback-test",
		Version:  plan.CurrentVersion,
		Database: database,
	}, nil
}

func (installer *fakeInstaller) DiscardRollback(context.Context, RollbackPoint) error {
	installer.recorder.steps = append(installer.recorder.steps, "discard-rollback")
	return nil
}

func (installer *fakeInstaller) Replace(
	_ context.Context,
	plan runtimeapi.ProductUpdatePlan,
	_ Package,
	_ RollbackPoint,
) error {
	installer.recorder.steps = append(installer.recorder.steps, "replace")
	if installer.recorder.replaceErr != nil {
		return installer.recorder.replaceErr
	}
	installer.recorder.previous = installer.recorder.current
	installer.recorder.current = plan.Version
	return nil
}

func (installer *fakeInstaller) Verify(
	_ context.Context,
	plan runtimeapi.ProductUpdatePlan,
	_ RollbackPoint,
) error {
	installer.recorder.steps = append(installer.recorder.steps, "installer-verify")
	if installer.recorder.current != plan.Version {
		return errors.New("installed version did not change")
	}
	return nil
}

func (installer *fakeInstaller) Rollback(context.Context, RollbackPoint) error {
	installer.recorder.steps = append(installer.recorder.steps, "rollback")
	if installer.recorder.rollbackErr != nil {
		return installer.recorder.rollbackErr
	}
	if installer.recorder.previous != "" {
		installer.recorder.current = installer.recorder.previous
		installer.recorder.previous = ""
	}
	return nil
}

type fakeSafety struct {
	recorder *updateRecorder
}

func (safety *fakeSafety) CaptureExpectedState(context.Context) (ExpectedRuntimeState, error) {
	safety.recorder.steps = append(safety.recorder.steps, "capture")
	return ExpectedRuntimeState{
		MihomoRunning: true,
		RunMode:       runtimeapi.RunModeTUN,
		Network: runtimeapi.NetworkStatus{
			Mode:  runtimeapi.RunModeTUN,
			State: runtimeapi.NetworkStateActive,
		},
	}, nil
}

func (safety *fakeSafety) StopForProductUpdate(
	context.Context,
	runtimeapi.Operation,
	Reporter,
) error {
	safety.recorder.steps = append(safety.recorder.steps, "stop-fail-open")
	return safety.recorder.stopErr
}

func (safety *fakeSafety) VerifyProductUpdateHealth(
	context.Context,
	runtimeapi.ProductUpdatePlan,
	Reporter,
) error {
	safety.recorder.steps = append(safety.recorder.steps, "runtime-health")
	return nil
}

func (safety *fakeSafety) RestoreExpectedState(
	context.Context,
	runtimeapi.Operation,
	ExpectedRuntimeState,
	Reporter,
) error {
	safety.recorder.steps = append(safety.recorder.steps, "restore-expected")
	return nil
}

func TestProductUpdateActivationUsesFixedRollbackSequence(t *testing.T) {
	manager, operation, recorder := stagedProductUpdate(t)
	result, err := manager.Activate(
		t.Context(),
		operation,
		&fakeSafety{recorder: recorder},
		func(string, int, bool) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified ||
		result.RuntimeVersion != "v2.0.0" ||
		result.PreviousRuntimeVersion != "v1.0.0" ||
		result.ProductRollback != "available" {
		t.Fatalf("product update result = %#v", result)
	}
	want := []string{
		"capture",
		"prepare",
		"stop-fail-open",
		"replace",
		"installer-verify",
		"runtime-health",
		"restore-expected",
	}
	if strings.Join(recorder.steps, ",") != strings.Join(want, ",") {
		t.Fatalf("product update steps = %v, want %v", recorder.steps, want)
	}
}

func TestProductUpdateFailureRollsBackProgramsDatabaseAndExpectedState(t *testing.T) {
	manager, operation, recorder := stagedProductUpdate(t)
	recorder.replaceErr = errors.New("injected platform replacement failure")
	_, err := manager.Activate(
		t.Context(),
		operation,
		&fakeSafety{recorder: recorder},
		func(string, int, bool) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "old programs, database, and expected running state were restored") {
		t.Fatalf("failed product update error = %v", err)
	}
	if recorder.current != "v1.0.0" {
		t.Fatalf("failed product update current version = %s", recorder.current)
	}
	wantSuffix := "replace,rollback,restore-expected"
	if !strings.HasSuffix(strings.Join(recorder.steps, ","), wantSuffix) {
		t.Fatalf("failed product update steps = %v", recorder.steps)
	}
}

func TestIncompleteProductUpdateIsRolledBackOnStartup(t *testing.T) {
	manager, operation, recorder := stagedProductUpdate(t)
	recorder.current = "v2.0.0"
	recorder.previous = "v1.0.0"
	transaction := transactionRecord{
		Operation: operation,
		Plan: runtimeapi.ProductUpdatePlan{
			Version: "v2.0.0",
		},
		Point: RollbackPoint{
			ID:      "rollback-test",
			Version: "v1.0.0",
		},
		Expected: ExpectedRuntimeState{
			MihomoRunning: true,
			RunMode:       runtimeapi.RunModeTUN,
		},
		Phase: "prepared",
	}
	if err := manager.persistTransaction(transaction); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecoverIncomplete(
		t.Context(),
		&fakeSafety{recorder: recorder},
		func(string, int, bool) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if recorder.current != "v1.0.0" {
		t.Fatalf("recovered Runtime version = %s", recorder.current)
	}
	if got := strings.Join(recorder.steps, ","); got != "stop-fail-open,rollback,restore-expected" {
		t.Fatalf("recovery steps = %s", got)
	}
	if _, err := os.Stat(filepath.Join(manager.Root, "transaction.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery transaction still exists: %v", err)
	}
}

func TestIncompleteProductUpdateDoesNotRollbackUntilFailOpenStopSucceeds(t *testing.T) {
	manager, operation, recorder := stagedProductUpdate(t)
	recorder.current = "v2.0.0"
	recorder.previous = "v1.0.0"
	recorder.stopErr = errors.New("injected fail-open stop failure")
	transaction := transactionRecord{
		Operation: operation,
		Plan:      runtimeapi.ProductUpdatePlan{Version: "v2.0.0"},
		Point: RollbackPoint{
			ID:      "rollback-test",
			Version: "v1.0.0",
		},
		Phase: "prepared",
	}
	if err := manager.persistTransaction(transaction); err != nil {
		t.Fatal(err)
	}
	err := manager.RecoverIncomplete(
		t.Context(),
		&fakeSafety{recorder: recorder},
		func(string, int, bool) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "fail-open recovery") {
		t.Fatalf("recovery stop failure = %v", err)
	}
	if recorder.current != "v2.0.0" || strings.Contains(strings.Join(recorder.steps, ","), "rollback") {
		t.Fatalf("unsafe recovery steps=%v current=%s", recorder.steps, recorder.current)
	}
	if _, err := os.Stat(filepath.Join(manager.Root, "transaction.json")); err != nil {
		t.Fatalf("recovery transaction was not retained: %v", err)
	}
}

func TestProductUpdateRejectsOperationIDPathEscapeBeforeCreatingRollback(t *testing.T) {
	manager, operation, recorder := stagedProductUpdate(t)
	operation.ID = "../../outside"
	_, err := manager.Activate(
		t.Context(),
		operation,
		&fakeSafety{recorder: recorder},
		func(string, int, bool) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "Operation ID is invalid") {
		t.Fatalf("path-escaping Operation ID error = %v", err)
	}
	if len(recorder.steps) != 0 {
		t.Fatalf("path-escaping Operation ID reached installer: %v", recorder.steps)
	}
}

func TestProductPackageRejectsUnlistedContent(t *testing.T) {
	target, body := buildProductPackage(t, true)
	digest := sha256.Sum256(body)
	target.Length = int64(len(body))
	target.SHA256 = hex.EncodeToString(digest[:])
	target.RequiredFreeBytes = int64(len(body)) * 4
	if _, err := ParsePackage(body, target); err == nil ||
		!strings.Contains(err.Error(), "unlisted") {
		t.Fatalf("unlisted Runtime product package error = %v", err)
	}
}

func stagedProductUpdate(
	t *testing.T,
) (*Manager, runtimeapi.Operation, *updateRecorder) {
	t.Helper()
	target, body := buildProductPackage(t, false)
	digest := sha256.Sum256(body)
	target.Length = int64(len(body))
	target.SHA256 = hex.EncodeToString(digest[:])
	target.RequiredFreeBytes = int64(len(body)) * 4
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	recorder := &updateRecorder{current: "v1.0.0"}
	manager := &Manager{
		Root:           filepath.Join(t.TempDir(), "product-updates"),
		Platform:       runtime.GOOS,
		Arch:           runtime.GOARCH,
		CurrentVersion: "v1.0.0",
		InstallationID: "installation-test",
		Trust:          &runtimeupdate.Verifier{},
		State:          state,
		Installer:      &fakeInstaller{recorder: recorder},
		Now: func() time.Time {
			return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		},
	}
	if err := manager.prepare(); err != nil {
		t.Fatal(err)
	}
	planID := "product_plan_" + strings.Repeat("a", 32)
	plan := runtimeapi.ProductUpdatePlan{
		PlanID:            planID,
		Source:            runtimeapi.ProductUpdateSourceOfflineTUF,
		Trust:             runtimeapi.ProductUpdateTrustTUF,
		Channel:           runtimeapi.ProductUpdateChannelStable,
		Version:           target.Version,
		CurrentVersion:    "v1.0.0",
		Platform:          target.Platform,
		Arch:              target.Arch,
		AssetName:         target.AssetName,
		AssetSize:         target.Length,
		AssetSHA256:       target.SHA256,
		ReleaseNotes:      target.ReleaseNotes,
		Components:        append([]string(nil), target.Components...),
		RequiredFreeBytes: target.RequiredFreeBytes,
		ExpiresAt:         manager.now().Add(time.Hour),
		Migration: runtimeapi.ProductUpdateMigration{
			CurrentSchema:   runtimestate.SchemaVersion,
			TargetSchema:    runtimestate.SchemaVersion,
			Reversible:      true,
			ProtocolMin:     runtimeapi.ProtocolVersion,
			ProtocolMax:     runtimeapi.ProtocolVersion,
			CurrentProtocol: runtimeapi.ProtocolVersion,
		},
	}
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	planDir := filepath.Join(manager.Root, "plans", planID)
	if err := preparePrivateDir(planDir); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(filepath.Join(planDir, "package.zip"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(planDir, "plan.json"), planRecord{
		Plan:   plan,
		Target: target,
		Owner:  peer.Key(),
	}); err != nil {
		t.Fatal(err)
	}
	return manager, runtimeapi.Operation{
		ID:             "op_" + strings.Repeat("b", 32),
		CallerIdentity: peer.Key(),
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionUpdateProduct,
			Params: runtimeapi.ActionParams{
				PlanID:  planID,
				Trust:   runtimeapi.ProductUpdateTrustTUF,
				Confirm: true,
			},
		},
	}, recorder
}

func buildProductPackage(
	t *testing.T,
	extra bool,
) (runtimeupdate.ProductTarget, []byte) {
	t.Helper()
	components := []string{
		"submux-runtime",
		"submux-runtime-net",
		"submux-runtime-gui",
	}
	payloads := map[string][]byte{
		"submux-runtime":     []byte("runtime-binary"),
		"submux-runtime-net": []byte("runtime-net-binary"),
		"submux-runtime-gui": []byte("runtime-gui-package"),
	}
	manifest := PackageManifest{
		FormatVersion:       PackageFormatVersion,
		Version:             "v2.0.0",
		Platform:            runtime.GOOS,
		Arch:                runtime.GOARCH,
		RuntimeSchemaTarget: runtimestate.SchemaVersion,
		ProtocolVersion:     runtimeapi.ProtocolVersion,
	}
	for _, name := range components {
		sum := sha256.Sum256(payloads[name])
		manifest.Components = append(manifest.Components, PackageComponent{
			Name:   name,
			Entry:  "payload/" + name,
			Size:   int64(len(payloads[name])),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	write := func(name string, body []byte) {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	write("product-manifest.json", manifestBody)
	for _, name := range components {
		write("payload/"+name, payloads[name])
	}
	if extra {
		write("payload/secret-extra", []byte("unlisted"))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	target := runtimeupdate.ProductTarget{
		Path:                "product/v2.0.0/" + runtime.GOOS + "/" + runtime.GOARCH + "/submux-runtime-v2.0.0.zip",
		Version:             "v2.0.0",
		Platform:            runtime.GOOS,
		Arch:                runtime.GOARCH,
		Channel:             runtimeupdate.ProductChannelStable,
		AssetName:           "submux-runtime-v2.0.0.zip",
		ReleaseNotes:        "Stable product update.",
		MigrationSummary:    "No migration required.",
		RuntimeSchemaMin:    runtimestate.SchemaVersion,
		RuntimeSchemaMax:    runtimestate.SchemaVersion,
		RuntimeSchemaTarget: runtimestate.SchemaVersion,
		ProtocolMin:         runtimeapi.ProtocolVersion,
		ProtocolMax:         runtimeapi.ProtocolVersion,
		Components:          components,
	}
	return target, output.Bytes()
}
