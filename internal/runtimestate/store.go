package runtimestate

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
	"submux/internal/safepath"
)

var (
	metadataBucket = []byte("runtime_metadata")

	revisionKey              = []byte("revision")
	eventCursorKey           = []byte("event_cursor")
	auditCursorKey           = []byte("audit_cursor")
	installationIDKey        = []byte("installation_id")
	createdAtKey             = []byte("created_at")
	mihomoStateKey           = []byte("mihomo_state")
	mihomoDesiredStateKey    = []byte("mihomo_desired_state")
	mihomoRecoveryStateKey   = []byte("mihomo_recovery_state")
	mihomoCrashAttemptsKey   = []byte("mihomo_crash_attempts")
	mihomoCrashWindowKey     = []byte("mihomo_crash_window_started_at")
	mihomoNextRestartKey     = []byte("mihomo_next_restart_at")
	mihomoStableSinceKey     = []byte("mihomo_stable_since")
	mihomoFaultCodeKey       = []byte("mihomo_fault_code")
	mihomoFaultMessageKey    = []byte("mihomo_fault_message")
	mihomoCoreVersionKey     = []byte("mihomo_core_version")
	runModeKey               = []byte("run_mode")
	currentOperationKey      = []byte("current_operation")
	currentConfigRevisionKey = []byte("current_config_revision")
	currentConfigSHA256Key   = []byte("current_config_sha256")
	schemaVersionKey         = []byte("schema_version")
)

const SchemaVersion = 1

type Store struct {
	db       *bbolt.DB
	root     string
	sourceMu sync.Mutex
}

func Open(root string) (*Store, error) {
	root, err := prepareStateRoot(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "runtime.db")
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open Runtime state: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure Runtime state: %w", err)
	}
	store := &Store{db: db, root: root}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.removeOrphanImportFiles(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SchemaVersion() (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("Runtime state is not open")
	}
	var version uint64
	if err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		var err error
		version, err = readUint64(metadata.Get(schemaVersionKey))
		return err
	}); err != nil {
		return 0, err
	}
	if version != SchemaVersion {
		return 0, fmt.Errorf("unsupported Runtime state schema version %d", version)
	}
	return int(version), nil
}

func (s *Store) Observe(runtimeVersion string, observedAt time.Time) (runtimeapi.Snapshot, error) {
	if s == nil || s.db == nil {
		return runtimeapi.Snapshot{}, errors.New("Runtime state is not open")
	}
	var snapshot runtimeapi.Snapshot
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		revision, err := readUint64(metadata.Get(revisionKey))
		if err != nil {
			return err
		}
		eventCursor, err := readUint64(metadata.Get(eventCursorKey))
		if err != nil {
			return err
		}
		mihomo, err := mihomoLifecycleSummary(metadata)
		if err != nil {
			return err
		}
		runMode := string(metadata.Get(runModeKey))
		if runMode == "" {
			runMode = "unconfigured"
		}
		operations, err := operationSummary(transaction, string(metadata.Get(currentOperationKey)))
		if err != nil {
			return err
		}
		sources, err := sourceSummary(transaction)
		if err != nil {
			return err
		}
		resources, err := managedResourceSummary(transaction)
		if err != nil {
			return err
		}
		override, err := advancedOverrideSummary(transaction)
		if err != nil {
			return err
		}
		backups, err := portableRestoreSummary(metadata)
		if err != nil {
			return err
		}
		snapshot = runtimeapi.Snapshot{
			ProtocolVersion: runtimeapi.ProtocolVersion,
			Revision:        revision,
			Runtime: runtimeapi.RuntimeStatus{
				Version:      runtimeVersion,
				ServiceState: "running",
			},
			Mihomo:            mihomo,
			RunMode:           runMode,
			Sources:           sources,
			Resources:         resources,
			AdvancedOverride:  override,
			Operations:        operations,
			Updates:           runtimeapi.UpdateStatus{},
			Backups:           backups,
			LatestEventCursor: eventCursor,
			ObservedAt:        observedAt.UTC(),
		}
		return nil
	})
	if err != nil {
		return runtimeapi.Snapshot{}, fmt.Errorf("observe Runtime state: %w", err)
	}
	return snapshot, nil
}

