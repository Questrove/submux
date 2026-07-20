package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// SubscriptionBinding is the ordered node selection for one template slot.
// Node order is intentionally preserved because it affects generated client
// configuration and is controlled by the transfer-list UI.
type SubscriptionBinding struct {
	Slot    string  `json:"slot"`
	NodeIDs []int64 `json:"node_ids"`
}

// OutputSubscription is a generated, shareable configuration. It owns the
// template choice and ordered node selection, and Mihomo subscriptions refer
// to a shared RuleProfile for routing behavior. There is no separate node-set
// workflow in the product model.
type OutputSubscription struct {
	ID                int64                 `json:"id"`
	Name              string                `json:"name"`
	TemplateVersionID int64                 `json:"template_version_id"`
	RuleProfileID     int64                 `json:"rule_profile_id,omitempty"`
	Engine            string                `json:"engine"`
	Bindings          []SubscriptionBinding `json:"bindings"`
	Token             string                `json:"token"`
	Enabled           bool                  `json:"enabled"`
	ExpiresAt         string                `json:"expires_at,omitempty"`
	CreatedAt         string                `json:"created_at"`
	UpdatedAt         string                `json:"updated_at"`
}

type SubscriptionArtifact struct {
	SubscriptionID int64    `json:"subscription_id"`
	Body           []byte   `json:"body,omitempty"`
	ContentType    string   `json:"content_type,omitempty"`
	Revision       string   `json:"revision,omitempty"`
	LastSuccess    string   `json:"last_success,omitempty"`
	LastError      string   `json:"last_error,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
	BlockedReason  string   `json:"blocked_reason,omitempty"`
	UpdatedAt      string   `json:"updated_at"`
}

func (s *Store) SaveOutputSubscription(value OutputSubscription) (int64, error) {
	var id int64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("subscriptions"))
		now, created, oldToken := nowRFC3339(), "", ""
		if value.ID == 0 {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			id = int64(seq)
		} else {
			id = value.ID
			raw := b.Get(itob(id))
			if raw == nil {
				return fmt.Errorf("no output subscription with id %d", id)
			}
			var old OutputSubscription
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			created, oldToken = old.CreatedAt, old.Token
		}
		value.Name = strings.TrimSpace(value.Name)
		value.Bindings = normalizeSubscriptionBindings(value.Bindings)
		if value.Token == "" {
			return fmt.Errorf("output subscription token is required")
		}
		index := tx.Bucket([]byte("token_index"))
		if owner := index.Get([]byte(value.Token)); owner != nil && !bytes.Equal(owner, itob(id)) {
			return fmt.Errorf("output subscription token already exists")
		}
		if oldToken != "" && oldToken != value.Token {
			if err := index.Delete([]byte(oldToken)); err != nil {
				return err
			}
		}
		value.ID, value.UpdatedAt = id, now
		if created == "" {
			value.CreatedAt = now
		} else {
			value.CreatedAt = created
		}
		if err := putJSON(b, itob(id), value); err != nil {
			return err
		}
		return index.Put([]byte(value.Token), itob(id))
	})
	return id, err
}

func (s *Store) GetOutputSubscription(id int64) (OutputSubscription, error) {
	var value OutputSubscription
	err := getJSONByID(s.db, "subscriptions", id, &value)
	return value, err
}

func (s *Store) GetOutputSubscriptionByToken(token string) (OutputSubscription, error) {
	var value OutputSubscription
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("token_index")).Get([]byte(token))
		if id == nil {
			return fmt.Errorf("output subscription token not found")
		}
		raw := tx.Bucket([]byte("subscriptions")).Get(id)
		if raw == nil {
			return fmt.Errorf("output subscription token index is stale")
		}
		return json.Unmarshal(raw, &value)
	})
	return value, err
}

func (s *Store) ListOutputSubscriptions() ([]OutputSubscription, error) {
	out := make([]OutputSubscription, 0)
	err := listJSON(s.db, "subscriptions", func(raw []byte) error {
		var value OutputSubscription
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		out = append(out, value)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func (s *Store) DeleteOutputSubscription(id int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("subscriptions"))
		raw := b.Get(itob(id))
		if raw == nil {
			return fmt.Errorf("no output subscription with id %d", id)
		}
		var value OutputSubscription
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("token_index")).Delete([]byte(value.Token)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("subscription_artifacts")).Delete(itob(id)); err != nil {
			return err
		}
		return b.Delete(itob(id))
	})
}

func (s *Store) PutSubscriptionArtifact(value SubscriptionArtifact) error {
	value.UpdatedAt = nowRFC3339()
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket([]byte("subscription_artifacts")), itob(value.SubscriptionID), value)
	})
}

func (s *Store) GetSubscriptionArtifact(subscriptionID int64) (SubscriptionArtifact, error) {
	var value SubscriptionArtifact
	err := getJSONByID(s.db, "subscription_artifacts", subscriptionID, &value)
	return value, err
}

func normalizeSubscriptionBindings(values []SubscriptionBinding) []SubscriptionBinding {
	seenSlots := map[string]bool{}
	out := make([]SubscriptionBinding, 0, len(values))
	for _, value := range values {
		value.Slot = strings.TrimSpace(value.Slot)
		if value.Slot == "" || seenSlots[value.Slot] {
			continue
		}
		seenSlots[value.Slot] = true
		seenNodes := map[int64]bool{}
		nodeIDs := make([]int64, 0, len(value.NodeIDs))
		for _, nodeID := range value.NodeIDs {
			if nodeID > 0 && !seenNodes[nodeID] {
				seenNodes[nodeID] = true
				nodeIDs = append(nodeIDs, nodeID)
			}
		}
		value.NodeIDs = nodeIDs
		out = append(out, value)
	}
	return out
}
