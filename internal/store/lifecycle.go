package store

import (
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

type LifecycleEvent struct {
	ID        int64  `json:"id"`
	SourceID  int64  `json:"source_id"`
	FromState string `json:"from_state,omitempty"`
	ToState   string `json:"to_state"`
	CreatedAt string `json:"created_at"`
}

// RecordLifecycleState persists the latest entitlement state and appends an
// event only for subsequent transitions. The initial observation establishes
// a baseline without generating a misleading alert.
func (s *Store) RecordLifecycleState(sourceID int64, state string) (bool, error) {
	changed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		key := []byte(fmt.Sprintf("lifecycle_state:%d", sourceID))
		previous := string(meta.Get(key))
		if previous == state {
			return nil
		}
		if err := meta.Put(key, []byte(state)); err != nil {
			return err
		}
		if previous == "" {
			return nil
		}
		b := tx.Bucket([]byte("lifecycle_events"))
		sequence, err := b.NextSequence()
		if err != nil {
			return err
		}
		changed = true
		return putJSON(b, itob(int64(sequence)), LifecycleEvent{
			ID: int64(sequence), SourceID: sourceID, FromState: previous,
			ToState: state, CreatedAt: nowRFC3339(),
		})
	})
	return changed, err
}

func (s *Store) ListLifecycleEvents(limit int) ([]LifecycleEvent, error) {
	out := make([]LifecycleEvent, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("lifecycle_events")).ForEach(func(_, raw []byte) error {
			var value LifecycleEvent
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			out = append(out, value)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}
