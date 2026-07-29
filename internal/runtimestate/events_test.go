package runtimestate

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

func TestEventsAfterUsesMonotonicCursorAndExpiresOnlyMissingHistory(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	err = store.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		events := transaction.Bucket(eventsBucket)
		for cursor := uint64(1); cursor <= MaxRetainedEvents+5; cursor++ {
			event := runtimeapi.Event{
				Cursor:           cursor,
				Type:             "test.changed",
				At:               now.Add(time.Duration(cursor) * time.Second),
				SnapshotRevision: cursor + 1,
			}
			if err := putJSON(events, encodeUint64(cursor), event); err != nil {
				return err
			}
		}
		if err := metadata.Put(eventCursorKey, encodeUint64(MaxRetainedEvents+5)); err != nil {
			return err
		}
		return pruneEventHistory(transaction)
	})
	if err != nil {
		t.Fatalf("seed Runtime events: %v", err)
	}

	if _, earliest, err := store.EventsAfter(4, 10); !errors.Is(err, ErrCursorExpired) || earliest != 6 {
		t.Fatalf("expired event cursor: earliest=%d err=%v", earliest, err)
	}
	events, earliest, err := store.EventsAfter(5, 3)
	if err != nil {
		t.Fatalf("read retained Runtime events: %v", err)
	}
	if earliest != 6 || len(events) != 3 {
		t.Fatalf("retained events: earliest=%d events=%#v", earliest, events)
	}
	for index, event := range events {
		want := uint64(6 + index)
		if event.Cursor != want {
			t.Fatalf("event %d cursor=%d, want %d", index, event.Cursor, want)
		}
	}
	if _, _, err := store.EventsAfter(MaxRetainedEvents+6, 1); !errors.Is(err, ErrInvalidEventCursor) {
		t.Fatalf("future event cursor error = %v", err)
	}
}

