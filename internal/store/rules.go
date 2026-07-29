package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

type RuleSelection struct {
	Key    string `json:"key"`
	Action string `json:"action"`
}

type CustomRule struct {
	Type   string `json:"type"`
	Value  string `json:"value"`
	Action string `json:"action"`
}

type RuleProfile struct {
	ID             int64           `json:"id"`
	Key            string          `json:"key,omitempty"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	Builtin        bool            `json:"builtin"`
	Rules          []RuleSelection `json:"rules"`
	CustomRules    []CustomRule    `json:"custom_rules"`
	FallbackAction string          `json:"fallback_action"`
	CatalogCommit  string          `json:"catalog_commit"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

func (s *Store) SaveRuleProfile(value RuleProfile) (int64, error) {
	var id int64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("rule_profiles"))
		id = value.ID
		createdAt := ""
		if id == 0 {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			id = int64(seq)
		} else {
			raw := b.Get(itob(id))
			if raw == nil {
				return fmt.Errorf("no rule_profiles record with id %d", id)
			}
			var current RuleProfile
			if err := json.Unmarshal(raw, &current); err != nil {
				return err
			}
			createdAt = current.CreatedAt
		}
		now := nowRFC3339()
		value.ID, value.UpdatedAt = id, now
		value.Key = strings.TrimSpace(value.Key)
		value.Name = strings.TrimSpace(value.Name)
		value.Description = strings.TrimSpace(value.Description)
		value.CatalogCommit = strings.TrimSpace(value.CatalogCommit)
		value.Rules = normalizeRuleSelections(value.Rules)
		value.CustomRules = normalizeCustomRules(value.CustomRules)
		if createdAt == "" {
			value.CreatedAt = now
		} else {
			value.CreatedAt = createdAt
		}
		if err := putJSON(b, itob(id), value); err != nil {
			return err
		}
		return invalidateOutputSubscriptionsTx(tx, func(subscription OutputSubscription) bool {
			return subscription.RuleProfileID == id
		})
	})
	return id, err
}

func (s *Store) GetRuleProfile(id int64) (RuleProfile, error) {
	var value RuleProfile
	err := getJSONByID(s.db, "rule_profiles", id, &value)
	return value, err
}

func (s *Store) GetRuleProfileByKey(key string) (RuleProfile, error) {
	var result RuleProfile
	err := listJSON(s.db, "rule_profiles", func(raw []byte) error {
		var value RuleProfile
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Key == key {
			result = value
		}
		return nil
	})
	if err != nil {
		return RuleProfile{}, err
	}
	if result.ID == 0 {
		return RuleProfile{}, fmt.Errorf("no rule profile with key %q", key)
	}
	return result, nil
}

func (s *Store) ListRuleProfiles() ([]RuleProfile, error) {
	out := make([]RuleProfile, 0)
	err := listJSON(s.db, "rule_profiles", func(raw []byte) error {
		var value RuleProfile
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		out = append(out, value)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func (s *Store) DeleteRuleProfile(id int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("rule_profiles"))
		raw := bucket.Get(itob(id))
		if raw == nil {
			return &resourceDeletionNotFoundError{resourceKind: resourceKindRuleProfile, resourceID: id}
		}
		var profile RuleProfile
		if err := json.Unmarshal(raw, &profile); err != nil {
			return err
		}
		if err := requireResourceDeletionAllowedTx(tx, resourceKindRuleProfile, id, profile.Builtin); err != nil {
			return err
		}
		if err := bumpOutputInputsGenerationTx(tx); err != nil {
			return err
		}
		return bucket.Delete(itob(id))
	})
}

func normalizeRuleSelections(values []RuleSelection) []RuleSelection {
	seen := map[string]bool{}
	out := make([]RuleSelection, 0, len(values))
	for _, value := range values {
		value.Key = strings.TrimSpace(value.Key)
		value.Action = strings.TrimSpace(value.Action)
		if value.Key == "" || seen[value.Key] {
			continue
		}
		seen[value.Key] = true
		out = append(out, value)
	}
	return out
}

func normalizeCustomRules(values []CustomRule) []CustomRule {
	out := make([]CustomRule, 0, len(values))
	for _, value := range values {
		value.Type = strings.ToUpper(strings.TrimSpace(value.Type))
		value.Value = strings.TrimSpace(value.Value)
		value.Action = strings.TrimSpace(value.Action)
		if value.Type != "" && value.Value != "" {
			out = append(out, value)
		}
	}
	return out
}
