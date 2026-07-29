package runtimestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

const (
	MaxRetainedEvents     = 10_000
	MaxRetainedOperations = 2_000
	DefaultOperationTTL   = 30 * 24 * time.Hour
	DefaultEventBatchSize = 256
	MaxEventBatchSize     = 1_000
)

var (
	ErrCursorExpired      = errors.New("Runtime event cursor has expired")
	ErrInvalidEventCursor = errors.New("Runtime event cursor is ahead of the current state")
)

type CursorExpiredError struct {
	Earliest uint64
}

func (e *CursorExpiredError) Error() string {
	return fmt.Sprintf("Runtime event cursor has expired; earliest available cursor is %d", e.Earliest)
}

func (e *CursorExpiredError) Is(target error) bool {
	return target == ErrCursorExpired
}

func (s *Store) EventsAfter(after uint64, limit int) ([]runtimeapi.Event, uint64, error) {
	if s == nil || s.db == nil {
		return nil, 0, errors.New("Runtime state is not open")
	}
	if limit <= 0 {
		limit = DefaultEventBatchSize
	}
	if limit > MaxEventBatchSize {
		return nil, 0, errors.New("Runtime event batch size exceeds the hard limit")
	}

	var events []runtimeapi.Event
	var earliest uint64
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		bucket := transaction.Bucket(eventsBucket)
		if metadata == nil || bucket == nil {
			return errors.New("Runtime event state is unavailable")
		}
		latest, err := readUint64(metadata.Get(eventCursorKey))
		if err != nil {
			return err
		}
		if after > latest {
			return ErrInvalidEventCursor
		}
		earliest, err = earliestContinuousEventCursor(bucket, latest)
		if err != nil {
			return err
		}
		if earliest > 1 && after < earliest-1 {
			return &CursorExpiredError{Earliest: earliest}
		}
		if after == latest {
			return nil
		}

		cursor := bucket.Cursor()
		key, value := cursor.Seek(encodeUint64(after + 1))
		for key != nil && len(events) < limit {
			eventCursor, err := readUint64(key)
			if err != nil {
				return errors.New("Runtime event key is invalid")
			}
			if eventCursor < earliest {
				key, value = cursor.Next()
				continue
			}
			var event runtimeapi.Event
			if err := json.Unmarshal(value, &event); err != nil || event.Cursor != eventCursor {
				return errors.New("Runtime event record is invalid")
			}
			events = append(events, event)
			key, value = cursor.Next()
		}
		return nil
	})
	return events, earliest, err
}

func (s *Store) GCOperationHistory(now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("Runtime state is not open")
	}
	now = now.UTC()
	removed := 0
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		requests := transaction.Bucket(requestsBucket)
		cancels := transaction.Bucket(cancelRequestsBucket)
		if operations == nil || requests == nil || cancels == nil {
			return errors.New("Runtime operation state is unavailable")
		}

		type entry struct {
			operation runtimeapi.Operation
		}
		var entries []entry
		if err := operations.ForEach(func(_, value []byte) error {
			var operation runtimeapi.Operation
			if err := json.Unmarshal(value, &operation); err != nil {
				return errors.New("Runtime operation record is invalid")
			}
			entries = append(entries, entry{operation: operation})
			return nil
		}); err != nil {
			return err
		}
		protected, err := protectedOperationIDs(operations)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(left, right int) bool {
			if entries[left].operation.UpdatedAt.Equal(entries[right].operation.UpdatedAt) {
				return entries[left].operation.ID < entries[right].operation.ID
			}
			return entries[left].operation.UpdatedAt.Before(entries[right].operation.UpdatedAt)
		})

		cutoff := now.Add(-DefaultOperationTTL)
		total := len(entries)
		deleted := make(map[string]struct{})
		for _, value := range entries {
			operation := value.operation
			if _, keep := protected[operation.ID]; keep || !isTerminalOperationState(operation.State) {
				continue
			}
			if !operation.UpdatedAt.Before(cutoff) && total <= MaxRetainedOperations {
				continue
			}
			if err := operations.Delete([]byte(operation.ID)); err != nil {
				return err
			}
			deleted[operation.ID] = struct{}{}
			total--
			removed++
		}
		if err := deleteRequestRecords(requests, deleted); err != nil {
			return err
		}
		if err := deleteRequestRecords(cancels, deleted); err != nil {
			return err
		}
		return pruneEventHistory(transaction)
	})
	return removed, err
}