func (s *Store) InstallationID() (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("Runtime state is not open")
	}
	var value string
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		value = string(metadata.Get(installationIDKey))
		if value == "" {
			return errors.New("Runtime installation ID is unavailable")
		}
		return nil
	})
	return value, err
}

func (s *Store) initialize() error {
	return s.db.Update(func(transaction *bbolt.Tx) error {
		metadata, err := transaction.CreateBucketIfNotExists(metadataBucket)
		if err != nil {
			return err
		}
		if metadata.Get(revisionKey) == nil {
			if err := metadata.Put(revisionKey, encodeUint64(1)); err != nil {
				return err
			}
		}
		if metadata.Get(schemaVersionKey) == nil {
			if err := metadata.Put(schemaVersionKey, encodeUint64(SchemaVersion)); err != nil {
				return err
			}
		} else {
			version, err := readUint64(metadata.Get(schemaVersionKey))
			if err != nil {
				return err
			}
			if version != SchemaVersion {
				return fmt.Errorf("unsupported Runtime state schema version %d", version)
			}
		}
		if metadata.Get(eventCursorKey) == nil {
			if err := metadata.Put(eventCursorKey, encodeUint64(0)); err != nil {
				return err
			}
		}
		if metadata.Get(auditCursorKey) == nil {
			if err := metadata.Put(auditCursorKey, encodeUint64(0)); err != nil {
				return err
			}
		}
		if metadata.Get(installationIDKey) == nil {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return fmt.Errorf("create Runtime installation ID: %w", err)
			}
			if err := metadata.Put(installationIDKey, []byte(hex.EncodeToString(random[:]))); err != nil {
				return err
			}
		}
		if metadata.Get(createdAtKey) == nil {
			if err := metadata.Put(createdAtKey, []byte(time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
				return err
			}
		}
		if metadata.Get(mihomoStateKey) == nil {
			if err := metadata.Put(mihomoStateKey, []byte("not_installed")); err != nil {
				return err
			}
		}
		if metadata.Get(mihomoDesiredStateKey) == nil {
			if err := metadata.Put(mihomoDesiredStateKey, []byte(runtimeapi.MihomoDesiredUnset)); err != nil {
				return err
			}
		}
		if metadata.Get(mihomoRecoveryStateKey) == nil {
			if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
				return err
			}
		}
		if metadata.Get(mihomoCrashAttemptsKey) == nil {
			if err := metadata.Put(mihomoCrashAttemptsKey, encodeUint64(0)); err != nil {
				return err
			}
		}
		if metadata.Get(runModeKey) == nil {
			if err := metadata.Put(runModeKey, []byte("unconfigured")); err != nil {
				return err
			}
		}
		if err := createRuntimeBuckets(transaction); err != nil {
			return err
		}
		return nil
	})
}

func prepareStateRoot(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("Runtime state root must be a fixed absolute non-root path")
	}
	absolute, err := filepath.Abs(root)
	if err != nil || absolute == filepath.VolumeName(absolute)+string(filepath.Separator) {
		return "", errors.New("Runtime state root must be a fixed absolute non-root path")
	}
	linked, err := safepath.ContainsLinkInExistingPath(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime state root ancestors: %w", err)
	}
	if linked {
		return "", errors.New("Runtime state root must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Runtime state root must be a real directory")
	}
	linked, err = safepath.ContainsLink(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime state root: %w", err)
	}
	if linked {
		return "", errors.New("Runtime state root must not contain symbolic or reparse links")
	}
	if err := os.Chmod(absolute, 0700); err != nil {
		return "", err
	}
	return absolute, nil
}

func encodeUint64(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return encoded[:]
}

func readUint64(value []byte) (uint64, error) {
	if len(value) != 8 {
		return 0, errors.New("Runtime metadata integer is invalid")
	}
	return binary.BigEndian.Uint64(value), nil
}
