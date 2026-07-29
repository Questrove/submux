package runtimestate

import (
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

func TestOperationAuditCapturesActorClientStagesAndRedactsFailures(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	peer := runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-21-test"}
	operation, _, err := store.SubmitOperation(peer, "gui", "1.2.3", runtimeapi.CreateOperationRequest{
		RequestID:  "request-audit",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit Runtime operation: %v", err)
	}
	if _, found, err := store.BeginNextOperation(now.Add(time.Second)); err != nil || !found {
		t.Fatalf("begin Runtime operation: found=%v err=%v", found, err)
	}
	if err := store.UpdateOperationStage(operation.ID, "starting_proxy", 50, false, now.Add(2*time.Second)); err != nil {
		t.Fatalf("update Runtime operation: %v", err)
	}
	if err := store.CompleteOperation(operation.ID, runtimeapi.OperationFailed, nil, &runtimeapi.ProtocolError{
		Code:    "test_failure",
		Message: `GET https://user:pass@[2001:db8::1]/config?token=one&token=two failed at C:\Users\Test\config.yaml`,
	}, now.Add(3*time.Second)); err != nil {
		t.Fatalf("complete Runtime operation: %v", err)
	}

	records, err := store.RecentAudit(10)
	if err != nil {
		t.Fatalf("read Runtime audit: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("Runtime audit record count=%d records=%#v", len(records), records)
	}
	if records[0].Actor != peer.Key() || records[0].ClientType != "gui" || records[0].ClientVersion != "1.2.3" ||
		records[0].RequestID != "request-audit" || records[0].OperationID != operation.ID ||
		records[0].Action != runtimeapi.ActionStartProxy || records[0].Stage != "failed" ||
		records[0].Result != runtimeapi.OperationFailed {
		t.Fatalf("final Runtime audit record=%#v", records[0])
	}
	for _, secret := range []string{"user", "pass", "one", "two", `C:\Users\Test`} {
		if strings.Contains(records[0].Error.Message, secret) {
			t.Fatalf("Runtime audit failure leaked %q: %s", secret, records[0].Error.Message)
		}
	}
	stages := []string{records[3].Stage, records[2].Stage, records[1].Stage, records[0].Stage}
	if strings.Join(stages, ",") != "accepted,preparing,starting_proxy,failed" {
		t.Fatalf("Runtime audit stages=%v", stages)
	}
}

func TestAuditRetentionKeepsConfiguredCount(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		for index := 0; index < MaxRetainedAuditRecords+5; index++ {
			if err := appendAuditRecord(transaction, runtimeapi.AuditRecord{
				Actor:         "test:uid:1000",
				ClientType:    "cli",
				ClientVersion: "test",
				Action:        "test",
				Stage:         "completed",
				Result:        "succeeded",
				At:            now.Add(time.Duration(index) * time.Millisecond),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed Runtime audit: %v", err)
	}
	if _, err := store.GCOperationHistory(now.Add(time.Hour)); err != nil {
		t.Fatalf("prune Runtime audit: %v", err)
	}
	records, err := store.RecentAudit(MaxRetainedAuditRecords)
	if err != nil {
		t.Fatalf("read Runtime audit: %v", err)
	}
	if len(records) != MaxRetainedAuditRecords {
		t.Fatalf("retained Runtime audit records=%d", len(records))
	}
}
