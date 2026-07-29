package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type deletionFixture struct {
	store          *Store
	sourceID       int64
	nodeID         int64
	templateID     int64
	versionID      int64
	ruleProfileID  int64
	subscriptionID int64
}

func newDeletionFixture(t *testing.T) deletionFixture {
	t.Helper()
	st := newTestStore(t)
	sourceID, err := st.CreateSource(Source{Name: "manual", Kind: SourceKindManual})
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := st.CreateManualNode(NodeRecord{
		SourceID: sourceID, Name: "node", Protocol: "ss", Fingerprint: "node",
		Config: json.RawMessage(`{"name":"node","type":"ss"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	templateID, err := st.SaveTemplate(Template{
		Name: "template", Engine: "mihomo", Scenario: "desktop", Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.PublishTemplateVersion(templateID, "", "proxies: []", nil)
	if err != nil {
		t.Fatal(err)
	}
	ruleProfileID, err := st.SaveRuleProfile(RuleProfile{
		Name: "rules", FallbackAction: "proxy",
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriptionID, err := st.SaveOutputSubscription(OutputSubscription{
		Name:              "consumer",
		Token:             "consumer",
		Enabled:           true,
		TemplateVersionID: version.ID,
		RuleProfileID:     ruleProfileID,
		Bindings: []SubscriptionBinding{{
			Slot: "primary", NodeIDs: []int64{nodeID},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return deletionFixture{
		store:          st,
		sourceID:       sourceID,
		nodeID:         nodeID,
		templateID:     templateID,
		versionID:      version.ID,
		ruleProfileID:  ruleProfileID,
		subscriptionID: subscriptionID,
	}
}

func TestReferencedResourcesCannotBeDeleted(t *testing.T) {
	tests := []struct {
		name         string
		resourceKind string
		id           func(deletionFixture) int64
		delete       func(deletionFixture) error
		assertExists func(*testing.T, deletionFixture)
	}{
		{
			name: "source", resourceKind: resourceKindSource,
			id:     func(f deletionFixture) int64 { return f.sourceID },
			delete: func(f deletionFixture) error { return f.store.DeleteSource(f.sourceID) },
			assertExists: func(t *testing.T, f deletionFixture) {
				t.Helper()
				if _, err := f.store.GetSource(f.sourceID); err != nil {
					t.Fatalf("source was partially deleted: %v", err)
				}
				if _, err := f.store.GetNode(f.nodeID); err != nil {
					t.Fatalf("source node was partially deleted: %v", err)
				}
			},
		},
		{
			name: "node", resourceKind: resourceKindNode,
			id:     func(f deletionFixture) int64 { return f.nodeID },
			delete: func(f deletionFixture) error { return f.store.DeleteNode(f.nodeID) },
			assertExists: func(t *testing.T, f deletionFixture) {
				t.Helper()
				if _, err := f.store.GetNode(f.nodeID); err != nil {
					t.Fatalf("node was deleted: %v", err)
				}
			},
		},
		{
			name: "template", resourceKind: resourceKindTemplate,
			id:     func(f deletionFixture) int64 { return f.templateID },
			delete: func(f deletionFixture) error { return f.store.DeleteTemplate(f.templateID) },
			assertExists: func(t *testing.T, f deletionFixture) {
				t.Helper()
				if _, err := f.store.GetTemplate(f.templateID); err != nil {
					t.Fatalf("template was partially deleted: %v", err)
				}
				versions, err := f.store.ListTemplateVersions(f.templateID)
				if err != nil || len(versions) != 1 || versions[0].ID != f.versionID {
					t.Fatalf("template versions were partially deleted: %v %+v", err, versions)
				}
			},
		},
		{
			name: "rule profile", resourceKind: resourceKindRuleProfile,
			id:     func(f deletionFixture) int64 { return f.ruleProfileID },
			delete: func(f deletionFixture) error { return f.store.DeleteRuleProfile(f.ruleProfileID) },
			assertExists: func(t *testing.T, f deletionFixture) {
				t.Helper()
				if _, err := f.store.GetRuleProfile(f.ruleProfileID); err != nil {
					t.Fatalf("rule profile was deleted: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDeletionFixture(t)
			before, err := fixture.store.OutputInputsGeneration()
			if err != nil {
				t.Fatal(err)
			}
			err = test.delete(fixture)
			var conflict *ResourceDeletionConflict
			if !errors.As(err, &conflict) {
				t.Fatalf("delete error = %v, want ResourceDeletionConflict", err)
			}
			if conflict.ResourceKind != test.resourceKind || conflict.ResourceID != test.id(fixture) || conflict.Reason != resourceDeletionReferenced {
				t.Fatalf("conflict = %+v", conflict)
			}
			if len(conflict.References) != 1 ||
				conflict.References[0].SubscriptionID != fixture.subscriptionID ||
				conflict.References[0].SubscriptionName != "consumer" {
				t.Fatalf("references = %+v", conflict.References)
			}
			if !strings.Contains(conflict.Error(), "consumer") {
				t.Fatalf("conflict message omits subscription name: %q", conflict.Error())
			}
			after, err := fixture.store.OutputInputsGeneration()
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("failed deletion changed input generation: before=%d after=%d", before, after)
			}
			test.assertExists(t, fixture)
			if _, err := fixture.store.GetOutputSubscription(fixture.subscriptionID); err != nil {
				t.Fatalf("referencing subscription changed: %v", err)
			}
		})
	}
}

func TestAllSubscriptionStatesPreventResourceDeletion(t *testing.T) {
	st := newTestStore(t)
	sourceID, err := st.CreateSource(Source{Name: "manual", Kind: SourceKindManual})
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := st.CreateManualNode(NodeRecord{
		SourceID: sourceID, Name: "node", Protocol: "ss", Fingerprint: "node",
		Config: json.RawMessage(`{"name":"node","type":"ss"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	values := []OutputSubscription{
		{Name: "enabled", Token: "enabled", Enabled: true},
		{Name: "disabled", Token: "disabled", Enabled: false},
		{Name: "expired", Token: "expired", Enabled: true, ExpiresAt: "2000-01-01T00:00:00Z"},
		{Name: "blocked", Token: "blocked", Enabled: true},
	}
	var ids []int64
	for _, value := range values {
		value.Bindings = []SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{nodeID, nodeID}}}
		id, err := st.SaveOutputSubscription(value)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	update, err := st.GetSubscriptionUpdate(ids[3])
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := st.RecordSubscriptionUpdateFailureIfCurrent(
		ids[3], update.InputGeneration, SubscriptionFailureLifecycle,
		"source blocked", "expired", nil, time.Time{},
	)
	if err != nil || !recorded {
		t.Fatalf("block subscription: recorded=%v err=%v", recorded, err)
	}

	err = st.DeleteNode(nodeID)
	var conflict *ResourceDeletionConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("delete error = %v, want ResourceDeletionConflict", err)
	}
	if len(conflict.References) != len(values) {
		t.Fatalf("references = %+v", conflict.References)
	}
	for i, reference := range conflict.References {
		if reference.SubscriptionID != ids[i] || reference.SubscriptionName != values[i].Name {
			t.Fatalf("reference %d = %+v, want id=%d name=%q", i, reference, ids[i], values[i].Name)
		}
	}
}

func TestProtectedResourcesCannotBeDeleted(t *testing.T) {
	st := newTestStore(t)
	builtinSourceID, err := st.CreateSource(Source{
		Name: "built-in", Kind: SourceKindManual, Builtin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriptionSourceID, err := st.CreateSource(Source{Name: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSourceNodes(subscriptionSourceID, []NodeRecord{{
		Origin: SourceKindSubscription, Name: "remote node", Fingerprint: "remote-node",
	}}); err != nil {
		t.Fatal(err)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("list subscription node: %v %+v", err, nodes)
	}
	builtinProfileID, err := st.SaveRuleProfile(RuleProfile{
		Name: "built-in rules", Builtin: true, FallbackAction: "proxy",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.OutputInputsGeneration()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		resourceKind string
		resourceID   int64
		delete       func() error
	}{
		{"built-in source", resourceKindSource, builtinSourceID, func() error { return st.DeleteSource(builtinSourceID) }},
		{"subscription node", resourceKindNode, nodes[0].ID, func() error { return st.DeleteNode(nodes[0].ID) }},
		{"built-in rule profile", resourceKindRuleProfile, builtinProfileID, func() error { return st.DeleteRuleProfile(builtinProfileID) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var conflict *ResourceDeletionConflict
			if err := test.delete(); !errors.As(err, &conflict) {
				t.Fatalf("delete error = %v, want ResourceDeletionConflict", err)
			}
			if conflict.ResourceKind != test.resourceKind || conflict.ResourceID != test.resourceID || conflict.Reason != resourceDeletionProtected || len(conflict.References) != 0 {
				t.Fatalf("conflict = %+v", conflict)
			}
		})
	}
	after, err := st.OutputInputsGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("protected deletions changed input generation: before=%d after=%d", before, after)
	}
}

func TestSuccessfulResourceDeletionCascadesAndBumpsInputGeneration(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		st := newTestStore(t)
		sourceID, err := st.CreateSource(Source{Name: "remote"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.ReplaceSourceNodes(sourceID, []NodeRecord{
			{Origin: SourceKindSubscription, Name: "one", Fingerprint: "one"},
			{Origin: SourceKindSubscription, Name: "two", Fingerprint: "two"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertCacheSuccess(sourceID, "upload=1"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RecordLifecycleState(sourceID, "active"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RecordLifecycleState(sourceID, "expired"); err != nil {
			t.Fatal(err)
		}
		before, _ := st.OutputInputsGeneration()
		if err := st.DeleteSource(sourceID); err != nil {
			t.Fatal(err)
		}
		assertInputGenerationIncremented(t, st, before)
		if _, err := st.GetSource(sourceID); err == nil {
			t.Fatal("source still exists")
		}
		nodes, err := st.ListNodes()
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range nodes {
			if node.SourceID == sourceID {
				t.Fatalf("source node still exists: %+v", node)
			}
		}
		if _, err := st.GetCache(sourceID); err == nil {
			t.Fatal("source cache still exists")
		}
		events, err := st.ListLifecycleEvents(100)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.SourceID == sourceID {
				t.Fatalf("source lifecycle event still exists: %+v", event)
			}
		}
	})

	t.Run("node", func(t *testing.T) {
		st := newTestStore(t)
		sourceID, _ := st.CreateSource(Source{Name: "manual", Kind: SourceKindManual})
		nodeID, err := st.CreateManualNode(NodeRecord{
			SourceID: sourceID, Name: "node", Fingerprint: "node",
		})
		if err != nil {
			t.Fatal(err)
		}
		before, _ := st.OutputInputsGeneration()
		if err := st.DeleteNode(nodeID); err != nil {
			t.Fatal(err)
		}
		assertInputGenerationIncremented(t, st, before)
		if _, err := st.GetNode(nodeID); err == nil {
			t.Fatal("node still exists")
		}
	})

	t.Run("template", func(t *testing.T) {
		st := newTestStore(t)
		templateID, err := st.SaveTemplate(Template{
			Name: "template", Engine: "mihomo", Scenario: "desktop", Status: "draft",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PublishTemplateVersion(templateID, "", "one", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PublishTemplateVersion(templateID, "", "two", nil); err != nil {
			t.Fatal(err)
		}
		before, _ := st.OutputInputsGeneration()
		if err := st.DeleteTemplate(templateID); err != nil {
			t.Fatal(err)
		}
		assertInputGenerationIncremented(t, st, before)
		if _, err := st.GetTemplate(templateID); err == nil {
			t.Fatal("template still exists")
		}
		versions, err := st.ListTemplateVersions(templateID)
		if err != nil || len(versions) != 0 {
			t.Fatalf("template versions still exist: %v %+v", err, versions)
		}
	})

	t.Run("rule profile", func(t *testing.T) {
		st := newTestStore(t)
		id, err := st.SaveRuleProfile(RuleProfile{Name: "rules", FallbackAction: "proxy"})
		if err != nil {
			t.Fatal(err)
		}
		before, _ := st.OutputInputsGeneration()
		if err := st.DeleteRuleProfile(id); err != nil {
			t.Fatal(err)
		}
		assertInputGenerationIncremented(t, st, before)
		if _, err := st.GetRuleProfile(id); err == nil {
			t.Fatal("rule profile still exists")
		}
	})
}

func TestResourceDeletionNotFoundAndConcurrentPublicationGuard(t *testing.T) {
	st := newTestStore(t)
	before, err := st.OutputInputsGeneration()
	if err != nil {
		t.Fatal(err)
	}
	for name, deleteResource := range map[string]func() error{
		"source":       func() error { return st.DeleteSource(99) },
		"node":         func() error { return st.DeleteNode(99) },
		"template":     func() error { return st.DeleteTemplate(99) },
		"rule profile": func() error { return st.DeleteRuleProfile(99) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := deleteResource(); !IsResourceDeletionNotFound(err) {
				t.Fatalf("delete error = %v, want resource deletion not found", err)
			}
		})
	}
	after, _ := st.OutputInputsGeneration()
	if after != before {
		t.Fatalf("not-found deletions changed input generation: before=%d after=%d", before, after)
	}

	sourceID, _ := st.CreateSource(Source{Name: "manual", Kind: SourceKindManual})
	nodeID, err := st.CreateManualNode(NodeRecord{
		SourceID: sourceID, Name: "node", Fingerprint: "node",
	})
	if err != nil {
		t.Fatal(err)
	}
	guardGeneration, _ := st.OutputInputsGeneration()
	if err := st.DeleteNode(nodeID); err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveOutputSubscriptionWithArtifact(OutputSubscription{
		Name: "stale", Token: "stale", Enabled: true,
		Bindings: []SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{nodeID}}},
	}, SubscriptionArtifact{Body: []byte("stale")}, nil, OutputPublicationGuard{
		InputsGeneration: guardGeneration,
	})
	if !errors.Is(err, ErrOutputInputsChanged) {
		t.Fatalf("stale publication error = %v, want ErrOutputInputsChanged", err)
	}
}

func assertInputGenerationIncremented(t *testing.T, st *Store, before uint64) {
	t.Helper()
	after, err := st.OutputInputsGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("input generation = %d, want %d", after, before+1)
	}
}
