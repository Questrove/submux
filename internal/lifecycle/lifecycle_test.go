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
	neverExpiry := ClassifyLabel("套餐到期：长期有效")
	if neverExpiry == nil || neverExpiry.Type != "expires_never" || neverExpiry.Confidence != "high" {
		t.Fatalf("never-expiring notice not classified: %+v", neverExpiry)
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
		Type: "traffic_remaining", Value: 2 << 30, Confidence: "high",
	}}, now)
	if merged.Remaining != 70 || len(merged.Conflicts) != 1 {
		t.Fatalf("header priority/conflict failed: %+v", merged)
	}
}

func TestRoundedTrafficNoticeUsesGenerousConflictTolerance(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	header := store.SubscriptionMetadata{
		Remaining:  100 << 30,
		Provenance: map[string]string{"remaining": "header"},
	}
	withinTolerance := MergeMetadata(store.SubscriptionMetadata{}, header, true, []*store.NodeNotice{{
		Type: "traffic_remaining", Value: (100 << 30) - (512 << 20), Confidence: "high",
	}}, now)
	if len(withinTolerance.Conflicts) != 0 || withinTolerance.Remaining != header.Remaining {
		t.Fatalf("rounded traffic value was reported as a conflict: %+v", withinTolerance)
	}
	beyondTolerance := MergeMetadata(store.SubscriptionMetadata{}, header, true, []*store.NodeNotice{{
		Type: "traffic_remaining", Value: 98 << 30, Confidence: "high",
	}}, now)
	if len(beyondTolerance.Conflicts) != 1 || beyondTolerance.Remaining != header.Remaining {
		t.Fatalf("meaningful traffic difference was not reported: %+v", beyondTolerance)
	}
}

func TestExpiryNoticeOnSameDateDoesNotConflict(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	header := store.SubscriptionMetadata{
		ExpiresAt:  "2027-07-16T12:02:28Z",
		Provenance: map[string]string{"expires_at": "header"},
	}
	sameDate := MergeMetadata(store.SubscriptionMetadata{}, header, true, []*store.NodeNotice{{
		Type: "expires_at", TextValue: "2027-07-16T23:59:59Z", Confidence: "high",
	}}, now)
	if len(sameDate.Conflicts) != 0 || sameDate.ExpiresAt != header.ExpiresAt {
		t.Fatalf("same-date expiry was reported as a conflict: %+v", sameDate)
	}
	differentDate := MergeMetadata(store.SubscriptionMetadata{}, header, true, []*store.NodeNotice{{
		Type: "expires_at", TextValue: "2027-07-17T23:59:59Z", Confidence: "high",
	}}, now)
	if len(differentDate.Conflicts) != 1 || differentDate.ExpiresAt != header.ExpiresAt {
		t.Fatalf("different expiry date was not reported: %+v", differentDate)
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

func TestZeroTotalIsNotEnforcedAsExhausted(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	header, ok := ParseSubscriptionUserinfo("upload=0; download=0; total=0; expire=0", now)
	if !ok || header.Provenance["total"] != "header" || header.Provenance["remaining"] != "" {
		t.Fatalf("zero total should be recognized without inventing remaining traffic: %+v", header)
	}
	previous := store.SubscriptionMetadata{
		Total: 100, Remaining: 0,
		Provenance: map[string]string{"total": "header", "remaining": "header"},
	}
	merged := MergeMetadata(previous, header, true, []*store.NodeNotice{{
		Type: "traffic_remaining", Value: 0, Confidence: "high",
	}}, now)
	if merged.Total != 0 || merged.Provenance["remaining"] != "" || merged.Stale {
		t.Fatalf("unlimited header did not clear the old finite quota: %+v", merged)
	}
	source := store.Source{Kind: store.SourceKindSubscription, LifecyclePolicy: store.LifecycleStrict, WarnBeforeDays: 7}
	status := Evaluate(source, store.Cache{LastSuccessAt: now.Format(time.RFC3339), Metadata: merged}, now)
	if !status.UnlimitedTraffic || status.Entitlement != EntitlementActive || status.RemainingBytes != nil || status.TotalBytes != nil || ShouldExclude(source, status) {
		t.Fatalf("zero total was enforced as exhaustion: %+v", status)
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

func TestNeverExpiresNoticeClearsPreviousNodeDate(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	previous := store.SubscriptionMetadata{
		ExpiresAt:  "2026-08-01T23:59:59Z",
		Provenance: map[string]string{"expires_at": "node_name"},
	}
	merged := MergeMetadata(previous, store.SubscriptionMetadata{}, false, []*store.NodeNotice{{
		Type: "expires_never", TextValue: "long_term", Confidence: "high",
	}}, now)
	if merged.ExpiresAt != "" || merged.Provenance["expires_at"] != "node_name" || merged.Stale {
		t.Fatalf("never-expiring notice did not replace the previous node date: %+v", merged)
	}
	source := store.Source{Kind: store.SourceKindSubscription, LifecyclePolicy: store.LifecycleStrict, WarnBeforeDays: 7}
	status := Evaluate(source, store.Cache{LastSuccessAt: now.Format(time.RFC3339), Metadata: merged}, now)
	if status.Entitlement != EntitlementActive || !status.NeverExpires || ShouldExclude(source, status) {
		t.Fatalf("never-expiring source was not kept active: %+v", status)
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
