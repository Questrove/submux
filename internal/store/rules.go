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
	return saveWithID(s.db, "rule_profiles", value.ID, func(id int64, createdAt string) any {
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
		return value
	})
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
		if bucket.Get(itob(id)) == nil {
			return fmt.Errorf("no rule profile with id %d", id)
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
