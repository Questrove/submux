package outputsubscription

import (
	"errors"
	"strings"
	"testing"
	"time"

	"submux/internal/compiler"
	"submux/internal/node"
	"submux/internal/outputupdate"
	"submux/internal/store"
)

type publisherFixture struct {
	store      *store.Store
	compiler   *compiler.Service
	updater    *outputupdate.Service
	publisher  *Publisher
	versionID  int64
	nodeID     int64
	validInput SaveIntent
}

func newPublisherFixture(t *testing.T) publisherFixture {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/submux.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	compilerModule := compiler.New(st)
	if err := compilerModule.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := compilerModule.EnsureBuiltinRuleProfiles(); err != nil {
		t.Fatal(err)
	}
	sourceID, err := st.EnsureDefaultManualSource()
	if err != nil {
		t.Fatal(err)
	}
	records, err := node.Import(
		sourceID,
		store.SourceKindManual,
		"vless://00000000-0000-0000-0000-000000000001@node.example.com:443?encryption=none&type=tcp#HK",
	)
	if err != nil {
		t.Fatal(err)
	}
	nodeIDs, err := st.CreateManualNodes(records)
	if err != nil || len(nodeIDs) != 1 {
		t.Fatalf("create test node: ids=%v err=%v", nodeIDs, err)
	}
	templates, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	versionID := int64(0)
	for _, template := range templates {
		if template.Engine == compiler.EngineMihomo && template.CurrentVersionID != 0 {
			versionID = template.CurrentVersionID
			break
		}
	}
	if versionID == 0 {
		t.Fatal("built-in Mihomo template is missing")
	}
	updater := outputupdate.New(st, compilerModule)
	validInput := SaveIntent{
		Name: "test output", TemplateVersionID: versionID,
		Bindings: []store.SubscriptionBinding{{Slot: "primary", NodeIDs: nodeIDs}},
	}
	return publisherFixture{
		store: st, compiler: compilerModule, updater: updater,
		publisher: New(st, compilerModule, updater), versionID: versionID,
		nodeID: nodeIDs[0], validInput: validInput,
	}
}

