package runtimeapp

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimestate"
)

type scheduledRecoveryTimer struct {
	delay time.Duration
	fire  chan time.Time
}

type recoveryTestClock struct {
	requests chan scheduledRecoveryTimer
}

func newRecoveryTestClock() *recoveryTestClock {
	return &recoveryTestClock{requests: make(chan scheduledRecoveryTimer, 16)}
}

func (clock *recoveryTestClock) After(delay time.Duration) <-chan time.Time {
	timer := scheduledRecoveryTimer{delay: delay, fire: make(chan time.Time, 1)}
	clock.requests <- timer
	return timer.fire
}

type fakeRecoveryTarget struct {
	exits chan runtimeprocess.ExitEvent

	mu             sync.Mutex
	restoreResults []error
	restoreCalls   chan struct{}
	failOpenCalls  chan struct{}
	failOpenErr    error
	reconcileCalls chan struct{}
	reconcileErr   error
}

func newFakeRecoveryTarget(results ...error) *fakeRecoveryTarget {
	return &fakeRecoveryTarget{
		exits:          make(chan runtimeprocess.ExitEvent, 16),
		restoreResults: append([]error(nil), results...),
		restoreCalls:   make(chan struct{}, 16),
		failOpenCalls:  make(chan struct{}, 16),
		reconcileCalls: make(chan struct{}, 16),
	}
}

func (target *fakeRecoveryTarget) ReconcileCoreVersion(context.Context) error {
	target.reconcileCalls <- struct{}{}
	return target.reconcileErr
}

func (target *fakeRecoveryTarget) RestoreLastGood(context.Context) error {
	target.restoreCalls <- struct{}{}
	target.mu.Lock()
	defer target.mu.Unlock()
	if len(target.restoreResults) == 0 {
		return nil
	}
	result := target.restoreResults[0]
	target.restoreResults = target.restoreResults[1:]
	return result
}

func (target *fakeRecoveryTarget) FailOpen(context.Context) error {
	target.failOpenCalls <- struct{}{}
	return target.failOpenErr
}

func (target *fakeRecoveryTarget) ExitEvents() <-chan runtimeprocess.ExitEvent {
	return target.exits
}

