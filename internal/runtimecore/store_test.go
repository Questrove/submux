package runtimecore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBinaryVerifier struct {
	wantVersion string
	err         error
	calls       int
}

func (v *fakeBinaryVerifier) VerifyBinary(_ context.Context, path, exactVersion string) error {
	v.calls++
	if v.err != nil {
		return v.err
	}
	if v.wantVersion != "" && exactVersion != v.wantVersion {
		return errors.New("unexpected version")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("binary is not a regular file")
	}
	return nil
}

type fakeActivation struct {
	running    bool
	starts     int
	stops      int
	failStarts int
}

func (a *fakeActivation) IsRunning(context.Context) (bool, error) {
	return a.running, nil
}

func (a *fakeActivation) Stop(context.Context) error {
	a.stops++
	a.running = false
	return nil
}

func (a *fakeActivation) Start(context.Context) error {
	a.starts++
	if a.failStarts > 0 {
		a.failStarts--
		return errors.New("start failed")
	}
	a.running = true
	return nil
}

func testBinary(version, value string) Binary {
	data := []byte(value)
	digest := sha256.Sum256(data)
	return Binary{
		Version:      version,
		BinaryDigest: hex.EncodeToString(digest[:]),
		Data:         data,
	}
}

func TestStoreInstallsRetainsPreviousAndRollsBack(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cores")
	activation := &fakeActivation{}
	store := &Store{
		Root:       root,
		Verifier:   &fakeBinaryVerifier{},
		Activation: activation,
	}
	if err := store.Activate(context.Background(), testBinary("v1.19.28", "first")); err != nil {
		t.Fatal(err)
	}
	activation.running = true
	if err := store.Activate(context.Background(), testBinary("v1.19.29", "second")); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.Version != "v1.19.29" || status.PreviousVersion != "v1.19.28" {
		t.Fatalf("status after update = %#v", status)
	}
	if activation.stops != 1 || activation.starts != 1 || !activation.running {
		t.Fatalf("activation after update = %#v", activation)
	}
	if err := store.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "v1.19.28" || status.PreviousVersion != "v1.19.29" {
		t.Fatalf("status after rollback = %#v", status)
	}
	if activation.stops != 2 || activation.starts != 2 || !activation.running {
		t.Fatalf("activation after rollback = %#v", activation)
	}
}

func TestStoreRejectsDigestMismatchBeforeReplacingCurrent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cores")
	store := &Store{Root: root, Verifier: &fakeBinaryVerifier{}}
	if err := store.Activate(context.Background(), testBinary("v1.19.28", "first")); err != nil {
		t.Fatal(err)
	}
	invalid := testBinary("v1.19.29", "second")
	invalid.BinaryDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := store.Activate(context.Background(), invalid); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "v1.19.28" || status.PreviousVersion != "" {
		t.Fatalf("digest failure changed core state: %#v", status)
	}
}

func TestStoreRestoresPreviousWhenNewRunningCoreFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cores")
	activation := &fakeActivation{}
	store := &Store{Root: root, Verifier: &fakeBinaryVerifier{}, Activation: activation}
	if err := store.Activate(context.Background(), testBinary("v1.19.28", "first")); err != nil {
		t.Fatal(err)
	}
	activation.running = true
	activation.failStarts = 1
	if err := store.Activate(context.Background(), testBinary("v1.19.29", "second")); err == nil {
		t.Fatal("failed replacement start was accepted")
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "v1.19.28" || status.PreviousVersion != "v1.19.29" || !activation.running {
		t.Fatalf("failed replacement did not restore the original core: status=%#v activation=%#v", status, activation)
	}
}

func TestStoreRecoversInterruptedRollback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cores")
	store := &Store{Root: root, Verifier: &fakeBinaryVerifier{}}
	if err := store.Activate(context.Background(), testBinary("v1.19.28", "first")); err != nil {
		t.Fatal(err)
	}
	if err := store.Activate(context.Background(), testBinary("v1.19.29", "second")); err != nil {
		t.Fatal(err)
	}
	if err := beginOperation(root, operation{Kind: "rollback", TargetVersion: "v1.19.28"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "current"), filepath.Join(root, "failed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "previous"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "v1.19.28" || status.PreviousVersion != "v1.19.29" {
		t.Fatalf("interrupted rollback recovery = %#v", status)
	}
	if _, exists, err := readOperation(root); err != nil || exists {
		t.Fatalf("operation marker after recovery: exists=%v err=%v", exists, err)
	}
}

func TestOperationMarkerIsStrictAndAtomic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cores")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := beginOperation(root, operation{Kind: "install", TargetVersion: "v1.19.29", WasRunning: true}); err != nil {
		t.Fatal(err)
	}
	value, exists, err := readOperation(root)
	if err != nil || !exists || value.TargetVersion != "v1.19.29" || !value.WasRunning {
		t.Fatalf("operation marker = %#v, exists=%v err=%v", value, exists, err)
	}
	if err := os.WriteFile(filepath.Join(root, "core-operation.json"), []byte(`{"kind":"install","target_version":"v1.19.29","unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readOperation(root); err == nil {
		t.Fatal("operation marker with unknown fields was accepted")
	}
}

func TestStoreRejectsLinkedRootAndTamperedBinary(t *testing.T) {
	t.Run("linked-root", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "target")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symbolic links are unavailable: %v", err)
		}
		store := &Store{Root: link, Verifier: &fakeBinaryVerifier{}}
		if err := store.Activate(context.Background(), testBinary("v1.19.29", "binary")); err == nil {
			t.Fatal("linked core root was accepted")
		}
	})
	t.Run("tampered-binary", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "cores")
		store := &Store{Root: root, Verifier: &fakeBinaryVerifier{}}
		if err := store.Activate(context.Background(), testBinary("v1.19.29", "binary")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "current", binaryName()), []byte("tampered"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Status(); err == nil {
			t.Fatal("tampered managed binary was accepted")
		}
	})
}

func TestReportsExactVersion(t *testing.T) {
	if !reportsExactVersion("Mihomo Meta v1.19.29 windows amd64", "v1.19.29") {
		t.Fatal("exact version was not recognized")
	}
	for _, output := range []string{"v1.19.290", "prefixv1.19.29", "v1.19.29-alpha"} {
		if reportsExactVersion(output, "v1.19.29") {
			t.Fatalf("inexact version %q was accepted", output)
		}
	}
}

type serialBinaryVerifier struct {
	active atomic.Int32
	max    atomic.Int32
}

func (v *serialBinaryVerifier) VerifyBinary(context.Context, string, string) error {
	active := v.active.Add(1)
	for {
		currentMax := v.max.Load()
		if active <= currentMax || v.max.CompareAndSwap(currentMax, active) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	v.active.Add(-1)
	return nil
}

func TestStoreSerializesConcurrentActivations(t *testing.T) {
	verifier := &serialBinaryVerifier{}
	store := &Store{
		Root:     filepath.Join(t.TempDir(), "cores"),
		Verifier: verifier,
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	for _, binary := range []Binary{
		testBinary("v1.19.28", "first"),
		testBinary("v1.19.29", "second"),
	} {
		binary := binary
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsFound <- store.Activate(context.Background(), binary)
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if verifier.max.Load() != 1 {
		t.Fatalf("concurrent core verifiers = %d", verifier.max.Load())
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.Version == "" || status.PreviousVersion == "" || status.Version == status.PreviousVersion {
		t.Fatalf("serialized activation status = %#v", status)
	}
}
