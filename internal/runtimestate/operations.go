package runtimestate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprivacy"
)

var (
	operationsBucket     = []byte("runtime_operations")
	requestsBucket       = []byte("runtime_requests")
	cancelRequestsBucket = []byte("runtime_cancel_requests")
	importsBucket        = []byte("runtime_imports")
	eventsBucket         = []byte("runtime_events")
	auditBucket          = []byte("runtime_audit")
)

var (
	ErrRevisionConflict  = errors.New("Runtime Snapshot revision has changed")
	ErrRequestConflict   = errors.New("Runtime request ID is already used by another request")
	ErrBusy              = errors.New("Runtime operation queue is full")
	ErrOperationNotFound = errors.New("Runtime operation was not found")
	ErrNotCancellable    = errors.New("Runtime operation is not cancellable")
	ErrOperationTerminal = errors.New("Runtime operation is already terminal")
)

type RevisionConflictError struct {
	Current uint64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("Runtime Snapshot revision is %d", e.Current)
}

func (e *RevisionConflictError) Is(target error) bool {
	return target == ErrRevisionConflict
}

type requestRecord struct {
	OperationID string `json:"operation_id"`
	Caller      string `json:"caller"`
	Fingerprint string `json:"fingerprint"`
}

type cancelRequestRecord struct {
	OperationID string `json:"operation_id"`
	Caller      string `json:"caller"`
	Fingerprint string `json:"fingerprint"`
}

