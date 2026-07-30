package runtimestate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

const PortableStateSchema = 1

var (
	restoreMachineSettingsPendingKey = []byte("restore_machine_settings_pending")
	restoreCompletedAtKey            = []byte("restore_completed_at")
	restoreSourceInstallationIDKey   = []byte("restore_source_installation_id")
)

type PortableState struct {
	Schema                int                     `json:"schema"`
	Complete              bool                    `json:"complete"`
	SourceInstallationID  string                  `json:"source_installation_id"`
	ExportedAt            time.Time               `json:"exported_at"`
	CurrentSourceID       string                  `json:"current_source_id,omitempty"`
	Sources               []PortableSource        `json:"sources"`
	AdvancedOverride      *PortableOverride       `json:"advanced_override,omitempty"`
	ManagedResources      []PortableResource      `json:"managed_resources"`
	MachineSettings       PortableMachineSettings `json:"machine_settings"`
	CurrentConfigRevision string                  `json:"current_config_revision,omitempty"`
	CurrentConfigSHA256   string                  `json:"current_config_sha256,omitempty"`
}

type PortableSource struct {
	Record    SourceRecord `json:"record"`
	Raw       []byte       `json:"raw,omitempty"`
	Candidate []byte       `json:"candidate,omitempty"`
}

type PortableOverride struct {
	Record AdvancedOverrideRecord `json:"record"`
	Body   []byte                 `json:"body,omitempty"`
}

type PortableResource struct {
	Record ManagedResourceRecord `json:"record"`
	Body   []byte                `json:"body,omitempty"`
}

type PortableMachineSettings struct {
	RunMode            string `json:"run_mode"`
	MihomoDesiredState string `json:"mihomo_desired_state"`
}

type RestoreStatus struct {
	MachineSettingsPending bool       `json:"machine_settings_pending"`
	RestoredAt             *time.Time `json:"restored_at,omitempty"`
	SourceInstallationID   string     `json:"source_installation_id,omitempty"`
}

func (s *Store) ExportPortableState(includeSecrets bool, exportedAt time.Time) (PortableState, error) {
	if s == nil || s.db == nil {
		return PortableState{}, errors.New("Runtime state is not open")
	}
	exportedAt = exportedAt.UTC()
	if exportedAt.IsZero() {
		return PortableState{}, errors.New("portable state export time is required")
	}
	result := PortableState{
		Schema:           PortableStateSchema,
		Complete:         includeSecrets,
		ExportedAt:       exportedAt,
		Sources:          []PortableSource{},
		ManagedResources: []PortableResource{},
	}
	var sourceRecords []SourceRecord
	var resourceRecords []ManagedResourceRecord
	var overrideRecord *AdvancedOverrideRecord
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		sources := transaction.Bucket(sourcesBucket)
		resources := transaction.Bucket(managedResourcesBucket)
		if metadata == nil || sources == nil || resources == nil {
			return errors.New("Runtime portable state is unavailable")
		}
		result.SourceInstallationID = string(metadata.Get(installationIDKey))
		result.CurrentSourceID = string(metadata.Get(currentSourceIDKey))
		result.CurrentConfigRevision = string(metadata.Get(currentConfigRevisionKey))
		result.CurrentConfigSHA256 = string(metadata.Get(currentConfigSHA256Key))
		result.MachineSettings = PortableMachineSettings{
			RunMode:            defaultString(string(metadata.Get(runModeKey)), runtimeapi.RunModeUnconfigured),
			MihomoDesiredState: desiredMihomoState(metadata),
		}
		if value := metadata.Get(advancedOverrideKey); value != nil {
			var record AdvancedOverrideRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return errors.New("Runtime advanced override record is invalid")
			}
			if err := validateAdvancedOverrideRecord(record); err != nil {
				return err
			}
			overrideRecord = &record
		}
		if err := sources.ForEach(func(_, value []byte) error {
			var record SourceRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return errors.New("Runtime source record is invalid")
			}
			if err := validateSourceRecord(record, false); err != nil {
				return err
			}
			sourceRecords = append(sourceRecords, record)
			return nil
		}); err != nil {
			return err
		}
		return resources.ForEach(func(_, value []byte) error {
			var record ManagedResourceRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return errors.New("Runtime managed resource record is invalid")
			}
			if err := validateManagedResourceRecord(record); err != nil {
				return err
			}
			resourceRecords = append(resourceRecords, record)
			return nil
		})
	})
	if err != nil {
		return PortableState{}, err
	}
	sort.Slice(sourceRecords, func(left, right int) bool { return sourceRecords[left].ID < sourceRecords[right].ID })
	sort.Slice(resourceRecords, func(left, right int) bool { return resourceRecords[left].ID < resourceRecords[right].ID })
	for _, record := range sourceRecords {
		portable := PortableSource{Record: record}
		if includeSecrets {
			raw, candidate, err := s.ReadSourceRevision(record)
			if err != nil {
				return PortableState{}, fmt.Errorf("export Runtime source %s: %w", record.ID, err)
			}
			portable.Raw = raw
			portable.Candidate = candidate
		} else {
			portable.Record = redactedPortableSource(record)
		}
		result.Sources = append(result.Sources, portable)
	}
	if overrideRecord != nil {
		portable := &PortableOverride{Record: *overrideRecord}
		if includeSecrets {
			body, _, err := s.AdvancedOverride()
			if err != nil {
				return PortableState{}, err
			}
			portable.Body = body
		}
		result.AdvancedOverride = portable
	}
	for _, record := range resourceRecords {
		portable := PortableResource{Record: record}
		if includeSecrets {
			path, err := s.managedResourceObjectPath(record.SHA256)
			if err != nil {
				return PortableState{}, err
			}
			body, err := readVerifiedSourceFile(path, record.SHA256)
			if err != nil || int64(len(body)) != record.Size {
				return PortableState{}, fmt.Errorf("export Runtime managed resource %s: content is unavailable", record.ID)
			}
			portable.Body = body
		}
		result.ManagedResources = append(result.ManagedResources, portable)
	}
	return result, nil
}

