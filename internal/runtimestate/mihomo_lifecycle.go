package runtimestate

import (
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

const MihomoCrashWindow = 10 * time.Minute

var MihomoRestartBackoff = [...]time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
}

type MihomoRecoveryDecision struct {
	Attempt       int
	Restart       bool
	NextRestartAt *time.Time
}

func (s *Store) PrepareMihomoStartup(now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("Runtime state is not open")
	}
	now = now.UTC()
	restore := false
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		desired := desiredMihomoState(metadata)
		actual := string(metadata.Get(mihomoStateKey))
		if actual == "running" || actual == "starting" {
			actual = "stopped"
			if err := metadata.Put(mihomoStateKey, []byte(actual)); err != nil {
				return err
			}
		}
		if desired == runtimeapi.MihomoDesiredRunning {
			restore = true
			if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryStartup)); err != nil {
				return err
			}
			if err := clearMihomoRetry(metadata); err != nil {
				return err
			}
			if err := clearMihomoFault(metadata); err != nil {
				return err
			}
			_, err := advanceRevisionAndEvent(transaction, "mihomo.startup_recovery", "", now)
			return err
		}
		if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
			return err
		}
		if err := clearMihomoRetry(metadata); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "mihomo.startup_observed", "", now)
		return err
	})
	return restore, err
}

func (s *Store) RegisterMihomoCrash(now time.Time) (MihomoRecoveryDecision, error) {
	if s == nil || s.db == nil {
		return MihomoRecoveryDecision{}, errors.New("Runtime state is not open")
	}
	now = now.UTC()
	var decision MihomoRecoveryDecision
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		if err := metadata.Put(mihomoStateKey, []byte("stopped")); err != nil {
			return err
		}
		if desiredMihomoState(metadata) != runtimeapi.MihomoDesiredRunning {
			if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
				return err
			}
			if err := clearMihomoRetry(metadata); err != nil {
				return err
			}
			_, err := advanceRevisionAndEvent(transaction, "mihomo.stopped", "", now)
			return err
		}
		switch string(metadata.Get(mihomoRecoveryStateKey)) {
		case runtimeapi.MihomoRecoveryNeedsAttention, runtimeapi.MihomoRecoveryFailOpenUnknown:
			attempts, err := readUint64(metadata.Get(mihomoCrashAttemptsKey))
			if err != nil {
				return err
			}
			decision.Attempt = int(attempts)
			return nil
		}

		windowStarted, err := readMetadataTime(metadata, mihomoCrashWindowKey)
		if err != nil {
			return err
		}
		attempts, err := readUint64(metadata.Get(mihomoCrashAttemptsKey))
		if err != nil {
			return err
		}
		if windowStarted.IsZero() || now.Sub(windowStarted) >= MihomoCrashWindow {
			windowStarted = now
			attempts = 0
		}
		if err := metadata.Put(mihomoCrashWindowKey, []byte(windowStarted.Format(time.RFC3339Nano))); err != nil {
			return err
		}
		if attempts >= uint64(len(MihomoRestartBackoff)) {
			decision.Attempt = int(attempts)
			if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryNeedsAttention)); err != nil {
				return err
			}
			if err := metadata.Delete(mihomoNextRestartKey); err != nil {
				return err
			}
			if err := putMihomoFault(metadata, "mihomo_restart_limit", "Mihomo exited repeatedly and automatic restart is disabled"); err != nil {
				return err
			}
			_, err := advanceRevisionAndEvent(transaction, "mihomo.needs_attention", "", now)
			return err
		}

		attempts++
		decision.Attempt = int(attempts)
		if err := metadata.Put(mihomoCrashAttemptsKey, encodeUint64(attempts)); err != nil {
			return err
		}
		next := now.Add(MihomoRestartBackoff[attempts-1])
		decision.Restart = true
		decision.NextRestartAt = &next
		if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryWaiting)); err != nil {
			return err
		}
		if err := metadata.Put(mihomoNextRestartKey, []byte(next.Format(time.RFC3339Nano))); err != nil {
			return err
		}
		if err := putMihomoFault(metadata, "mihomo_unexpected_exit", "Mihomo exited unexpectedly; traffic is using the direct path"); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "mihomo.restart_scheduled", "", now)
		return err
	})
	return decision, err
}

