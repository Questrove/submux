package lifecycle

import (
	"testing"
	"time"

	"submux/internal/store"
)

func TestClassifyHighConfidenceInformationNodes(t *testing.T) {
	traffic := ClassifyLabel("🚀 剩余流量：12.5 GB")
	if traffic == nil || traffic.Type != "traffic_remaining" || traffic.Confidence != "high" || traffic.Value != 13421772800 {
		t.Fatalf("traffic notice not classified: %+v", traffic)
	}
	expiry := ClassifyLabel("套餐到期：2026-08-01")
	if expiry == nil || expiry.TextValue != "2026-08-01T23:59:59Z" {
		t.Fatalf("expiry notice not classified: %+v", expiry)
	}
	announcement := ClassifyLabel("官网：example.com")
	if announcement == nil || announcement.Confidence != "medium" {
		t.Fatalf("announcement should remain a candidate: %+v", announcement)
	}
	if got := ClassifyLabel("香港 剩余流量优化线路"); got != nil {
		t.Fatalf("fuzzy real node was classified: %+v", got)
	}
}

func TestHeaderWinsAndConflictsAreVisible(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	header, ok := ParseSubscriptionUserinfo("upload=10; download=20; total=100; expire=1785456000", now)
	if !ok || header.Remaining != 70 || header.Provenance["remaining"] != "header" {
		t.Fatalf("header parse failed: %+v", header)
	}
	merged := MergeMetadata(store.SubscriptionMetadata{}, header, true, []*store.NodeNotice{{
		Type: "traffic_remaining", Value: 50, Confidence: "high",
	}}, now)
	if merged.Remaining != 70 || len(merged.Conflicts) != 1 {
		t.Fatalf("header priority/conflict failed: %+v", merged)
	}
}

func TestPartialHeaderDoesNotInventRemainingTraffic(t *testing.T) {
	metadata, ok := ParseSubscriptionUserinfo("download=20; total=100", time.Now())
	if !ok {
		t.Fatal("partial header should still be recognized")
	}
	if metadata.Provenance["remaining"] != "" {
		t.Fatalf("partial header invented remaining traffic: %+v", metadata)
	}
}

func TestNoticeEnforcementRequiresTrust(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	metadata := store.SubscriptionMetadata{
		ExpiresAt:  "2026-07-14T23:59:59Z",
		Provenance: map[string]string{"expires_at": "node_name"},
	}
	source := store.Source{Kind: store.SourceKindSubscription, LifecyclePolicy: store.LifecycleStrict, WarnBeforeDays: 7}
	status := Evaluate(source, store.Cache{LastSuccessAt: now.Format(time.RFC3339), Metadata: metadata}, now)
	if status.Entitlement != EntitlementExpired || status.Enforceable || ShouldExclude(source, status) {
		t.Fatalf("untrusted node-name expiry was enforced: %+v", status)
	}
	source.TrustNodeNotices = true
	status = Evaluate(source, store.Cache{LastSuccessAt: now.Format(time.RFC3339), Metadata: metadata}, now)
	if !status.Enforceable || !ShouldExclude(source, status) {
		t.Fatalf("trusted expiry was not enforced: %+v", status)
	}
}

func TestMissingMetadataPreservesPreviousAsStale(t *testing.T) {
	previous := store.SubscriptionMetadata{
		ExpiresAt:  "2030-01-01T00:00:00Z",
		Provenance: map[string]string{"expires_at": "header"},
	}
	merged := MergeMetadata(previous, store.SubscriptionMetadata{}, false, nil, time.Now())
	if merged.ExpiresAt != previous.ExpiresAt || !merged.Stale {
		t.Fatalf("previous metadata was not preserved as stale: %+v", merged)
	}
}