func createRuntimeBuckets(transaction *bbolt.Tx) error {
	for _, name := range [][]byte{
		operationsBucket,
		requestsBucket,
		cancelRequestsBucket,
		importsBucket,
		eventsBucket,
		sourcesBucket,
		managedResourcesBucket,
		auditBucket,
	} {
		if _, err := transaction.CreateBucketIfNotExists(name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SubmitOperation(
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	request runtimeapi.CreateOperationRequest,
	capacity int,
	now time.Time,
) (runtimeapi.Operation, bool, error) {
	if s == nil || s.db == nil {
		return runtimeapi.Operation{}, false, errors.New("Runtime state is not open")
	}
	if capacity <= 0 {
		return runtimeapi.Operation{}, false, errors.New("Runtime operation queue capacity must be positive")
	}
	if request.RequestID == "" || request.Action.Kind == "" || peer.Key() == "" || clientType == "" || clientVersion == "" {
		return runtimeapi.Operation{}, false, errors.New("Runtime operation metadata is incomplete")
	}
	fingerprint, err := operationFingerprint(request)
	if err != nil {
		return runtimeapi.Operation{}, false, err
	}
	operationID, err := randomIdentifier("op_")
	if err != nil {
		return runtimeapi.Operation{}, false, err
	}
	now = now.UTC()
	caller := peer.Key()
	var operation runtimeapi.Operation
	duplicate := false
	err = s.db.Update(func(transaction *bbolt.Tx) error {
		requests := transaction.Bucket(requestsBucket)
		cancels := transaction.Bucket(cancelRequestsBucket)
		operations := transaction.Bucket(operationsBucket)
		metadata := transaction.Bucket(metadataBucket)
		imports := transaction.Bucket(importsBucket)
		if requests == nil || cancels == nil || operations == nil || metadata == nil || imports == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		if cancels.Get([]byte(request.RequestID)) != nil {
			return ErrRequestConflict
		}
		if existing := requests.Get([]byte(request.RequestID)); existing != nil {
			var record requestRecord
			if err := json.Unmarshal(existing, &record); err != nil {
				return errors.New("Runtime request record is invalid")
			}
			if record.Caller != caller || record.Fingerprint != fingerprint {
				return ErrRequestConflict
			}
			value := operations.Get([]byte(record.OperationID))
			if value == nil {
				return errors.New("Runtime idempotency record points to a missing operation")
			}
			if err := json.Unmarshal(value, &operation); err != nil {
				return errors.New("Runtime operation record is invalid")
			}
			duplicate = true
			return nil
		}
		revision, err := readUint64(metadata.Get(revisionKey))
		if err != nil {
			return err
		}
		if request.IfRevision != revision {
			return &RevisionConflictError{Current: revision}
		}
		active, err := countActiveOperations(operations)
		if err != nil {
			return err
		}
		if active >= capacity {
			return ErrBusy
		}
		if actionConsumesImport(request.Action.Kind) {
			content, err := readImport(imports, request.Action.Params.ContentID)
			if err != nil {
				return err
			}
			if err := validateImportForCaller(content, caller, operationID, now); err != nil {
				return err
			}
			content.ReservedBy = operationID
			if err := putJSON(imports, []byte(content.Content.ID), content); err != nil {
				return err
			}
		}
		operation = runtimeapi.Operation{
			ID:             operationID,
			RequestID:      request.RequestID,
			Action:         request.Action,
			State:          runtimeapi.OperationQueued,
			Stage:          "accepted",
			Progress:       0,
			Cancellable:    true,
			CallerIdentity: caller,
			ClientType:     clientType,
			ClientVersion:  clientVersion,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := putJSON(operations, []byte(operation.ID), operation); err != nil {
			return err
		}
		if err := putJSON(requests, []byte(request.RequestID), requestRecord{
			OperationID: operation.ID,
			Caller:      caller,
			Fingerprint: fingerprint,
		}); err != nil {
			return err
		}
		if err := appendAuditRecord(transaction, auditForOperation(operation, "accepted", runtimeapi.OperationQueued, nil, now)); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "operation.queued", operation.ID, now)
		return err
	})
	if err != nil {
		return runtimeapi.Operation{}, false, err
	}
	return operation, duplicate, nil
}

func actionConsumesImport(kind string) bool {
	return kind == runtimeapi.ActionApplyImportedConfig ||
		kind == runtimeapi.ActionAddRemoteSource ||
		kind == runtimeapi.ActionAddManagedResource ||
		kind == runtimeapi.ActionSetAdvancedOverride
}

func (s *Store) GetOperation(id string) (runtimeapi.Operation, error) {
	if s == nil || s.db == nil {
		return runtimeapi.Operation{}, errors.New("Runtime state is not open")
	}
	var operation runtimeapi.Operation
	err := s.db.View(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(operationsBucket)
		if bucket == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		value := bucket.Get([]byte(id))
		if value == nil {
			return ErrOperationNotFound
		}
		if err := json.Unmarshal(value, &operation); err != nil {
			return errors.New("Runtime operation record is invalid")
		}
		return nil
	})
	return operation, err
}

func (s *Store) BeginNextOperation(now time.Time) (runtimeapi.Operation, bool, error) {
	if s == nil || s.db == nil {
		return runtimeapi.Operation{}, false, errors.New("Runtime state is not open")
	}
	now = now.UTC()
	var selected runtimeapi.Operation
	found := false
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		metadata := transaction.Bucket(metadataBucket)
		if operations == nil || metadata == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		var queued []runtimeapi.Operation
		if err := operations.ForEach(func(_, value []byte) error {
			var operation runtimeapi.Operation
			if err := json.Unmarshal(value, &operation); err != nil {
				return errors.New("Runtime operation record is invalid")
			}
			if operation.State == runtimeapi.OperationQueued {
				queued = append(queued, operation)
			}
			return nil
		}); err != nil {
			return err
		}
		if len(queued) == 0 {
			return nil
		}
		sort.Slice(queued, func(left, right int) bool {
			if queued[left].CreatedAt.Equal(queued[right].CreatedAt) {
				return queued[left].ID < queued[right].ID
			}
			return queued[left].CreatedAt.Before(queued[right].CreatedAt)
		})
		selected = queued[0]
		selected.State = runtimeapi.OperationRunning
		selected.Stage = "preparing"
		selected.Progress = 1
		selected.Cancellable = true
		selected.UpdatedAt = now
		if err := putJSON(operations, []byte(selected.ID), selected); err != nil {
			return err
		}
		if err := metadata.Put(currentOperationKey, []byte(selected.ID)); err != nil {
			return err
		}
		if err := appendAuditRecord(transaction, auditForOperation(selected, "preparing", runtimeapi.OperationRunning, nil, now)); err != nil {
			return err
		}
		if _, err := advanceRevisionAndEvent(transaction, "operation.running", selected.ID, now); err != nil {
			return err
		}
		found = true
		return nil
	})
	return selected, found, err
}