func TestSaveCreatesAndEditsPublishedSubscription(t *testing.T) {
	fixture := newPublisherFixture(t)
	created, err := fixture.publisher.Save(fixture.validInput)
	if err != nil {
		t.Fatal(err)
	}
	if created.SubscriptionID == 0 || created.Token == "" || created.Revision == "" ||
		!created.Enabled || created.Outcome != OutcomeCompleted {
		t.Fatalf("created result = %+v", created)
	}
	subscription, err := fixture.store.GetOutputSubscription(created.SubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if subscription.Engine != compiler.EngineMihomo || subscription.RuleProfileID == 0 ||
		subscription.Token != created.Token || !subscription.Enabled || subscription.RecordVersion != 1 {
		t.Fatalf("stored subscription = %+v", subscription)
	}
	artifact, err := fixture.store.GetSubscriptionArtifact(created.SubscriptionID)
	if err != nil || !strings.Contains(string(artifact.Body), "node.example.com") {
		t.Fatalf("artifact = %+v err=%v", artifact, err)
	}
	update, err := fixture.store.GetSubscriptionUpdate(created.SubscriptionID)
	if err != nil || update.Status != store.SubscriptionUpdateReady {
		t.Fatalf("update = %+v err=%v", update, err)
	}

	editedIntent := fixture.validInput
	editedIntent.ID = created.SubscriptionID
	editedIntent.Name = "edited output"
	edited, err := fixture.publisher.Save(editedIntent)
	if err != nil {
		t.Fatal(err)
	}
	after, err := fixture.store.GetOutputSubscription(created.SubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Token != created.Token || after.Token != created.Token || after.Name != "edited output" ||
		after.RecordVersion != 2 || !after.Enabled {
		t.Fatalf("edited result=%+v subscription=%+v", edited, after)
	}
}

func TestSaveCompilationFailureDoesNotWrite(t *testing.T) {
	fixture := newPublisherFixture(t)
	publisher := newWithDependencies(
		fixture.store,
		func(store.OutputSubscription) (compiler.Result, error) {
			return compiler.Result{}, errors.New("synthetic compile failure")
		},
		fixture.updater.Retry,
		time.Now,
		func() (string, error) { return "test-token", nil },
	)
	if _, err := publisher.Save(fixture.validInput); !errors.Is(err, ErrInvalidIntent) {
		t.Fatalf("save error = %v", err)
	}
	subscriptions, err := fixture.store.ListOutputSubscriptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 0 {
		t.Fatalf("failed save wrote subscriptions: %+v", subscriptions)
	}
}

func TestSaveRetriesWhenGlobalInputsChange(t *testing.T) {
	fixture := newPublisherFixture(t)
	calls := 0
	publisher := newWithDependencies(
		fixture.store,
		func(value store.OutputSubscription) (compiler.Result, error) {
			calls++
			if calls == 1 {
				if err := fixture.store.SetSetting(
					store.SharedFakeIPSettingKey,
					`{"mode":"blacklist","entries":["+.retry.test"]}`,
				); err != nil {
					return compiler.Result{}, err
				}
			}
			return fixture.compiler.Preview(value)
		},
		fixture.updater.Retry,
		time.Now,
		func() (string, error) { return "test-token", nil },
	)
	if _, err := publisher.Save(fixture.validInput); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("compile calls = %d, want 2", calls)
	}
}

func TestSaveRejectsConcurrentSubscriptionEdit(t *testing.T) {
	fixture := newPublisherFixture(t)
	created, err := fixture.publisher.Save(fixture.validInput)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	publisher := newWithDependencies(
		fixture.store,
		func(value store.OutputSubscription) (compiler.Result, error) {
			calls++
			if calls == 1 {
				current, getErr := fixture.store.GetOutputSubscription(created.SubscriptionID)
				if getErr != nil {
					return compiler.Result{}, getErr
				}
				current.Name = "concurrent edit"
				if _, saveErr := fixture.store.SaveOutputSubscription(current); saveErr != nil {
					return compiler.Result{}, saveErr
				}
			}
			return fixture.compiler.Preview(value)
		},
		fixture.updater.Retry,
		time.Now,
		func() (string, error) { return "unused", nil },
	)
	intent := fixture.validInput
	intent.ID = created.SubscriptionID
	intent.Name = "stale edit"
	if _, err := publisher.Save(intent); !errors.Is(err, ErrConflict) {
		t.Fatalf("save error = %v", err)
	}
	current, err := fixture.store.GetOutputSubscription(created.SubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Name != "concurrent edit" {
		t.Fatalf("concurrent edit was overwritten: %+v", current)
	}
}

func TestSetEnabledPreservesArtifactAndPrecompilesBeforeEnable(t *testing.T) {
	fixture := newPublisherFixture(t)
	created, err := fixture.publisher.Save(fixture.validInput)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.GetSubscriptionArtifact(created.SubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := fixture.publisher.SetEnabled(created.SubscriptionID, false)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || disabled.Revision != before.Revision {
		t.Fatalf("disabled result = %+v", disabled)
	}
	subscription, err := fixture.store.GetOutputSubscription(created.SubscriptionID)
	if err != nil || subscription.Enabled {
		t.Fatalf("disabled subscription = %+v err=%v", subscription, err)
	}
	afterDisable, err := fixture.store.GetSubscriptionArtifact(created.SubscriptionID)
	if err != nil || afterDisable.Revision != before.Revision {
		t.Fatalf("artifact after disable = %+v err=%v", afterDisable, err)
	}
	if _, err := fixture.store.GetSubscriptionUpdate(created.SubscriptionID); err == nil {
		t.Fatal("disabled subscription retained automatic update state")
	}

	edit := fixture.validInput
	edit.ID = created.SubscriptionID
	edit.Name = "edited while disabled"
	edited, err := fixture.publisher.Save(edit)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Enabled {
		t.Fatalf("editing changed enabled state: %+v", edited)
	}
	if _, err := fixture.store.GetSubscriptionUpdate(created.SubscriptionID); err == nil {
		t.Fatal("editing a disabled subscription scheduled automatic work")
	}

	enabled, err := fixture.publisher.SetEnabled(created.SubscriptionID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || enabled.Revision == "" {
		t.Fatalf("enabled result = %+v", enabled)
	}
	update, err := fixture.store.GetSubscriptionUpdate(created.SubscriptionID)
	if err != nil || update.Status != store.SubscriptionUpdateReady {
		t.Fatalf("enabled update = %+v err=%v", update, err)
	}
}

func TestRepublishReturnsOnlyRequestedSubscriptionOutcome(t *testing.T) {
	fixture := newPublisherFixture(t)
	created, err := fixture.publisher.Save(fixture.validInput)
	if err != nil {
		t.Fatal(err)
	}
	publisher := newWithDependencies(
		fixture.store,
		fixture.compiler.Preview,
		func(id int64) (outputupdate.Report, error) {
			return outputupdate.Report{Results: []outputupdate.Result{
				{SubscriptionID: 999, Outcome: OutcomeCompleted},
				{SubscriptionID: id, Outcome: OutcomeDegraded, Error: "synthetic failure"},
			}}, nil
		},
		time.Now,
		func() (string, error) { return "unused", nil },
	)
	result, err := publisher.Republish(created.SubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.SubscriptionID != created.SubscriptionID || result.Outcome != OutcomeDegraded ||
		result.Detail != "synthetic failure" {
		t.Fatalf("republish result = %+v", result)
	}
}

func TestRepublishMapsLifecycleFailureToBlocked(t *testing.T) {
	fixture := newPublisherFixture(t)
	created, err := fixture.publisher.Save(fixture.validInput)
	if err != nil {
		t.Fatal(err)
	}
	publisher := newWithDependencies(
		fixture.store,
		fixture.compiler.Preview,
		func(id int64) (outputupdate.Report, error) {
			generation, markErr := fixture.store.MarkOutputSubscriptionPending(id)
			if markErr != nil {
				return outputupdate.Report{}, markErr
			}
			if _, recordErr := fixture.store.RecordSubscriptionUpdateFailureIfCurrent(
				id, generation, store.SubscriptionFailureLifecycle,
				"source expired", "source expired", nil, time.Time{},
			); recordErr != nil {
				return outputupdate.Report{}, recordErr
			}
			return outputupdate.Report{Results: []outputupdate.Result{{
				SubscriptionID: id, Outcome: OutcomeDegraded, Error: "source expired",
			}}}, nil
		},
		time.Now,
		func() (string, error) { return "unused", nil },
	)
	result, err := publisher.Republish(created.SubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeBlocked {
		t.Fatalf("republish result = %+v", result)
	}
}

func TestRepublishConcurrentDisableReturnsStale(t *testing.T) {
	fixture := newPublisherFixture(t)
	created, err := fixture.publisher.Save(fixture.validInput)
	if err != nil {
		t.Fatal(err)
	}
	publisher := newWithDependencies(
		fixture.store,
		fixture.compiler.Preview,
		func(id int64) (outputupdate.Report, error) {
			current, getErr := fixture.store.GetOutputSubscription(id)
			if getErr != nil {
				return outputupdate.Report{}, getErr
			}
			if disableErr := fixture.store.DisableOutputSubscriptionIfCurrent(id, current.RecordVersion); disableErr != nil {
				return outputupdate.Report{}, disableErr
			}
			return outputupdate.Report{}, errors.New("subscription became disabled")
		},
		time.Now,
		func() (string, error) { return "unused", nil },
	)
	result, err := publisher.Republish(created.SubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.SubscriptionID != created.SubscriptionID || result.Outcome != OutcomeStale {
		t.Fatalf("republish result = %+v", result)
	}
}