func (s *Store) MarkMihomoRestarting(now time.Time) error {
	return s.updateMihomoRecovery(now, "starting", runtimeapi.MihomoRecoveryRestarting, "mihomo.restarting", false)
}

func (s *Store) MarkMihomoRestartSucceeded(now time.Time) error {
	return s.updateMihomoRecovery(now, "running", runtimeapi.MihomoRecoveryMonitoring, "mihomo.restart_succeeded", true)
}

func (s *Store) MarkMihomoStable(now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	now = now.UTC()
	return s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		if err := metadata.Put(mihomoStateKey, []byte("running")); err != nil {
			return err
		}
		if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
			return err
		}
		if err := clearMihomoRetry(metadata); err != nil {
			return err
		}
		if err := clearMihomoFault(metadata); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "mihomo.stable", "", now)
		return err
	})
}

func (s *Store) MarkMihomoRecoveryFailure(code, message string, failOpenUnknown bool, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	if code == "" || message == "" {
		return errors.New("Mihomo recovery failure is incomplete")
	}
	now = now.UTC()
	return s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		recovery := runtimeapi.MihomoRecoveryNeedsAttention
		if failOpenUnknown {
			recovery = runtimeapi.MihomoRecoveryFailOpenUnknown
		}
		if err := metadata.Put(mihomoStateKey, []byte("stopped")); err != nil {
			return err
		}
		if err := metadata.Put(mihomoRecoveryStateKey, []byte(recovery)); err != nil {
			return err
		}
		if err := metadata.Delete(mihomoNextRestartKey); err != nil {
			return err
		}
		if err := putMihomoFault(metadata, code, message); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "mihomo.needs_attention", "", now)
		return err
	})
}

func (s *Store) ObserveMihomoCoreVersion(current string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("Runtime state is not open")
	}
	if current == "" {
		return false, errors.New("Mihomo core version is required")
	}
	now = now.UTC()
	changed := false
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		previous := string(metadata.Get(mihomoCoreVersionKey))
		if previous == current {
			return nil
		}
		if err := metadata.Put(mihomoCoreVersionKey, []byte(current)); err != nil {
			return err
		}
		if previous == "" {
			_, err := advanceRevisionAndEvent(transaction, "mihomo.core_observed", "", now)
			return err
		}
		changed = true
		if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
			return err
		}
		if err := clearMihomoRetry(metadata); err != nil {
			return err
		}
		if err := clearMihomoFault(metadata); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "mihomo.recovery_cleared_by_core", "", now)
		return err
	})
	return changed, err
}

func (s *Store) updateMihomoRecovery(
	now time.Time,
	actual string,
	recovery string,
	eventType string,
	stableSince bool,
) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	now = now.UTC()
	return s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		if err := metadata.Put(mihomoStateKey, []byte(actual)); err != nil {
			return err
		}
		if err := metadata.Put(mihomoRecoveryStateKey, []byte(recovery)); err != nil {
			return err
		}
		if err := metadata.Delete(mihomoNextRestartKey); err != nil {
			return err
		}
		if stableSince {
			if err := metadata.Put(mihomoStableSinceKey, []byte(now.Format(time.RFC3339Nano))); err != nil {
				return err
			}
		}
		_, err := advanceRevisionAndEvent(transaction, eventType, "", now)
		return err
	})
}