func (s *Store) UpdateOperationStage(id, stage string, progress int, cancellable bool, now time.Time) error {
	if stage == "" || progress < 0 || progress > 100 {
		return errors.New("Runtime operation stage is invalid")
	}
	now = now.UTC()
	return s.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		if operations == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		operation, err := readOperation(operations, id)
		if err != nil {
			return err
		}
		if operation.State == runtimeapi.OperationCancelled {
			return ErrOperationTerminal
		}
		if operation.State != runtimeapi.OperationRunning {
			return ErrOperationTerminal
		}
		if !operation.Cancellable && cancellable {
			return ErrNotCancellable
		}
		operation.Stage = stage
		operation.Progress = progress
		operation.Cancellable = cancellable
		operation.UpdatedAt = now
		if err := putJSON(operations, []byte(id), operation); err != nil {
			return err
		}
		if err := appendAuditRecord(transaction, auditForOperation(operation, stage, runtimeapi.OperationRunning, nil, now)); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "operation.progress", id, now)
		return err
	})
}

func (s *Store) CompleteOperation(
	id string,
	state string,
	result *runtimeapi.OperationResult,
	operationError *runtimeapi.ProtocolError,
	now time.Time,
) error {
	if !isTerminalOperationState(state) {
		return errors.New("Runtime operation completion state is invalid")
	}
	now = now.UTC()
	return s.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		metadata := transaction.Bucket(metadataBucket)
		if operations == nil || metadata == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		operation, err := readOperation(operations, id)
		if err != nil {
			return err
		}
		if operation.State == runtimeapi.OperationCancelled {
			return ErrOperationTerminal
		}
		if operation.State != runtimeapi.OperationRunning {
			return ErrOperationTerminal
		}
		operation.State = state
		operation.Stage = terminalStage(state)
		operation.Progress = 100
		operation.Cancellable = false
		operation.UpdatedAt = now
		operation.Result = result
		if operationError != nil {
			copy := *operationError
			copy.Message = runtimeprivacy.RedactText(copy.Message)
			operationError = &copy
		}
		operation.Error = operationError
		if err := putJSON(operations, []byte(id), operation); err != nil {
			return err
		}
		if string(metadata.Get(currentOperationKey)) == id {
			if err := metadata.Delete(currentOperationKey); err != nil {
				return err
			}
		}
		if state == runtimeapi.OperationSucceeded {
			previousConfigSHA256 := string(metadata.Get(currentConfigSHA256Key))
			switch operation.Action.Kind {
			case runtimeapi.ActionApplyImportedConfig, runtimeapi.ActionApplySource, runtimeapi.ActionSwitchSource:
				mihomoState := "stopped"
				if result != nil && result.Verified {
					mihomoState = "running"
				}
				if err := metadata.Put(mihomoStateKey, []byte(mihomoState)); err != nil {
					return err
				}
				if err := metadata.Put(runModeKey, []byte("explicit")); err != nil {
					return err
				}
			case runtimeapi.ActionStartProxy:
				if err := recordExplicitMihomoStart(metadata); err != nil {
					return err
				}
				if err := metadata.Put(runModeKey, []byte("explicit")); err != nil {
					return err
				}
			case runtimeapi.ActionStopProxy:
				if err := recordExplicitMihomoStop(metadata); err != nil {
					return err
				}
				if err := metadata.Put(runModeKey, []byte("explicit")); err != nil {
					return err
				}
			}
			if result != nil && result.ConfigRevision != "" {
				if err := metadata.Put(currentConfigRevisionKey, []byte(result.ConfigRevision)); err != nil {
					return err
				}
			}
			if result != nil && result.CandidateSHA256 != "" {
				if err := metadata.Put(currentConfigSHA256Key, []byte(result.CandidateSHA256)); err != nil {
					return err
				}
				if err := clearMihomoRecoveryForNewConfig(
					metadata,
					previousConfigSHA256,
					result.CandidateSHA256,
				); err != nil {
					return err
				}
			}
		}
		if err := appendAuditRecord(transaction, auditForOperation(operation, operation.Stage, state, operationError, now)); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "operation."+state, id, now)
		return err
	})
}