func earliestContinuousEventCursor(bucket *bbolt.Bucket, latest uint64) (uint64, error) {
	if latest == 0 {
		return 1, nil
	}
	cursor := bucket.Cursor()
	key, _ := cursor.Last()
	if key == nil {
		return latest + 1, nil
	}
	current, err := readUint64(key)
	if err != nil || current != latest {
		return 0, errors.New("Runtime event cursor metadata does not match retained events")
	}
	earliest := current
	for {
		key, _ = cursor.Prev()
		if key == nil {
			break
		}
		previous, err := readUint64(key)
		if err != nil {
			return 0, errors.New("Runtime event key is invalid")
		}
		if previous+1 != earliest {
			break
		}
		earliest = previous
	}
	return earliest, nil
}

func pruneEventHistory(transaction *bbolt.Tx) error {
	events := transaction.Bucket(eventsBucket)
	operations := transaction.Bucket(operationsBucket)
	if events == nil || operations == nil {
		return errors.New("Runtime event state is unavailable")
	}
	count := 0
	if err := events.ForEach(func(_, _ []byte) error {
		count++
		return nil
	}); err != nil {
		return err
	}
	if count <= MaxRetainedEvents {
		return nil
	}
	protected, err := protectedOperationIDs(operations)
	if err != nil {
		return err
	}
	var keysToRemove [][]byte
	cursor := events.Cursor()
	for key, value := cursor.First(); key != nil && count > MaxRetainedEvents; key, value = cursor.Next() {
		var event runtimeapi.Event
		if err := json.Unmarshal(value, &event); err != nil {
			return errors.New("Runtime event record is invalid")
		}
		if _, keep := protected[event.OperationID]; keep && event.OperationID != "" {
			continue
		}
		keysToRemove = append(keysToRemove, append([]byte(nil), key...))
		count--
	}
	for _, key := range keysToRemove {
		if err := events.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func protectedOperationIDs(bucket *bbolt.Bucket) (map[string]struct{}, error) {
	protected := make(map[string]struct{})
	var latestDecisive runtimeapi.Operation
	err := bucket.ForEach(func(_, value []byte) error {
		var operation runtimeapi.Operation
		if err := json.Unmarshal(value, &operation); err != nil {
			return errors.New("Runtime operation record is invalid")
		}
		switch operation.State {
		case runtimeapi.OperationQueued, runtimeapi.OperationRunning, runtimeapi.OperationOutcomeUnknown:
			protected[operation.ID] = struct{}{}
		case runtimeapi.OperationFailed, runtimeapi.OperationSucceeded:
			if latestDecisive.ID == "" ||
				operation.UpdatedAt.After(latestDecisive.UpdatedAt) ||
				(operation.UpdatedAt.Equal(latestDecisive.UpdatedAt) && operation.ID > latestDecisive.ID) {
				latestDecisive = operation
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if latestDecisive.State == runtimeapi.OperationFailed {
		// A later successful execution clears this inferred current fault.
		// Cancellation does not, because it never attempted to change the
		// machine state.
		protected[latestDecisive.ID] = struct{}{}
	}
	return protected, nil
}

func deleteRequestRecords(bucket *bbolt.Bucket, deleted map[string]struct{}) error {
	if len(deleted) == 0 {
		return nil
	}
	var keysToRemove [][]byte
	cursor := bucket.Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		var record struct {
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return errors.New("Runtime request record is invalid")
		}
		if _, shouldRemove := deleted[record.OperationID]; !shouldRemove {
			continue
		}
		keysToRemove = append(keysToRemove, append([]byte(nil), key...))
	}
	for _, key := range keysToRemove {
		if err := bucket.Delete(key); err != nil {
			return err
		}
	}
	return nil
}
