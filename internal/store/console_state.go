package store

import (
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

const consoleLifecycleEventLimit = 100

var consoleSettingKeys = []string{
	"base_url",
	"fetch_interval_sec",
	"platform_resource_proxy_mode",
	"platform_resource_proxy_url",
	SharedFakeIPSettingKey,
	"rule_catalog_active_commit",
	"rule_catalog_refresh_state",
}

// ConsoleState is one consistent copy of the persisted state needed to build
// the administrator console. It is an internal read boundary, not an HTTP DTO.
type ConsoleState struct {
	Settings              map[string]string
	Sources               []Source
	Caches                map[int64]Cache
	Nodes                 []NodeRecord
	LifecycleEvents       []LifecycleEvent
	Templates             []Template
	TemplateVersions      []TemplateVersion
	RuleProfiles          []RuleProfile
	OutputSubscriptions   []OutputSubscription
	SubscriptionArtifacts map[int64]SubscriptionArtifact
	SubscriptionUpdates   map[int64]SubscriptionUpdateState
	RuleCatalogRaw        json.RawMessage
}

// ReadConsoleState copies every console record from one bbolt read
// transaction. Callers may safely retain and transform the returned values.
func (s *Store) ReadConsoleState() (ConsoleState, error) {
	state := ConsoleState{
		Settings:              make(map[string]string, len(consoleSettingKeys)),
		Caches:                make(map[int64]Cache),
		SubscriptionArtifacts: make(map[int64]SubscriptionArtifact),
		SubscriptionUpdates:   make(map[int64]SubscriptionUpdateState),
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		settings := tx.Bucket([]byte("settings"))
		for _, key := range consoleSettingKeys {
			if raw := settings.Get([]byte(key)); raw != nil {
				state.Settings[key] = string(raw)
			}
		}

		var err error
		if state.Sources, err = decodeConsoleBucket[Source](tx, "sources"); err != nil {
			return err
		}
		for index := range state.Sources {
			normalizeSourceLifecycle(&state.Sources[index])
		}
		caches, err := decodeConsoleBucket[Cache](tx, "source_cache")
		if err != nil {
			return err
		}
		for _, cache := range caches {
			state.Caches[cache.SourceID] = cache
		}
		if state.Nodes, err = decodeConsoleBucket[NodeRecord](tx, "nodes"); err != nil {
			return err
		}
		if state.LifecycleEvents, err = decodeConsoleBucket[LifecycleEvent](tx, "lifecycle_events"); err != nil {
			return err
		}
		if state.Templates, err = decodeConsoleBucket[Template](tx, "templates"); err != nil {
			return err
		}
		if state.TemplateVersions, err = decodeConsoleBucket[TemplateVersion](tx, "template_versions"); err != nil {
			return err
		}
		if state.RuleProfiles, err = decodeConsoleBucket[RuleProfile](tx, "rule_profiles"); err != nil {
			return err
		}
		if state.OutputSubscriptions, err = decodeConsoleBucket[OutputSubscription](tx, "subscriptions"); err != nil {
			return err
		}
		artifacts, err := decodeConsoleBucket[SubscriptionArtifact](tx, "subscription_artifacts")
		if err != nil {
			return err
		}
		for _, artifact := range artifacts {
			state.SubscriptionArtifacts[artifact.SubscriptionID] = artifact
		}
		updates, err := decodeConsoleBucket[SubscriptionUpdateState](tx, "subscription_updates")
		if err != nil {
			return err
		}
		for _, update := range updates {
			state.SubscriptionUpdates[update.SubscriptionID] = update
		}
		activeCommit := state.Settings["rule_catalog_active_commit"]
		if activeCommit != "" {
			if raw := tx.Bucket([]byte("rule_catalog_snapshots")).Get([]byte(activeCommit)); raw != nil {
				state.RuleCatalogRaw = append(json.RawMessage(nil), raw...)
			}
		}
		return nil
	})
	if err != nil {
		return ConsoleState{}, err
	}
	sortConsoleState(&state)
	return state, nil
}

func decodeConsoleBucket[T any](tx *bolt.Tx, bucketName string) ([]T, error) {
	values := make([]T, 0)
	err := tx.Bucket([]byte(bucketName)).ForEach(func(_, raw []byte) error {
		var value T
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("decode console %s record: %w", bucketName, err)
		}
		values = append(values, value)
		return nil
	})
	return values, err
}

func sortConsoleState(state *ConsoleState) {
	sort.Slice(state.Sources, func(i, j int) bool {
		if state.Sources[i].SortOrder != state.Sources[j].SortOrder {
			return state.Sources[i].SortOrder < state.Sources[j].SortOrder
		}
		return state.Sources[i].ID < state.Sources[j].ID
	})
	sort.Slice(state.Nodes, func(i, j int) bool { return state.Nodes[i].ID < state.Nodes[j].ID })
	sort.Slice(state.LifecycleEvents, func(i, j int) bool {
		return state.LifecycleEvents[i].ID > state.LifecycleEvents[j].ID
	})
	if len(state.LifecycleEvents) > consoleLifecycleEventLimit {
		state.LifecycleEvents = state.LifecycleEvents[:consoleLifecycleEventLimit]
	}
	sort.Slice(state.Templates, func(i, j int) bool { return state.Templates[i].ID < state.Templates[j].ID })
	sort.Slice(state.TemplateVersions, func(i, j int) bool {
		if state.TemplateVersions[i].TemplateID != state.TemplateVersions[j].TemplateID {
			return state.TemplateVersions[i].TemplateID < state.TemplateVersions[j].TemplateID
		}
		if state.TemplateVersions[i].Version != state.TemplateVersions[j].Version {
			return state.TemplateVersions[i].Version < state.TemplateVersions[j].Version
		}
		return state.TemplateVersions[i].ID < state.TemplateVersions[j].ID
	})
	sort.Slice(state.RuleProfiles, func(i, j int) bool {
		return state.RuleProfiles[i].ID < state.RuleProfiles[j].ID
	})
	sort.Slice(state.OutputSubscriptions, func(i, j int) bool {
		return state.OutputSubscriptions[i].ID < state.OutputSubscriptions[j].ID
	})
}
