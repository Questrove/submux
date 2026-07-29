package outputupdate

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"submux/internal/compiler"
	"submux/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func savePending(t *testing.T, st *store.Store, token string) int64 {
	t.Helper()
	id, err := st.SaveOutputSubscription(store.OutputSubscription{
		Name: token, Token: token, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAttemptPendingCompilesAndPublishesCurrentGeneration(t *testing.T) {
	st := testStore(t)
	id := savePending(t, st, "one")
	service := newWithCompiler(st, func(subscription store.OutputSubscription) (compiler.Result, error) {
		return compiler.Result{
			Body: []byte("compiled"), ContentType: "text/yaml", Revision: "r1",
			Warnings: []string{"warning"},
		}, nil
	})

	report := service.AttemptPending()
	result, ok := report.ResultFor(id)
	if !ok || result.Outcome != "completed" || report.Error != "" {
		t.Fatalf("report = %+v", report)
	}
	artifact, err := st.GetSubscriptionArtifact(id)
	if err != nil || string(artifact.Body) != "compiled" || artifact.Revision != "r1" {
		t.Fatalf("artifact = %+v err=%v", artifact, err)
	}
	update, err := st.GetSubscriptionUpdate(id)
	if err != nil || update.Status != store.SubscriptionUpdateReady || len(update.Warnings) != 1 {
		t.Fatalf("update = %+v err=%v", update, err)
	}
}

func TestConfigurationFailureKeepsLastGoodAndWaitsForRetry(t *testing.T) {
	st := testStore(t)
	id := savePending(t, st, "one")
	initial, _ := st.GetSubscriptionUpdate(id)
	if committed, err := st.CommitSubscriptionArtifactIfCurrent(id, initial.InputGeneration, store.SubscriptionArtifact{
		Body: []byte("last-good"), Revision: "old",
	}, nil); err != nil || !committed {
		t.Fatalf("seed artifact: committed=%v err=%v", committed, err)
	}
	if _, err := st.MarkOutputSubscriptionPending(id); err != nil {
		t.Fatal(err)
	}
	compileErr := errors.New("invalid binding")
	service := newWithCompiler(st, func(subscription store.OutputSubscription) (compiler.Result, error) {
		if compileErr != nil {
			return compiler.Result{}, compileErr
		}
		return compiler.Result{Body: []byte("new"), Revision: "new"}, nil
	})

	report := service.AttemptPending()
	result, _ := report.ResultFor(id)
	if result.Outcome != "degraded" {
		t.Fatalf("first report = %+v", report)
	}
	artifact, _ := st.GetSubscriptionArtifact(id)
	if string(artifact.Body) != "last-good" || artifact.Revision != "old" {
		t.Fatalf("last-good artifact changed: %+v", artifact)
	}
	update, _ := st.GetSubscriptionUpdate(id)
	if update.Status != store.SubscriptionUpdatePending || update.FailureClass != store.SubscriptionFailureConfig || update.AttemptCount != 1 {
		t.Fatalf("configuration failure state = %+v", update)
	}
	if due, err := st.ListDueSubscriptionUpdates(time.Now().Add(24 * time.Hour)); err != nil || len(due) != 0 {
		t.Fatalf("configuration failure retried automatically: due=%+v err=%v", due, err)
	}

	compileErr = nil
	retryReport, err := service.Retry(id)
	if err != nil {
		t.Fatal(err)
	}
	retryResult, _ := retryReport.ResultFor(id)
	if retryResult.Outcome != "completed" {
		t.Fatalf("retry report = %+v", retryReport)
	}
}

func TestTemporaryFailureUsesPersistentCappedBackoff(t *testing.T) {
	st := testStore(t)
	id := savePending(t, st, "one")
	attempts := 0
	service := newWithCompiler(st, func(subscription store.OutputSubscription) (compiler.Result, error) {
		attempts++
		if attempts == 1 {
			return compiler.Result{}, &TemporaryError{Err: errors.New("temporary store read")}
		}
		return compiler.Result{Body: []byte("recovered"), Revision: "r2"}, nil
	})
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	service.AttemptPending()
	update, _ := st.GetSubscriptionUpdate(id)
	if update.FailureClass != store.SubscriptionFailureTemporary || update.NextAttemptAt != now.Add(baseRetryDelay).Format(time.RFC3339) {
		t.Fatalf("temporary failure state = %+v", update)
	}
	if report := service.AttemptPending(); len(report.Results) != 0 || attempts != 1 {
		t.Fatalf("backoff was ignored: report=%+v attempts=%d", report, attempts)
	}
	now = now.Add(baseRetryDelay)
	report := service.AttemptPending()
	result, _ := report.ResultFor(id)
	if result.Outcome != "completed" || attempts != 2 {
		t.Fatalf("retry report=%+v attempts=%d", report, attempts)
	}
}

func TestStaleCompileResultIsDiscarded(t *testing.T) {
	st := testStore(t)
	id := savePending(t, st, "one")
	first := true
	service := newWithCompiler(st, func(subscription store.OutputSubscription) (compiler.Result, error) {
		if first {
			first = false
			if _, err := st.MarkOutputSubscriptionPending(subscription.ID); err != nil {
				t.Fatal(err)
			}
			return compiler.Result{Body: []byte("stale"), Revision: "stale"}, nil
		}
		return compiler.Result{Body: []byte("current"), Revision: "current"}, nil
	})

	report := service.AttemptPending()
	result, _ := report.ResultFor(id)
	if result.Outcome != "stale" {
		t.Fatalf("stale report = %+v", report)
	}
	if _, err := st.GetSubscriptionArtifact(id); err == nil {
		t.Fatal("stale artifact was published")
	}
	report = service.AttemptPending()
	result, _ = report.ResultFor(id)
	if result.Outcome != "completed" {
		t.Fatalf("current report = %+v", report)
	}
	artifact, _ := st.GetSubscriptionArtifact(id)
	if string(artifact.Body) != "current" {
		t.Fatalf("artifact = %+v", artifact)
	}
}

func TestExecutionIsGloballySerial(t *testing.T) {
	st := testStore(t)
	savePending(t, st, "one")
	savePending(t, st, "two")
	var active atomic.Int32
	var maximum atomic.Int32
	service := newWithCompiler(st, func(subscription store.OutputSubscription) (compiler.Result, error) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return compiler.Result{Body: []byte(subscription.Name), Revision: subscription.Name}, nil
	})

	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			service.AttemptPending()
		}()
	}
	wait.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent compilers = %d", maximum.Load())
	}
}

func TestDeleteWinsOverRunningCompile(t *testing.T) {
	st := testStore(t)
	id := savePending(t, st, "one")
	service := newWithCompiler(st, func(subscription store.OutputSubscription) (compiler.Result, error) {
		if err := st.DeleteOutputSubscription(subscription.ID); err != nil {
			t.Fatal(err)
		}
		return compiler.Result{Body: []byte("late"), Revision: "late"}, nil
	})

	report := service.AttemptPending()
	result, _ := report.ResultFor(id)
	if result.Outcome != "stale" {
		t.Fatalf("delete race report = %+v", report)
	}
	if _, err := st.GetSubscriptionArtifact(id); err == nil {
		t.Fatal("late artifact was recreated")
	}
}