func TestEventRetentionPreservesOutcomeUnknownAuditRecords(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	const protectedID = "op_outcome_unknown"

	err = store.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		events := transaction.Bucket(eventsBucket)
		metadata := transaction.Bucket(metadataBucket)
		if err := putJSON(operations, []byte(protectedID), runtimeapi.Operation{
			ID:        protectedID,
			State:     runtimeapi.OperationOutcomeUnknown,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			return err
		}
		for cursor := uint64(1); cursor <= MaxRetainedEvents+1; cursor++ {
			operationID := ""
			if cursor == 1 {
				operationID = protectedID
			}
			if err := putJSON(events, encodeUint64(cursor), runtimeapi.Event{
				Cursor:           cursor,
				Type:             "operation.changed",
				At:               now,
				OperationID:      operationID,
				SnapshotRevision: cursor + 1,
			}); err != nil {
				return err
			}
		}
		if err := metadata.Put(eventCursorKey, encodeUint64(MaxRetainedEvents+1)); err != nil {
			return err
		}
		return pruneEventHistory(transaction)
	})
	if err != nil {
		t.Fatalf("seed protected Runtime event: %v", err)
	}

	err = store.db.View(func(transaction *bbolt.Tx) error {
		events := transaction.Bucket(eventsBucket)
		if events.Get(encodeUint64(1)) == nil {
			return errors.New("outcome-unknown event was deleted")
		}
		if events.Get(encodeUint64(2)) != nil {
			return errors.New("oldest unprotected event was retained")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, earliest, err := store.EventsAfter(1, 1); !errors.Is(err, ErrCursorExpired) || earliest != 3 {
		t.Fatalf("gap after protected audit event: earliest=%d err=%v", earliest, err)
	}
}

func TestOperationHistoryGCPreservesActiveUnknownAndCurrentFailure(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-DefaultOperationTTL - time.Hour)
	operations := []runtimeapi.Operation{
		{ID: "old-success", RequestID: "request-old-success", State: runtimeapi.OperationSucceeded, UpdatedAt: old},
		{ID: "old-cancelled", RequestID: "request-old-cancelled", State: runtimeapi.OperationCancelled, UpdatedAt: old},
		{ID: "old-failure", RequestID: "request-old-failure", State: runtimeapi.OperationFailed, UpdatedAt: old},
		{ID: "current-failure", RequestID: "request-current-failure", State: runtimeapi.OperationFailed, UpdatedAt: old.Add(time.Minute)},
		{ID: "queued", RequestID: "request-queued", State: runtimeapi.OperationQueued, UpdatedAt: old},
		{ID: "running", RequestID: "request-running", State: runtimeapi.OperationRunning, UpdatedAt: old},
		{ID: "unknown", RequestID: "request-unknown", State: runtimeapi.OperationOutcomeUnknown, UpdatedAt: old},
		{ID: "recent-cancelled", RequestID: "request-recent-cancelled", State: runtimeapi.OperationCancelled, UpdatedAt: now},
	}
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		operationBucket := transaction.Bucket(operationsBucket)
		requestBucket := transaction.Bucket(requestsBucket)
		for _, operation := range operations {
			operation.CreatedAt = operation.UpdatedAt
			if err := putJSON(operationBucket, []byte(operation.ID), operation); err != nil {
				return err
			}
			if err := putJSON(requestBucket, []byte(operation.RequestID), requestRecord{
				OperationID: operation.ID,
				Caller:      "test:uid:1000",
				Fingerprint: operation.ID,
			}); err != nil {
				return err
			}
			if operation.ID == "old-cancelled" || operation.ID == "recent-cancelled" {
				if err := putJSON(transaction.Bucket(cancelRequestsBucket), []byte("cancel-"+operation.ID), cancelRequestRecord{
					OperationID: operation.ID,
					Caller:      "test:uid:1001",
					Fingerprint: operation.ID,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed Runtime operation history: %v", err)
	}

	removed, err := store.GCOperationHistory(now)
	if err != nil {
		t.Fatalf("clean Runtime operation history: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed operations=%d, want 3", removed)
	}
	for _, id := range []string{"current-failure", "queued", "running", "unknown", "recent-cancelled"} {
		if _, err := store.GetOperation(id); err != nil {
			t.Fatalf("protected operation %q: %v", id, err)
		}
	}
	for _, id := range []string{"old-success", "old-cancelled", "old-failure"} {
		if _, err := store.GetOperation(id); !errors.Is(err, ErrOperationNotFound) {
			t.Fatalf("expired operation %q error=%v", id, err)
		}
	}
	err = store.db.View(func(transaction *bbolt.Tx) error {
		requests := transaction.Bucket(requestsBucket)
		if requests.Get([]byte("request-old-success")) != nil {
			return errors.New("deleted operation idempotency record was retained")
		}
		if requests.Get([]byte("request-current-failure")) == nil {
			return errors.New("protected operation idempotency record was deleted")
		}
		cancels := transaction.Bucket(cancelRequestsBucket)
		if cancels.Get([]byte("cancel-old-cancelled")) != nil {
			return errors.New("deleted operation cancellation record was retained")
		}
		if cancels.Get([]byte("cancel-recent-cancelled")) == nil {
			return errors.New("protected operation cancellation record was deleted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOperationHistoryGCClearsFailureProtectionAfterSuccess(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-DefaultOperationTTL - time.Hour)
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		for _, operation := range []runtimeapi.Operation{
			{ID: "failed", State: runtimeapi.OperationFailed, CreatedAt: old, UpdatedAt: old},
			{ID: "succeeded", State: runtimeapi.OperationSucceeded, CreatedAt: now, UpdatedAt: now},
		} {
			if err := putJSON(operations, []byte(operation.ID), operation); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed resolved Runtime failure: %v", err)
	}
	removed, err := store.GCOperationHistory(now)
	if err != nil || removed != 1 {
		t.Fatalf("clean resolved Runtime failure: removed=%d err=%v", removed, err)
	}
	if _, err := store.GetOperation("failed"); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("resolved failure operation error=%v", err)
	}
}

func TestOperationHistoryGCEnforcesMaximumCount(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		for index := 0; index <= MaxRetainedOperations; index++ {
			id := fmt.Sprintf("op-%04d", index)
			if err := putJSON(operations, []byte(id), runtimeapi.Operation{
				ID:        id,
				State:     runtimeapi.OperationSucceeded,
				CreatedAt: now.Add(time.Duration(index) * time.Second),
				UpdatedAt: now.Add(time.Duration(index) * time.Second),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed Runtime operation count: %v", err)
	}
	removed, err := store.GCOperationHistory(now.Add(time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("clean Runtime operation count: removed=%d err=%v", removed, err)
	}
	if _, err := store.GetOperation("op-0000"); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("oldest operation error=%v", err)
	}
}