func (s *Store) CancelOperation(
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	targetID string,
	request runtimeapi.CancelOperationRequest,
	now time.Time,
) (runtimeapi.Operation, bool, error) {
	if s == nil || s.db == nil {
		return runtimeapi.Operation{}, false, errors.New("Runtime state is not open")
	}
	if targetID == "" || request.RequestID == "" || peer.Key() == "" || clientType == "" || clientVersion == "" {
		return runtimeapi.Operation{}, false, errors.New("Runtime cancellation metadata is incomplete")
	}
	fingerprintValue, err := json.Marshal(struct {
		TargetID   string `json:"target_id"`
		IfRevision uint64 `json:"if_revision"`
	}{TargetID: targetID, IfRevision: request.IfRevision})
	if err != nil {
		return runtimeapi.Operation{}, false, err
	}
	fingerprintHash := sha256.Sum256(fingerprintValue)
	fingerprint := hex.EncodeToString(fingerprintHash[:])
	now = now.UTC()
	caller := peer.Key()
	var operation runtimeapi.Operation
	duplicate := false
	err = s.db.Update(func(transaction *bbolt.Tx) error {
		cancels := transaction.Bucket(cancelRequestsBucket)
		requests := transaction.Bucket(requestsBucket)
		operations := transaction.Bucket(operationsBucket)
		metadata := transaction.Bucket(metadataBucket)
		if cancels == nil || requests == nil || operations == nil || metadata == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		if requests.Get([]byte(request.RequestID)) != nil {
			return ErrRequestConflict
		}
		if existing := cancels.Get([]byte(request.RequestID)); existing != nil {
			var record cancelRequestRecord
			if err := json.Unmarshal(existing, &record); err != nil {
				return errors.New("Runtime cancellation record is invalid")
			}
			if record.Caller != caller || record.Fingerprint != fingerprint || record.OperationID != targetID {
				return ErrRequestConflict
			}
			var err error
			operation, err = readOperation(operations, targetID)
			if err != nil {
				return err
			}
			duplicate = true
			return nil
		}
		revision, err := readUint64(metadata.Get(revisionKey))
		if err != nil {
			return err
		}
		if request.IfRevision != revision {
			return &RevisionConflictError{Current: revision}
		}
		operation, err = readOperation(operations, targetID)
		if err != nil {
			return err
		}
		if operation.State != runtimeapi.OperationQueued &&
			!(operation.State == runtimeapi.OperationRunning && operation.Cancellable) {
			return ErrNotCancellable
		}
		operation.State = runtimeapi.OperationCancelled
		operation.Stage = "cancelled"
		operation.Progress = 100
		operation.Cancellable = false
		operation.CancelledBy = caller
		operation.UpdatedAt = now
		if err := putJSON(operations, []byte(targetID), operation); err != nil {
			return err
		}
		if string(metadata.Get(currentOperationKey)) == targetID {
			if err := metadata.Delete(currentOperationKey); err != nil {
				return err
			}
		}
		if err := putJSON(cancels, []byte(request.RequestID), cancelRequestRecord{
			OperationID: targetID,
			Caller:      caller,
			Fingerprint: fingerprint,
		}); err != nil {
			return err
		}
		if err := appendAuditRecord(transaction, runtimeapi.AuditRecord{
			RequestID:     request.RequestID,
			OperationID:   targetID,
			Actor:         caller,
			ClientType:    clientType,
			ClientVersion: clientVersion,
			Action:        "operation.cancel",
			ObjectID:      targetID,
			Stage:         "cancelled",
			Result:        runtimeapi.OperationCancelled,
			At:            now,
		}); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "operation.cancelled", targetID, now)
		return err
	})
	if err != nil {
		return runtimeapi.Operation{}, false, err
	}
	return operation, duplicate, nil
}