func mihomoLifecycleSummary(metadata *bbolt.Bucket) (runtimeapi.MihomoStatus, error) {
	state := string(metadata.Get(mihomoStateKey))
	if state == "" {
		state = "not_installed"
	}
	recovery := string(metadata.Get(mihomoRecoveryStateKey))
	if recovery == "" {
		recovery = runtimeapi.MihomoRecoveryIdle
	}
	attempts, err := readUint64(metadata.Get(mihomoCrashAttemptsKey))
	if err != nil {
		return runtimeapi.MihomoStatus{}, err
	}
	next, err := readMetadataTime(metadata, mihomoNextRestartKey)
	if err != nil {
		return runtimeapi.MihomoStatus{}, err
	}
	status := runtimeapi.MihomoStatus{
		Version:       string(metadata.Get(mihomoCoreVersionKey)),
		DesiredState:  desiredMihomoState(metadata),
		State:         state,
		Recovery:      recovery,
		CrashAttempts: int(attempts),
	}
	if !next.IsZero() {
		status.NextRestartAt = &next
	}
	if code := string(metadata.Get(mihomoFaultCodeKey)); code != "" {
		status.Fault = &runtimeapi.Fault{
			Code:    code,
			Message: string(metadata.Get(mihomoFaultMessageKey)),
		}
	}
	return status, nil
}

func desiredMihomoState(metadata *bbolt.Bucket) string {
	value := string(metadata.Get(mihomoDesiredStateKey))
	switch value {
	case runtimeapi.MihomoDesiredRunning, runtimeapi.MihomoDesiredStopped:
		return value
	default:
		return runtimeapi.MihomoDesiredUnset
	}
}

func recordExplicitMihomoStart(metadata *bbolt.Bucket) error {
	if err := metadata.Put(mihomoDesiredStateKey, []byte(runtimeapi.MihomoDesiredRunning)); err != nil {
		return err
	}
	if err := metadata.Put(mihomoStateKey, []byte("running")); err != nil {
		return err
	}
	if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
		return err
	}
	if err := clearMihomoRetry(metadata); err != nil {
		return err
	}
	return clearMihomoFault(metadata)
}

func recordExplicitMihomoStop(metadata *bbolt.Bucket) error {
	if err := metadata.Put(mihomoDesiredStateKey, []byte(runtimeapi.MihomoDesiredStopped)); err != nil {
		return err
	}
	if err := metadata.Put(mihomoStateKey, []byte("stopped")); err != nil {
		return err
	}
	if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
		return err
	}
	if err := clearMihomoRetry(metadata); err != nil {
		return err
	}
	return clearMihomoFault(metadata)
}

func clearMihomoRecoveryForNewConfig(metadata *bbolt.Bucket, previous, current string) error {
	if current == "" || current == previous {
		return nil
	}
	if err := metadata.Put(mihomoRecoveryStateKey, []byte(runtimeapi.MihomoRecoveryIdle)); err != nil {
		return err
	}
	if err := clearMihomoRetry(metadata); err != nil {
		return err
	}
	return clearMihomoFault(metadata)
}

func clearMihomoRetry(metadata *bbolt.Bucket) error {
	if err := metadata.Put(mihomoCrashAttemptsKey, encodeUint64(0)); err != nil {
		return err
	}
	for _, key := range [][]byte{mihomoCrashWindowKey, mihomoNextRestartKey, mihomoStableSinceKey} {
		if err := metadata.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func putMihomoFault(metadata *bbolt.Bucket, code, message string) error {
	if err := metadata.Put(mihomoFaultCodeKey, []byte(code)); err != nil {
		return err
	}
	return metadata.Put(mihomoFaultMessageKey, []byte(message))
}

func clearMihomoFault(metadata *bbolt.Bucket) error {
	if err := metadata.Delete(mihomoFaultCodeKey); err != nil {
		return err
	}
	return metadata.Delete(mihomoFaultMessageKey)
}

func readMetadataTime(metadata *bbolt.Bucket, key []byte) (time.Time, error) {
	value := metadata.Get(key)
	if len(value) == 0 {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, string(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("Runtime metadata time %q is invalid: %w", key, err)
	}
	return parsed.UTC(), nil
}
