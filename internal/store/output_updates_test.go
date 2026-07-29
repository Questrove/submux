package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestNodeMutationMarksOnlyDependentEnabledSubscriptionPending(t *testing.T) {
	st := newTestStore(t)
	sourceID, err := st.CreateSource(Source{Name: "manual", Kind: SourceKindManual})
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := st.CreateManualNode(NodeRecord{
		SourceID: sourceID, Name: "one", Protocol: "ss", Fingerprint: "one",
		Config: json.RawMessage(`{"name":"one","type":"ss"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	dependentID, err := st.SaveOutputSubscription(OutputSubscription{
		Name: "dependent", Token: "dependent", Enabled: true,
		Bindings: []SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{nodeID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := st.SaveOutputSubscription(OutputSubscription{
		Name: "other", Token: "other", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	disabledID, err := st.SaveOutputSubscription(OutputSubscription{
		Name: "disabled", Token: "disabled", Enabled: false,
		Bindings: []SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{nodeID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady := func(id int64) SubscriptionUpdateState {
		t.Helper()
		state, err := st.GetSubscriptionUpdate(id)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := st.CommitSubscriptionArtifactIfCurrent(id, state.InputGeneration, SubscriptionArtifact{
			Body: []byte("last-good"), ContentType: "text/yaml", Revision: "r1",
		}, nil)
		if err != nil || !ok {
			t.Fatalf("make subscription %d ready: committed=%v err=%v", id, ok, err)
		}
		ready, err := st.GetSubscriptionUpdate(id)
		if err != nil {
			t.Fatal(err)
		}
		return ready
	}
	dependentReady := makeReady(dependentID)
	otherReady := makeReady(otherID)

	if err := st.UpdateNodeMetadata(nodeID, []string{"changed"}, true); err != nil {
		t.Fatal(err)
	}
	dependent, err := st.GetSubscriptionUpdate(dependentID)
	if err != nil {
		t.Fatal(err)
	}
	if dependent.Status != SubscriptionUpdatePending || dependent.InputGeneration != dependentReady.InputGeneration+1 {
		t.Fatalf("dependent update = %+v", dependent)
	}
	other, err := st.GetSubscriptionUpdate(otherID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != SubscriptionUpdateReady || other.InputGeneration != otherReady.InputGeneration {
		t.Fatalf("unrelated update changed = %+v", other)
	}
	if _, err := st.GetSubscriptionUpdate(disabledID); err == nil {
		t.Fatal("disabled subscription unexpectedly has automatic update state")
	}
	artifact, err := st.GetSubscriptionArtifact(dependentID)
	if err != nil || string(artifact.Body) != "last-good" {
		t.Fatalf("last-good artifact was not retained: %+v err=%v", artifact, err)
	}
}

func TestFailedDomainMutationRollsBackPendingMarker(t *testing.T) {
	st := newTestStore(t)
	sourceID, _ := st.CreateSource(Source{Name: "manual", Kind: SourceKindManual})
	nodeID, _ := st.CreateManualNode(NodeRecord{
		SourceID: sourceID, Name: "one", Protocol: "ss", Fingerprint: "one",
		Config: json.RawMessage(`{"name":"one","type":"ss"}`),
	})
	subscriptionID, _ := st.SaveOutputSubscription(OutputSubscription{
		Name: "dependent", Token: "dependent", Enabled: true,
		Bindings: []SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{nodeID}}},
	})
	before, _ := st.GetSubscriptionUpdate(subscriptionID)
	if ok, err := st.CommitSubscriptionArtifactIfCurrent(subscriptionID, before.InputGeneration, SubscriptionArtifact{
		Body: []byte("last-good"), Revision: "r1",
	}, nil); err != nil || !ok {
		t.Fatalf("make ready: committed=%v err=%v", ok, err)
	}
	before, _ = st.GetSubscriptionUpdate(subscriptionID)

	if err := st.SetNodeRoleOverride(nodeID, "notice"); err == nil {
		t.Fatal("manual node role override unexpectedly succeeded")
	}
	after, err := st.GetSubscriptionUpdate(subscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != SubscriptionUpdateReady || after.InputGeneration != before.InputGeneration {
		t.Fatalf("failed mutation leaked pending state: before=%+v after=%+v", before, after)
	}
}

func TestArtifactCommitRejectsStaleGenerationAndDeleteWins(t *testing.T) {
	st := newTestStore(t)
	id, err := st.SaveOutputSubscription(OutputSubscription{Name: "one", Token: "one", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := st.GetSubscriptionUpdate(id)
	if _, err := st.MarkOutputSubscriptionPending(id); err != nil {
		t.Fatal(err)
	}
	if committed, err := st.CommitSubscriptionArtifactIfCurrent(id, first.InputGeneration, SubscriptionArtifact{
		Body: []byte("stale"),
	}, nil); err != nil || committed {
		t.Fatalf("stale generation committed=%v err=%v", committed, err)
	}
	current, _ := st.GetSubscriptionUpdate(id)
	if err := st.DeleteOutputSubscription(id); err != nil {
		t.Fatal(err)
	}
	if committed, err := st.CommitSubscriptionArtifactIfCurrent(id, current.InputGeneration, SubscriptionArtifact{
		Body: []byte("after-delete"),
	}, nil); err != nil || committed {
		t.Fatalf("deleted subscription committed=%v err=%v", committed, err)
	}
	if _, err := st.GetSubscriptionArtifact(id); err == nil {
		t.Fatal("artifact reappeared after delete")
	}
}

func TestAtomicPublicationRejectsChangedInputsAndSubscription(t *testing.T) {
	st := newTestStore(t)
	sourceID, _ := st.CreateSource(Source{Name: "manual", Kind: SourceKindManual})
	nodeID, _ := st.CreateManualNode(NodeRecord{
		SourceID: sourceID, Name: "one", Protocol: "ss", Fingerprint: "one",
		Config: json.RawMessage(`{"name":"one","type":"ss"}`),
	})
	inputsGeneration, err := st.OutputInputsGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeMetadata(nodeID, []string{"changed"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveOutputSubscriptionWithArtifact(OutputSubscription{
		Name: "new", Token: "new", Enabled: true,
	}, SubscriptionArtifact{Body: []byte("stale")}, nil, OutputPublicationGuard{
		InputsGeneration: inputsGeneration,
	}); !errors.Is(err, ErrOutputInputsChanged) {
		t.Fatalf("stale input publication error = %v", err)
	}
	if subscriptions, err := st.ListOutputSubscriptions(); err != nil || len(subscriptions) != 0 {
		t.Fatalf("stale create became visible: subscriptions=%+v err=%v", subscriptions, err)
	}

	id, err := st.SaveOutputSubscription(OutputSubscription{Name: "disabled", Token: "old", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetOutputSubscription(id)
	inputsGeneration, _ = st.OutputInputsGeneration()
	if err := st.UpdateOutputSubscriptionToken(id, "new-token"); err != nil {
		t.Fatal(err)
	}
	current.Enabled = true
	current.Token = "new-token"
	if _, err := st.SaveOutputSubscriptionWithArtifact(current, SubscriptionArtifact{
		Body: []byte("concurrent"),
	}, nil, OutputPublicationGuard{
		InputsGeneration: inputsGeneration, SubscriptionVersion: current.RecordVersion,
	}); !errors.Is(err, ErrOutputSubscriptionChanged) {
		t.Fatalf("concurrent subscription publication error = %v", err)
	}
}

func TestDisableOutputSubscriptionRejectsStaleRecordAndKeepsArtifact(t *testing.T) {
	st := newTestStore(t)
	id, err := st.SaveOutputSubscription(OutputSubscription{Name: "one", Token: "one", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	update, err := st.GetSubscriptionUpdate(id)
	if err != nil {
		t.Fatal(err)
	}
	if committed, err := st.CommitSubscriptionArtifactIfCurrent(id, update.InputGeneration, SubscriptionArtifact{
		Body: []byte("last-good"), Revision: "r1",
	}, nil); err != nil || !committed {
		t.Fatalf("publish artifact: committed=%v err=%v", committed, err)
	}
	stale, err := st.GetOutputSubscription(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOutputSubscriptionToken(id, "changed"); err != nil {
		t.Fatal(err)
	}
	if err := st.DisableOutputSubscriptionIfCurrent(id, stale.RecordVersion); !errors.Is(err, ErrOutputSubscriptionChanged) {
		t.Fatalf("stale disable error = %v", err)
	}
	current, err := st.GetOutputSubscription(id)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Enabled {
		t.Fatalf("stale disable changed subscription: %+v", current)
	}
	if err := st.DisableOutputSubscriptionIfCurrent(id, current.RecordVersion); err != nil {
		t.Fatal(err)
	}
	disabled, err := st.GetOutputSubscription(id)
	if err != nil || disabled.Enabled {
		t.Fatalf("disabled subscription = %+v err=%v", disabled, err)
	}
	if _, err := st.GetSubscriptionUpdate(id); err == nil {
		t.Fatal("disabled subscription retained update state")
	}
	artifact, err := st.GetSubscriptionArtifact(id)
	if err != nil || string(artifact.Body) != "last-good" || artifact.Revision != "r1" {
		t.Fatalf("artifact after disable = %+v err=%v", artifact, err)
	}
}

func TestOpenV10SeparatesLegacyArtifactAndSchedulesEnabledSubscriptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			if name == "subscription_updates" {
				continue
			}
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("9")); err != nil {
			return err
		}
		enabled := OutputSubscription{ID: 1, Name: "enabled", Token: "enabled", Enabled: true}
		disabled := OutputSubscription{ID: 2, Name: "disabled", Token: "disabled", Enabled: false}
		if err := putJSON(tx.Bucket([]byte("subscriptions")), itob(1), enabled); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket([]byte("subscriptions")), itob(2), disabled); err != nil {
			return err
		}
		legacy := map[string]any{
			"subscription_id": int64(1), "body": []byte("kept"), "revision": "r1",
			"last_success": "2026-07-01T00:00:00Z", "last_error": "old error",
			"warnings": []string{"old warning"}, "blocked_reason": "expired",
		}
		if err := putJSON(tx.Bucket([]byte("subscription_artifacts")), itob(1), legacy); err != nil {
			return err
		}
		return putJSON(tx.Bucket([]byte("subscription_artifacts")), itob(2), map[string]any{
			"subscription_id": int64(2), "body": []byte("disabled-kept"),
			"last_error": "old disabled error",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state, err := st.GetSubscriptionUpdate(1)
	if err != nil {
		t.Fatal(err)
	}
	if state.InputGeneration != 1 || state.Status != SubscriptionUpdatePending || state.BlockedReason != "expired" || state.AttemptCount != 0 {
		t.Fatalf("migrated update = %+v", state)
	}
	if _, err := st.GetSubscriptionUpdate(2); err == nil {
		t.Fatal("disabled subscription was scheduled")
	}
	artifact, err := st.GetSubscriptionArtifact(1)
	if err != nil || string(artifact.Body) != "kept" || artifact.Revision != "r1" {
		t.Fatalf("migrated artifact = %+v err=%v", artifact, err)
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"last_error", "warnings", "blocked_reason"} {
		if _, exists := fields[key]; exists {
			t.Fatalf("legacy field %q remained in artifact JSON: %s", key, raw)
		}
	}
	disabledArtifact, err := st.GetSubscriptionArtifact(2)
	if err != nil || string(disabledArtifact.Body) != "disabled-kept" {
		t.Fatalf("disabled artifact was not retained: artifact=%+v err=%v", disabledArtifact, err)
	}
	disabledRaw, _ := json.Marshal(disabledArtifact)
	for _, key := range []string{"last_error", "warnings", "blocked_reason"} {
		if string(disabledRaw) == "" || bytes.Contains(disabledRaw, []byte(key)) {
			t.Fatalf("disabled artifact retained legacy field %q: %s", key, disabledRaw)
		}
	}
	due, err := st.ListDueSubscriptionUpdates(time.Now())
	if err != nil || len(due) != 1 || due[0].Subscription.ID != 1 {
		t.Fatalf("startup work = %+v err=%v", due, err)
	}
}