func (s *Store) RecoverOperations(now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	now = now.UTC()
	return s.db.Update(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		metadata := transaction.Bucket(metadataBucket)
		if operations == nil || metadata == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		var interrupted []runtimeapi.Operation
		if err := operations.ForEach(func(_, value []byte) error {
			var operation runtimeapi.Operation
			if err := json.Unmarshal(value, &operation); err != nil {
				return errors.New("Runtime operation record is invalid")
			}
			if operation.State == runtimeapi.OperationRunning {
				interrupted = append(interrupted, operation)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, operation := range interrupted {
			operation.State = runtimeapi.OperationOutcomeUnknown
			operation.Stage = "recovery_required"
			operation.Progress = 100
			operation.Cancellable = false
			operation.UpdatedAt = now
			operation.Error = &runtimeapi.ProtocolError{
				Code:      runtimeapi.OperationOutcomeUnknown,
				Message:   "Runtime stopped while this operation could have external side effects",
				Retryable: false,
			}
			if err := putJSON(operations, []byte(operation.ID), operation); err != nil {
				return err
			}
			if err := appendAuditRecord(transaction, auditForOperation(
				operation,
				"recovery_required",
				runtimeapi.OperationOutcomeUnknown,
				operation.Error,
				now,
			)); err != nil {
				return err
			}
			if _, err := advanceRevisionAndEvent(transaction, "operation.outcome_unknown", operation.ID, now); err != nil {
				return err
			}
		}
		return metadata.Delete(currentOperationKey)
	})
}

func operationSummary(transaction *bbolt.Tx, currentID string) (runtimeapi.OperationStatus, error) {
	bucket := transaction.Bucket(operationsBucket)
	if bucket == nil {
		return runtimeapi.OperationStatus{}, errors.New("Runtime operation state is unavailable")
	}
	status := runtimeapi.OperationStatus{CurrentOperationID: currentID}
	err := bucket.ForEach(func(_, value []byte) error {
		var operation runtimeapi.Operation
		if err := json.Unmarshal(value, &operation); err != nil {
			return errors.New("Runtime operation record is invalid")
		}
		if operation.State == runtimeapi.OperationQueued {
			status.Queued++
		}
		return nil
	})
	return status, err
}

func operationFingerprint(request runtimeapi.CreateOperationRequest) (string, error) {
	value, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:]), nil
}

func countActiveOperations(bucket *bbolt.Bucket) (int, error) {
	count := 0
	err := bucket.ForEach(func(_, value []byte) error {
		var operation runtimeapi.Operation
		if err := json.Unmarshal(value, &operation); err != nil {
			return errors.New("Runtime operation record is invalid")
		}
		if operation.State == runtimeapi.OperationQueued || operation.State == runtimeapi.OperationRunning {
			count++
		}
		return nil
	})
	return count, err
}

func readOperation(bucket *bbolt.Bucket, id string) (runtimeapi.Operation, error) {
	value := bucket.Get([]byte(id))
	if value == nil {
		return runtimeapi.Operation{}, ErrOperationNotFound
	}
	var operation runtimeapi.Operation
	if err := json.Unmarshal(value, &operation); err != nil {
		return runtimeapi.Operation{}, errors.New("Runtime operation record is invalid")
	}
	return operation, nil
}

func advanceRevisionAndEvent(
	transaction *bbolt.Tx,
	eventType string,
	operationID string,
	now time.Time,
) (uint64, error) {
	metadata := transaction.Bucket(metadataBucket)
	events := transaction.Bucket(eventsBucket)
	if metadata == nil || events == nil {
		return 0, errors.New("Runtime event state is unavailable")
	}
	revision, err := readUint64(metadata.Get(revisionKey))
	if err != nil {
		return 0, err
	}
	cursor, err := readUint64(metadata.Get(eventCursorKey))
	if err != nil {
		return 0, err
	}
	revision++
	cursor++
	if err := metadata.Put(revisionKey, encodeUint64(revision)); err != nil {
		return 0, err
	}
	if err := metadata.Put(eventCursorKey, encodeUint64(cursor)); err != nil {
		return 0, err
	}
	event := runtimeapi.Event{
		Cursor:           cursor,
		Type:             eventType,
		At:               now.UTC(),
		OperationID:      operationID,
		SnapshotRevision: revision,
	}
	if err := putJSON(events, encodeUint64(cursor), event); err != nil {
		return 0, err
	}
	if err := pruneEventHistory(transaction); err != nil {
		return 0, err
	}
	return revision, nil
}

func putJSON(bucket *bbolt.Bucket, key []byte, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, encoded)
}

func randomIdentifier(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func isTerminalOperationState(state string) bool {
	switch state {
	case runtimeapi.OperationSucceeded,
		runtimeapi.OperationFailed,
		runtimeapi.OperationCancelled,
		runtimeapi.OperationOutcomeUnknown:
		return true
	default:
		return false
	}
}

func terminalStage(state string) string {
	switch state {
	case runtimeapi.OperationSucceeded:
		return "completed"
	case runtimeapi.OperationFailed:
		return "failed"
	case runtimeapi.OperationCancelled:
		return "cancelled"
	case runtimeapi.OperationOutcomeUnknown:
		return "recovery_required"
	default:
		return "finished"
	}
}