func TestMihomoSupervisorUsesBoundedBackoffAndStopsAfterThirdRetry(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	seedDesiredRunning(t, state)
	target := newFakeRecoveryTarget(
		errors.New("restart one failed"),
		errors.New("restart two failed"),
		errors.New("restart three failed"),
	)
	clock := newRecoveryTestClock()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	supervisor := &MihomoSupervisor{
		State:  state,
		Target: target,
		Now:    func() time.Time { return now },
		After:  clock.After,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()

	target.exits <- runtimeprocess.ExitEvent{ExitedAt: now, Err: errors.New("crashed")}
	for index, wantDelay := range runtimestate.MihomoRestartBackoff {
		timer := waitRecoveryTimer(t, clock)
		if timer.delay != wantDelay {
			t.Fatalf("restart timer %d=%s, want %s", index+1, timer.delay, wantDelay)
		}
		timer.fire <- now.Add(wantDelay)
		waitRecoveryCall(t, target.restoreCalls, "restore last-good configuration")
	}
	waitMihomoRecovery(t, state, runtimeapi.MihomoRecoveryNeedsAttention)
	snapshot, err := state.Observe("test", now)
	if err != nil {
		t.Fatalf("observe limited Mihomo recovery: %v", err)
	}
	if snapshot.Mihomo.CrashAttempts != 3 ||
		snapshot.Mihomo.State != "stopped" ||
		snapshot.Mihomo.Fault == nil {
		t.Fatalf("limited Mihomo recovery=%#v", snapshot.Mihomo)
	}
	select {
	case timer := <-clock.requests:
		t.Fatalf("unexpected fourth Mihomo restart timer=%s", timer.delay)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Mihomo supervisor: %v", err)
	}
}

func TestMihomoSupervisorResetsCrashWindowAfterStableRun(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	seedDesiredRunning(t, state)
	target := newFakeRecoveryTarget(nil)
	clock := newRecoveryTestClock()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	supervisor := &MihomoSupervisor{
		State:  state,
		Target: target,
		Now:    func() time.Time { return now },
		After:  clock.After,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()

	target.exits <- runtimeprocess.ExitEvent{ExitedAt: now, Err: errors.New("crashed")}
	restart := waitRecoveryTimer(t, clock)
	if restart.delay != 5*time.Second {
		t.Fatalf("first restart delay=%s", restart.delay)
	}
	restart.fire <- now.Add(restart.delay)
	waitRecoveryCall(t, target.restoreCalls, "restore last-good configuration")
	stable := waitRecoveryTimer(t, clock)
	if stable.delay != runtimestate.MihomoCrashWindow {
		t.Fatalf("stability timer=%s", stable.delay)
	}
	stable.fire <- now.Add(stable.delay)
	waitMihomoRecovery(t, state, runtimeapi.MihomoRecoveryIdle)
	snapshot, _ := state.Observe("test", now)
	if snapshot.Mihomo.CrashAttempts != 0 {
		t.Fatalf("stable Mihomo crash attempts=%d", snapshot.Mihomo.CrashAttempts)
	}

	target.exits <- runtimeprocess.ExitEvent{ExitedAt: now, Err: errors.New("crashed again")}
	afterStable := waitRecoveryTimer(t, clock)
	if afterStable.delay != 5*time.Second {
		t.Fatalf("restart delay after stable run=%s", afterStable.delay)
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Mihomo supervisor: %v", err)
	}
}

func TestMihomoSupervisorStartupFailureIsAttemptedOnlyOnce(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	seedDesiredRunning(t, state)
	target := newFakeRecoveryTarget(errors.New("startup restore failed"))
	clock := newRecoveryTestClock()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	supervisor := &MihomoSupervisor{
		State:  state,
		Target: target,
		Now:    func() time.Time { return now },
		After:  clock.After,
	}
	if err := supervisor.RecoverStartup(context.Background()); err != nil {
		t.Fatalf("recover Mihomo at Runtime startup: %v", err)
	}
	waitRecoveryCall(t, target.reconcileCalls, "reconcile core before startup restore")
	waitRecoveryCall(t, target.failOpenCalls, "fail open before startup restore")
	waitRecoveryCall(t, target.restoreCalls, "attempt startup restore")
	snapshot, err := state.Observe("test", now)
	if err != nil {
		t.Fatalf("observe failed startup recovery: %v", err)
	}
	if snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryNeedsAttention {
		t.Fatalf("failed startup recovery=%#v", snapshot.Mihomo)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	target.exits <- runtimeprocess.ExitEvent{ExitedAt: now, Err: errors.New("startup process exited")}
	waitRecoveryCall(t, target.failOpenCalls, "fail open after queued startup exit")
	select {
	case <-target.restoreCalls:
		t.Fatal("startup recovery failure triggered another restore")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case timer := <-clock.requests:
		t.Fatalf("startup recovery failure scheduled timer=%s", timer.delay)
	default:
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Mihomo supervisor: %v", err)
	}
}

func TestMihomoSupervisorCoreReconciliationFailureDoesNotLoop(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	seedDesiredRunning(t, state)
	target := newFakeRecoveryTarget()
	target.reconcileErr = errors.New("installed core is invalid")
	supervisor := &MihomoSupervisor{State: state, Target: target}

	if err := supervisor.RecoverStartup(context.Background()); err != nil {
		t.Fatalf("recover Runtime with invalid core: %v", err)
	}
	waitRecoveryCall(t, target.reconcileCalls, "inspect installed core")
	select {
	case <-target.restoreCalls:
		t.Fatal("invalid core triggered startup restore")
	default:
	}
	snapshot, err := state.Observe("test", time.Now())
	if err != nil {
		t.Fatalf("observe invalid core recovery: %v", err)
	}
	if snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryNeedsAttention ||
		snapshot.Mihomo.Fault == nil ||
		snapshot.Mihomo.Fault.Code != "mihomo_core_unavailable" {
		t.Fatalf("invalid core recovery=%#v", snapshot.Mihomo)
	}
}

func TestMihomoSupervisorIgnoresIntentionalProcessExit(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	seedDesiredRunning(t, state)
	target := newFakeRecoveryTarget()
	clock := newRecoveryTestClock()
	supervisor := &MihomoSupervisor{State: state, Target: target, After: clock.After}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	target.exits <- runtimeprocess.ExitEvent{Intentional: true}
	select {
	case timer := <-clock.requests:
		t.Fatalf("intentional exit scheduled recovery timer=%s", timer.delay)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Mihomo supervisor: %v", err)
	}
}

func TestMihomoSupervisorReportsUnknownWhenFailOpenCannotBeConfirmed(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	seedDesiredRunning(t, state)
	target := newFakeRecoveryTarget()
	target.failOpenErr = errors.New("network cleanup failed")
	clock := newRecoveryTestClock()
	supervisor := &MihomoSupervisor{State: state, Target: target, After: clock.After}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	target.exits <- runtimeprocess.ExitEvent{Err: errors.New("crashed")}
	waitRecoveryCall(t, target.failOpenCalls, "attempt fail-open cleanup")
	waitMihomoRecovery(t, state, runtimeapi.MihomoRecoveryFailOpenUnknown)
	select {
	case timer := <-clock.requests:
		t.Fatalf("unknown fail-open state scheduled restart=%s", timer.delay)
	default:
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Mihomo supervisor: %v", err)
	}
}

func TestMihomoSupervisorDoesNotRetryAfterExplicitStop(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	seedDesiredRunning(t, state)
	target := newFakeRecoveryTarget()
	clock := newRecoveryTestClock()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	supervisor := &MihomoSupervisor{
		State:  state,
		Target: target,
		Now:    func() time.Time { return now },
		After:  clock.After,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	target.exits <- runtimeprocess.ExitEvent{Err: errors.New("crashed")}
	retry := waitRecoveryTimer(t, clock)
	completeDesiredOperation(t, state, runtimeapi.ActionStopProxy, "stop-during-recovery")
	retry.fire <- now.Add(retry.delay)
	select {
	case <-target.restoreCalls:
		t.Fatal("explicit stop did not cancel scheduled Mihomo restart")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Mihomo supervisor: %v", err)
	}
}

func seedDesiredRunning(t *testing.T, state *runtimestate.Store) {
	t.Helper()
	completeDesiredOperation(t, state, runtimeapi.ActionStartProxy, "seed-desired-running")
}

func completeDesiredOperation(t *testing.T, state *runtimestate.Store, action, requestID string) {
	t.Helper()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	snapshot, err := state.Observe("test", time.Now())
	if err != nil {
		t.Fatalf("observe Runtime before desired-state seed: %v", err)
	}
	operation, _, err := state.SubmitOperation(peer, "test", runtimeapi.CreateOperationRequest{
		RequestID:  requestID,
		IfRevision: snapshot.Revision,
		Action:     runtimeapi.Action{Kind: action},
	}, 4, time.Now())
	if err != nil {
		t.Fatalf("submit desired-state operation %q: %v", action, err)
	}
	if _, found, err := state.BeginNextOperation(time.Now()); err != nil || !found {
		t.Fatalf("begin desired-state operation %q: found=%v err=%v", action, found, err)
	}
	if err := state.CompleteOperation(
		operation.ID,
		runtimeapi.OperationSucceeded,
		&runtimeapi.OperationResult{Verified: true},
		nil,
		time.Now(),
	); err != nil {
		t.Fatalf("complete desired-state operation %q: %v", action, err)
	}
}

func waitRecoveryTimer(t *testing.T, clock *recoveryTestClock) scheduledRecoveryTimer {
	t.Helper()
	select {
	case timer := <-clock.requests:
		return timer
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Mihomo recovery timer")
		return scheduledRecoveryTimer{}
	}
}

func waitRecoveryCall(t *testing.T, calls <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting to %s", description)
	}
}

func waitMihomoRecovery(t *testing.T, state *runtimestate.Store, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := state.Observe("test", time.Now())
		if err != nil {
			t.Fatalf("observe Mihomo recovery: %v", err)
		}
		if snapshot.Mihomo.Recovery == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for Mihomo recovery %q", want)
}