func (s *Store) ReplacePortableState(
	state PortableState,
	operationID string,
	now time.Time,
) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	now = now.UTC()
	if operationID == "" || now.IsZero() {
		return errors.New("portable state restore operation and time are required")
	}
	if err := validatePortableState(state); err != nil {
		return err
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()

	for _, source := range state.Sources {
		rawDigest, candidateDigest, revisionKey, _, err := s.writeSourceRevision(
			source.Record.ID,
			source.Raw,
			source.Candidate,
		)
		if err != nil {
			return fmt.Errorf("stage restored Runtime source: %w", err)
		}
		if rawDigest != source.Record.RawSHA256 ||
			candidateDigest != source.Record.CandidateSHA256 ||
			revisionKey != source.Record.RevisionKey {
			return errors.New("restored Runtime source revision does not match its record")
		}
	}
	for _, resource := range state.ManagedResources {
		path, err := s.managedResourceObjectPath(resource.Record.SHA256)
		if err != nil {
			return err
		}
		if err := writeOrVerifyImmutableSourceFile(path, resource.Body, resource.Record.SHA256); err != nil {
			return fmt.Errorf("stage restored Runtime managed resource: %w", err)
		}
	}
	if state.AdvancedOverride != nil {
		path, err := s.advancedOverridePath(state.AdvancedOverride.Record.SHA256)
		if err != nil {
			return err
		}
		if err := writeOrVerifyImmutableSourceFile(
			path,
			state.AdvancedOverride.Body,
			state.AdvancedOverride.Record.SHA256,
		); err != nil {
			return fmt.Errorf("stage restored Runtime advanced override: %w", err)
		}
	}

	return s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		if err := replacePortableBucket(transaction, sourcesBucket, func(bucket *bbolt.Bucket) error {
			for _, source := range state.Sources {
				if err := putJSON(bucket, []byte(source.Record.ID), source.Record); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := replacePortableBucket(transaction, managedResourcesBucket, func(bucket *bbolt.Bucket) error {
			for _, resource := range state.ManagedResources {
				if err := putJSON(bucket, []byte(resource.Record.ID), resource.Record); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if state.CurrentSourceID == "" {
			if err := metadata.Delete(currentSourceIDKey); err != nil {
				return err
			}
		} else if err := metadata.Put(currentSourceIDKey, []byte(state.CurrentSourceID)); err != nil {
			return err
		}
		if state.AdvancedOverride == nil {
			if err := metadata.Delete(advancedOverrideKey); err != nil {
				return err
			}
		} else {
			encoded, err := json.Marshal(state.AdvancedOverride.Record)
			if err != nil {
				return err
			}
			if err := metadata.Put(advancedOverrideKey, encoded); err != nil {
				return err
			}
		}
		if err := recordExplicitMihomoStop(metadata); err != nil {
			return err
		}
		if err := metadata.Put(runModeKey, []byte(runtimeapi.RunModeUnconfigured)); err != nil {
			return err
		}
		for _, key := range [][]byte{currentConfigRevisionKey, currentConfigSHA256Key} {
			if err := metadata.Delete(key); err != nil {
				return err
			}
		}
		if err := metadata.Put(restoreMachineSettingsPendingKey, []byte("true")); err != nil {
			return err
		}
		if err := metadata.Put(restoreCompletedAtKey, []byte(now.Format(time.RFC3339Nano))); err != nil {
			return err
		}
		if state.SourceInstallationID == "" {
			if err := metadata.Delete(restoreSourceInstallationIDKey); err != nil {
				return err
			}
		} else if err := metadata.Put(restoreSourceInstallationIDKey, []byte(state.SourceInstallationID)); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "backup.restored", operationID, now)
		return err
	})
}

func (s *Store) PortableRestoreStatus() (RestoreStatus, error) {
	if s == nil || s.db == nil {
		return RestoreStatus{}, errors.New("Runtime state is not open")
	}
	var status RestoreStatus
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		status.MachineSettingsPending = string(metadata.Get(restoreMachineSettingsPendingKey)) == "true"
		status.SourceInstallationID = string(metadata.Get(restoreSourceInstallationIDKey))
		if value := metadata.Get(restoreCompletedAtKey); value != nil {
			parsed, err := time.Parse(time.RFC3339Nano, string(value))
			if err != nil {
				return errors.New("Runtime restore completion time is invalid")
			}
			parsed = parsed.UTC()
			status.RestoredAt = &parsed
		}
		return nil
	})
	return status, err
}

func portableRestoreSummary(metadata *bbolt.Bucket) (runtimeapi.BackupStatus, error) {
	if metadata == nil {
		return runtimeapi.BackupStatus{}, errors.New("Runtime metadata is unavailable")
	}
	status := runtimeapi.BackupStatus{
		MachineSettingsPending: string(metadata.Get(restoreMachineSettingsPendingKey)) == "true",
		SourceInstallationID:   string(metadata.Get(restoreSourceInstallationIDKey)),
	}
	if value := metadata.Get(restoreCompletedAtKey); value != nil {
		parsed, err := time.Parse(time.RFC3339Nano, string(value))
		if err != nil {
			return runtimeapi.BackupStatus{}, errors.New("Runtime restore completion time is invalid")
		}
		parsed = parsed.UTC()
		status.LastRestoredAt = &parsed
	}
	return status, nil
}

func validatePortableState(state PortableState) error {
	if state.Schema != PortableStateSchema || !state.Complete || state.ExportedAt.IsZero() {
		return errors.New("Runtime portable state is incomplete or uses an unsupported schema")
	}
	sourceIDs := make(map[string]struct{}, len(state.Sources))
	for _, source := range state.Sources {
		if _, duplicate := sourceIDs[source.Record.ID]; duplicate {
			return errors.New("Runtime portable state contains duplicate source IDs")
		}
		sourceIDs[source.Record.ID] = struct{}{}
		if err := validateSourceRecord(source.Record, false); err != nil {
			return err
		}
		if sha256Hex(source.Raw) != strings.ToLower(source.Record.RawSHA256) ||
			sha256Hex(source.Candidate) != strings.ToLower(source.Record.CandidateSHA256) {
			return errors.New("Runtime portable source content does not match its digest")
		}
	}
	if state.CurrentSourceID != "" {
		if _, exists := sourceIDs[state.CurrentSourceID]; !exists {
			return errors.New("Runtime portable current source is unavailable")
		}
	}
	resourceIDs := make(map[string]struct{}, len(state.ManagedResources))
	var resourceBytes int64
	for _, resource := range state.ManagedResources {
		if _, duplicate := resourceIDs[resource.Record.ID]; duplicate {
			return errors.New("Runtime portable state contains duplicate managed resource IDs")
		}
		resourceIDs[resource.Record.ID] = struct{}{}
		if err := validateManagedResourceRecord(resource.Record); err != nil {
			return err
		}
		if int64(len(resource.Body)) != resource.Record.Size ||
			sha256Hex(resource.Body) != strings.ToLower(resource.Record.SHA256) {
			return errors.New("Runtime portable managed resource content does not match its record")
		}
		resourceBytes += resource.Record.Size
		if resourceBytes > MaxManagedResourcesBytes || len(resourceIDs) > MaxManagedResourceCount {
			return errors.New("Runtime portable managed resources exceed their limits")
		}
	}
	if state.AdvancedOverride != nil {
		if err := validateAdvancedOverrideRecord(state.AdvancedOverride.Record); err != nil {
			return err
		}
		if int64(len(state.AdvancedOverride.Body)) != state.AdvancedOverride.Record.Size ||
			sha256Hex(state.AdvancedOverride.Body) != strings.ToLower(state.AdvancedOverride.Record.SHA256) {
			return errors.New("Runtime portable advanced override content does not match its record")
		}
	}
	if state.CurrentConfigSHA256 != "" {
		decoded, err := hex.DecodeString(state.CurrentConfigSHA256)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("Runtime portable current configuration digest is invalid")
		}
	}
	return nil
}

func replacePortableBucket(
	transaction *bbolt.Tx,
	name []byte,
	populate func(*bbolt.Bucket) error,
) error {
	if transaction.Bucket(name) == nil {
		return errors.New("Runtime portable state bucket is unavailable")
	}
	if err := transaction.DeleteBucket(name); err != nil {
		return err
	}
	bucket, err := transaction.CreateBucket(name)
	if err != nil {
		return err
	}
	return populate(bucket)
}

func redactedPortableSource(record SourceRecord) SourceRecord {
	record.URL = ""
	record.UserAgent = ""
	record.Username = ""
	record.Password = ""
	record.AuthorizedTarget = ""
	record.CustomCAPEM = ""
	record.ETag = ""
	record.LastModified = ""
	record.LastRefreshResult = ""
	record.LastRefreshRoute = ""
	record.FailureClass = ""
	return record
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
