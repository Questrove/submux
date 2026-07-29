package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

const outputInputsGenerationKey = "output_inputs_generation"

const (
	SubscriptionUpdatePending = "pending"
	SubscriptionUpdateReady   = "ready"
	SubscriptionUpdateBlocked = "blocked"

	SubscriptionFailureTemporary = "temporary"
	SubscriptionFailureConfig    = "config"
	SubscriptionFailureLifecycle = "lifecycle"
)

// SubscriptionUpdateState is durable control-plane state for producing one
// output artifact. It is deliberately stored apart from the last-good artifact.
type SubscriptionUpdateState struct {
	SubscriptionID  int64    `json:"subscription_id"`
	InputGeneration uint64   `json:"input_generation"`
	Status          string   `json:"status"`
	FailureClass    string   `json:"failure_class,omitempty"`
	LastError       string   `json:"last_error,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
	BlockedReason   string   `json:"blocked_reason,omitempty"`
	AttemptCount    int      `json:"attempt_count,omitempty"`
	NextAttemptAt   string   `json:"next_attempt_at,omitempty"`
	LastAttemptAt   string   `json:"last_attempt_at,omitempty"`
	UpdatedAt       string   `json:"updated_at"`
}

type PendingSubscriptionUpdate struct {
	Subscription OutputSubscription
	Update       SubscriptionUpdateState
}

func (s *Store) OutputInputsGeneration() (uint64, error) {
	var generation uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		generation = outputInputsGenerationTx(tx)
		return nil
	})
	return generation, err
}

func outputInputsGenerationTx(tx *bolt.Tx) uint64 {
	raw := tx.Bucket([]byte("meta")).Get([]byte(outputInputsGenerationKey))
	if len(raw) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

func bumpOutputInputsGenerationTx(tx *bolt.Tx) error {
	next := outputInputsGenerationTx(tx) + 1
	raw := make([]byte, 8)
	binary.BigEndian.PutUint64(raw, next)
	return tx.Bucket([]byte("meta")).Put([]byte(outputInputsGenerationKey), raw)
}

func (s *Store) GetSubscriptionUpdate(id int64) (SubscriptionUpdateState, error) {
	var value SubscriptionUpdateState
	err := getJSONByID(s.db, "subscription_updates", id, &value)
	return value, err
}

// ListDueSubscriptionUpdates returns work that may be attempted automatically.
// Configuration and lifecycle failures wait for a relevant write or manual
// retry; temporary failures become due at NextAttemptAt.
func (s *Store) ListDueSubscriptionUpdates(now time.Time) ([]PendingSubscriptionUpdate, error) {
	out := make([]PendingSubscriptionUpdate, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		updates := tx.Bucket([]byte("subscription_updates"))
		subscriptions := tx.Bucket([]byte("subscriptions"))
		return updates.ForEach(func(key, raw []byte) error {
			var update SubscriptionUpdateState
			if err := json.Unmarshal(raw, &update); err != nil {
				return err
			}
			if update.Status != SubscriptionUpdatePending {
				return nil
			}
			due := update.AttemptCount == 0
			if update.FailureClass == SubscriptionFailureTemporary && update.NextAttemptAt != "" {
				at, err := time.Parse(time.RFC3339, update.NextAttemptAt)
				due = err != nil || !at.After(now)
			}
			if !due {
				return nil
			}
			subRaw := subscriptions.Get(key)
			if subRaw == nil {
				return nil
			}
			var subscription OutputSubscription
			if err := json.Unmarshal(subRaw, &subscription); err != nil {
				return err
			}
			if !subscription.Enabled {
				return nil
			}
			out = append(out, PendingSubscriptionUpdate{Subscription: subscription, Update: update})
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Subscription.ID < out[j].Subscription.ID })
	return out, err
}

func (s *Store) MarkOutputSubscriptionPending(id int64) (uint64, error) {
	var generation uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		generation, err = markOutputSubscriptionPendingTx(tx, id)
		return err
	})
	return generation, err
}

func markOutputSubscriptionPendingTx(tx *bolt.Tx, id int64) (uint64, error) {
	raw := tx.Bucket([]byte("subscriptions")).Get(itob(id))
	if raw == nil {
		return 0, fmt.Errorf("no output subscription with id %d", id)
	}
	var subscription OutputSubscription
	if err := json.Unmarshal(raw, &subscription); err != nil {
		return 0, err
	}
	if !subscription.Enabled {
		return 0, fmt.Errorf("output subscription %d is disabled", id)
	}
	var current SubscriptionUpdateState
	if updateRaw := tx.Bucket([]byte("subscription_updates")).Get(itob(id)); updateRaw != nil {
		if err := json.Unmarshal(updateRaw, &current); err != nil {
			return 0, err
		}
	}
	generation := current.InputGeneration + 1
	now := nowRFC3339()
	return generation, putJSON(tx.Bucket([]byte("subscription_updates")), itob(id), SubscriptionUpdateState{
		SubscriptionID:  id,
		InputGeneration: generation,
		Status:          SubscriptionUpdatePending,
		BlockedReason:   current.BlockedReason,
		UpdatedAt:       now,
	})
}

func nextOutputSubscriptionGenerationTx(tx *bolt.Tx, id int64) (uint64, error) {
	var current SubscriptionUpdateState
	if raw := tx.Bucket([]byte("subscription_updates")).Get(itob(id)); raw != nil {
		if err := json.Unmarshal(raw, &current); err != nil {
			return 0, err
		}
	}
	return current.InputGeneration + 1, nil
}

func (s *Store) CommitSubscriptionArtifactIfCurrent(id int64, generation uint64, artifact SubscriptionArtifact, warnings []string) (bool, error) {
	committed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		subRaw := tx.Bucket([]byte("subscriptions")).Get(itob(id))
		if subRaw == nil {
			return nil
		}
		var subscription OutputSubscription
		if err := json.Unmarshal(subRaw, &subscription); err != nil {
			return err
		}
		if !subscription.Enabled {
			return nil
		}
		var update SubscriptionUpdateState
		raw := tx.Bucket([]byte("subscription_updates")).Get(itob(id))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &update); err != nil {
			return err
		}
		if update.InputGeneration != generation || update.Status != SubscriptionUpdatePending {
			return nil
		}
		now := nowRFC3339()
		artifact.SubscriptionID = id
		artifact.UpdatedAt = now
		if artifact.LastSuccess == "" {
			artifact.LastSuccess = now
		}
		if err := putJSON(tx.Bucket([]byte("subscription_artifacts")), itob(id), artifact); err != nil {
			return err
		}
		update.Status = SubscriptionUpdateReady
		update.FailureClass = ""
		update.LastError = ""
		update.BlockedReason = ""
		update.Warnings = append([]string(nil), warnings...)
		update.AttemptCount = 0
		update.NextAttemptAt = ""
		update.LastAttemptAt = now
		update.UpdatedAt = now
		if err := putJSON(tx.Bucket([]byte("subscription_updates")), itob(id), update); err != nil {
			return err
		}
		committed = true
		return nil
	})
	return committed, err
}

func (s *Store) RecordSubscriptionUpdateFailureIfCurrent(id int64, generation uint64, failureClass, message, blockedReason string, warnings []string, nextAttempt time.Time) (bool, error) {
	recorded := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		subRaw := tx.Bucket([]byte("subscriptions")).Get(itob(id))
		if subRaw == nil {
			return nil
		}
		var subscription OutputSubscription
		if err := json.Unmarshal(subRaw, &subscription); err != nil {
			return err
		}
		if !subscription.Enabled {
			return nil
		}
		var update SubscriptionUpdateState
		raw := tx.Bucket([]byte("subscription_updates")).Get(itob(id))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &update); err != nil {
			return err
		}
		if update.InputGeneration != generation || update.Status != SubscriptionUpdatePending {
			return nil
		}
		now := nowRFC3339()
		update.FailureClass = failureClass
		update.LastError = message
		update.BlockedReason = blockedReason
		update.Warnings = append([]string(nil), warnings...)
		update.AttemptCount++
		update.LastAttemptAt = now
		update.UpdatedAt = now
		update.NextAttemptAt = ""
		if failureClass == SubscriptionFailureTemporary && !nextAttempt.IsZero() {
			update.NextAttemptAt = nextAttempt.UTC().Format(time.RFC3339)
		}
		if failureClass == SubscriptionFailureLifecycle {
			update.Status = SubscriptionUpdateBlocked
		}
		if err := putJSON(tx.Bucket([]byte("subscription_updates")), itob(id), update); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	return recorded, err
}

func invalidateOutputSubscriptionsTx(tx *bolt.Tx, matches func(OutputSubscription) bool) error {
	if err := bumpOutputInputsGenerationTx(tx); err != nil {
		return err
	}
	var ids []int64
	if err := tx.Bucket([]byte("subscriptions")).ForEach(func(_, raw []byte) error {
		var subscription OutputSubscription
		if err := json.Unmarshal(raw, &subscription); err != nil {
			return err
		}
		if subscription.Enabled && matches(subscription) {
			ids = append(ids, subscription.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := markOutputSubscriptionPendingTx(tx, id); err != nil {
			return err
		}
	}
	return nil
}

func invalidateOutputSubscriptionsForNodeIDsTx(tx *bolt.Tx, nodeIDs map[int64]bool) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	return invalidateOutputSubscriptionsTx(tx, func(subscription OutputSubscription) bool {
		for _, binding := range subscription.Bindings {
			for _, nodeID := range binding.NodeIDs {
				if nodeIDs[nodeID] {
					return true
				}
			}
		}
		return false
	})
}

func invalidateOutputSubscriptionsForSourceTx(tx *bolt.Tx, sourceID int64) error {
	nodeIDs := map[int64]bool{}
	if err := tx.Bucket([]byte("nodes")).ForEach(func(_, raw []byte) error {
		var node NodeRecord
		if err := json.Unmarshal(raw, &node); err != nil {
			return err
		}
		if node.SourceID == sourceID {
			nodeIDs[node.ID] = true
		}
		return nil
	}); err != nil {
		return err
	}
	return invalidateOutputSubscriptionsForNodeIDsTx(tx, nodeIDs)
}

func invalidateOutputSubscriptionsForTemplateTx(tx *bolt.Tx, templateID int64) error {
	versionIDs := map[int64]bool{}
	if err := tx.Bucket([]byte("template_versions")).ForEach(func(_, raw []byte) error {
		var version TemplateVersion
		if err := json.Unmarshal(raw, &version); err != nil {
			return err
		}
		if version.TemplateID == templateID {
			versionIDs[version.ID] = true
		}
		return nil
	}); err != nil {
		return err
	}
	return invalidateOutputSubscriptionsTx(tx, func(subscription OutputSubscription) bool {
		return versionIDs[subscription.TemplateVersionID]
	})
}
