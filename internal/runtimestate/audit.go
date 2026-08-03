package runtimestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprivacy"
)

const (
	MaxRetainedAuditRecords = 2_000
	DefaultAuditTTL         = 30 * 24 * time.Hour
)

func (s *Store) RecordAudit(record runtimeapi.AuditRecord) (runtimeapi.AuditRecord, error) {
	if s == nil || s.db == nil {
		return runtimeapi.AuditRecord{}, errors.New("Runtime state is not open")
	}
	if record.Actor == "" || record.ClientType == "" || record.ClientVersion == "" ||
		record.Action == "" || record.Stage == "" || record.Result == "" || record.At.IsZero() {
		return runtimeapi.AuditRecord{}, errors.New("Runtime audit record is incomplete")
	}
	record = runtimeprivacy.SanitizeAudit(record)
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		return appendAuditRecord(transaction, record)
	})
	return record, err
}

func (s *Store) RecentAudit(limit int) ([]runtimeapi.AuditRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("Runtime state is not open")
	}
	if limit <= 0 || limit > MaxRetainedAuditRecords {
		limit = MaxRetainedAuditRecords
	}
	var records []runtimeapi.AuditRecord
	err := s.db.View(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(auditBucket)
		if bucket == nil {
			return errors.New("Runtime audit state is unavailable")
		}
		cursor := bucket.Cursor()
		for _, value := cursor.Last(); value != nil && len(records) < limit; _, value = cursor.Prev() {
			var record runtimeapi.AuditRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return errors.New("Runtime audit record is invalid")
			}
			records = append(records, runtimeprivacy.SanitizeAudit(record))
		}
		return nil
	})
	return records, err
}

func (s *Store) RecentOperations(limit int) ([]runtimeapi.Operation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("Runtime state is not open")
	}
	if limit <= 0 || limit > MaxRetainedOperations {
		limit = MaxRetainedOperations
	}
	var operations []runtimeapi.Operation
	err := s.db.View(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(operationsBucket)
		if bucket == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		if err := bucket.ForEach(func(_, value []byte) error {
			var operation runtimeapi.Operation
			if err := json.Unmarshal(value, &operation); err != nil {
				return errors.New("Runtime operation record is invalid")
			}
			operations = append(operations, runtimeprivacy.SanitizeOperation(operation))
			return nil
		}); err != nil {
			return err
		}
		sort.Slice(operations, func(left, right int) bool {
			if operations[left].UpdatedAt.Equal(operations[right].UpdatedAt) {
				return operations[left].ID > operations[right].ID
			}
			return operations[left].UpdatedAt.After(operations[right].UpdatedAt)
		})
		if len(operations) > limit {
			operations = operations[:limit]
		}
		return nil
	})
	return operations, err
}

func (s *Store) RecentEvents(limit int) ([]runtimeapi.Event, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("Runtime state is not open")
	}
	if limit <= 0 || limit > MaxRetainedEvents {
		limit = MaxRetainedEvents
	}
	var events []runtimeapi.Event
	err := s.db.View(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(eventsBucket)
		if bucket == nil {
			return errors.New("Runtime event state is unavailable")
		}
		cursor := bucket.Cursor()
		for _, value := cursor.Last(); value != nil && len(events) < limit; _, value = cursor.Prev() {
			var event runtimeapi.Event
			if err := json.Unmarshal(value, &event); err != nil {
				return errors.New("Runtime event record is invalid")
			}
			events = append(events, runtimeprivacy.SanitizeEvent(event))
		}
		return nil
	})
	return events, err
}

func appendAuditRecord(transaction *bbolt.Tx, record runtimeapi.AuditRecord) error {
	metadata := transaction.Bucket(metadataBucket)
	bucket := transaction.Bucket(auditBucket)
	if metadata == nil || bucket == nil {
		return errors.New("Runtime audit state is unavailable")
	}
	cursor, err := readUint64(metadata.Get(auditCursorKey))
	if err != nil {
		return err
	}
	cursor++
	record.ID = fmt.Sprintf("audit_%020d", cursor)
	record.At = record.At.UTC()
	record = runtimeprivacy.SanitizeAudit(record)
	if err := putJSON(bucket, encodeUint64(cursor), record); err != nil {
		return err
	}
	return metadata.Put(auditCursorKey, encodeUint64(cursor))
}

func auditForOperation(
	operation runtimeapi.Operation,
	stage string,
	result string,
	operationError *runtimeapi.ProtocolError,
	now time.Time,
) runtimeapi.AuditRecord {
	return runtimeapi.AuditRecord{
		RequestID:     operation.RequestID,
		OperationID:   operation.ID,
		Actor:         operation.CallerIdentity,
		ClientType:    operation.ClientType,
		ClientVersion: operation.ClientVersion,
		Action:        operation.Action.Kind,
		ObjectID:      auditObjectID(operation.Action),
		Trust:         operation.Action.Params.Trust,
		Stage:         stage,
		Result:        result,
		At:            now,
		Error:         operationError,
	}
}

func auditObjectID(action runtimeapi.Action) string {
	switch {
	case action.Params.TrafficPolicy != "":
		return action.Params.TrafficPolicy
	case action.Params.ConnectionID != "":
		return action.Params.ConnectionID
	case action.Params.ConnectionScope != nil:
		return "current_connection_scope"
	case action.Params.SourceID != "":
		return action.Params.SourceID
	case action.Params.ResourceName != "":
		return action.Params.ResourceName
	case action.Params.PlanID != "":
		return action.Params.PlanID
	default:
		return ""
	}
}

func pruneAuditHistory(transaction *bbolt.Tx, now time.Time) error {
	bucket := transaction.Bucket(auditBucket)
	operations := transaction.Bucket(operationsBucket)
	if bucket == nil || operations == nil {
		return errors.New("Runtime audit state is unavailable")
	}
	protected, err := protectedOperationIDs(operations)
	if err != nil {
		return err
	}
	type entry struct {
		key    []byte
		record runtimeapi.AuditRecord
	}
	var entries []entry
	if err := bucket.ForEach(func(key, value []byte) error {
		var record runtimeapi.AuditRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return errors.New("Runtime audit record is invalid")
		}
		entries = append(entries, entry{key: append([]byte(nil), key...), record: record})
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].record.At.Equal(entries[right].record.At) {
			return string(entries[left].key) < string(entries[right].key)
		}
		return entries[left].record.At.Before(entries[right].record.At)
	})
	cutoff := now.UTC().Add(-DefaultAuditTTL)
	total := len(entries)
	for _, item := range entries {
		if _, keep := protected[item.record.OperationID]; keep && item.record.OperationID != "" {
			continue
		}
		if !item.record.At.Before(cutoff) && total <= MaxRetainedAuditRecords {
			continue
		}
		if err := bucket.Delete(item.key); err != nil {
			return err
		}
		total--
	}
	return nil
}
