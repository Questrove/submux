package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"

	"submux/internal/agentproto"
)

const (
	InstanceEnrolling = "enrolling"
	InstanceOnline    = "online"
	InstanceOffline   = "offline"
	InstanceDegraded  = "degraded"
	InstanceRevoked   = "revoked"

	RuntimeNotInstalled = "not_installed"
	RuntimeStopped      = "stopped"
	RuntimeStarting     = "starting"
	RuntimeRunning      = "running"
	RuntimeFailed       = "failed"
)

type AgentEnrollment struct {
	Digest    string `json:"digest"`
	Name      string `json:"name"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

type RuntimeInstance struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	DeviceKey    string   `json:"device_public_key,omitempty"`
	OS           string   `json:"os,omitempty"`
	Arch         string   `json:"arch,omitempty"`
	AgentVersion string   `json:"agent_version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Status       string   `json:"status"`
	LastSeen     string   `json:"last_seen,omitempty"`
	RevokedAt    string   `json:"revoked_at,omitempty"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

type RuntimeOperation struct {
	RequestID      string `json:"request_id,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	Kind           string `json:"kind"`
	Phase          string `json:"phase"`
	Status         string `json:"status"`
	BytesCompleted int64  `json:"bytes_completed,omitempty"`
	BytesTotal     int64  `json:"bytes_total,omitempty"`
	Error          string `json:"error,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	UpdatedAt      string `json:"updated_at"`
	FinishedAt     string `json:"finished_at,omitempty"`
}

type RuntimeObservation struct {
	InstanceID           int64                                   `json:"instance_id"`
	RemoteRevision       string                                  `json:"remote_revision,omitempty"`
	AppliedRevision      string                                  `json:"applied_revision,omitempty"`
	RejectedRevision     string                                  `json:"rejected_revision,omitempty"`
	UpdateAvailable      bool                                    `json:"update_available"`
	LastUpdateAt         string                                  `json:"last_update_at,omitempty"`
	CoreVersion          string                                  `json:"core_version,omitempty"`
	PreviousCoreVersion  string                                  `json:"previous_core_version,omitempty"`
	CoreStatus           string                                  `json:"core_status"`
	AgentUptimeSeconds   int64                                   `json:"agent_uptime_seconds,omitempty"`
	ProxyListening       bool                                    `json:"proxy_listening"`
	ProxyPort            int                                     `json:"proxy_port,omitempty"`
	ProxyKind            string                                  `json:"proxy_kind,omitempty"`
	ControllerPort       int                                     `json:"controller_port,omitempty"`
	SelectedProxies      map[string]string                       `json:"selected_proxies,omitempty"`
	SelectionNotice      string                                  `json:"selection_notice,omitempty"`
	ResourceProxyMode    string                                  `json:"resource_proxy_mode,omitempty"`
	ResourceProxyURL     string                                  `json:"resource_proxy_url,omitempty"`
	Operation            *RuntimeOperation                       `json:"operation,omitempty"`
	LastGoodRevision     string                                  `json:"last_good_revision,omitempty"`
	ActiveSubscriptionID string                                  `json:"active_subscription_id,omitempty"`
	Subscriptions        []agentproto.RuntimeSubscriptionSummary `json:"subscriptions,omitempty"`
	RecentError          string                                  `json:"recent_error,omitempty"`
	ObservedAt           string                                  `json:"observed_at"`
}

type AgentJob struct {
	agentproto.Job
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
}

type AuditEvent struct {
	ID         int64  `json:"id"`
	ActorType  string `json:"actor_type"`
	RequestID  string `json:"request_id"`
	InstanceID int64  `json:"instance_id,omitempty"`
	Action     string `json:"action"`
	Revision   string `json:"revision,omitempty"`
	Result     string `json:"result"`
	Summary    string `json:"summary,omitempty"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) SaveAgentEnrollment(value AgentEnrollment) error {
	if value.Digest == "" || value.Name == "" || value.ExpiresAt == "" {
		return errors.New("enrollment digest, name and expiry are required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket([]byte("agent_enrollments")), []byte(value.Digest), value)
	})
}

func (s *Store) ConsumeAgentEnrollment(digest string, now time.Time) (AgentEnrollment, error) {
	var result AgentEnrollment
	expired := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("agent_enrollments"))
		raw := bucket.Get([]byte(digest))
		if raw == nil {
			return errors.New("invalid or already used pairing code")
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if err := bucket.Delete([]byte(digest)); err != nil {
			return err
		}
		expires, err := time.Parse(time.RFC3339, result.ExpiresAt)
		if err != nil || !now.Before(expires) {
			expired = true
		}
		return nil
	})
	if err == nil && expired {
		err = errors.New("pairing code has expired")
	}
	return result, err
}

func (s *Store) CreateRuntimeInstance(value RuntimeInstance) (RuntimeInstance, error) {
	var result RuntimeInstance
	err := s.db.Update(func(tx *bolt.Tx) error {
		instances := tx.Bucket([]byte("runtime_instances"))
		if err := instances.ForEach(func(_, raw []byte) error {
			var existing RuntimeInstance
			if err := json.Unmarshal(raw, &existing); err != nil {
				return err
			}
			if existing.DeviceKey == value.DeviceKey {
				return errors.New("device key is already enrolled")
			}
			return nil
		}); err != nil {
			return err
		}
		seq, err := instances.NextSequence()
		if err != nil {
			return err
		}
		now := nowRFC3339()
		value.ID, value.Status, value.CreatedAt, value.UpdatedAt = int64(seq), InstanceOnline, now, now
		value.Capabilities = NormalizeStringSet(value.Capabilities)
		if err := putJSON(instances, itob(value.ID), value); err != nil {
			return err
		}
		observation := RuntimeObservation{InstanceID: value.ID, CoreStatus: RuntimeNotInstalled, ObservedAt: now}
		if err := putJSON(tx.Bucket([]byte("runtime_observations")), itob(value.ID), observation); err != nil {
			return err
		}
		result = value
		return nil
	})
	return result, err
}

func (s *Store) GetRuntimeInstance(id int64) (RuntimeInstance, error) {
	var result RuntimeInstance
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("runtime_instances")).Get(itob(id))
		if raw == nil {
			return fmt.Errorf("no runtime instance with id %d", id)
		}
		return json.Unmarshal(raw, &result)
	})
	return result, err
}

func (s *Store) ListRuntimeInstances() ([]RuntimeInstance, error) {
	var result []RuntimeInstance
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("runtime_instances")).ForEach(func(_, raw []byte) error {
			var value RuntimeInstance
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			result = append(result, value)
			return nil
		})
	})
	return result, err
}

func (s *Store) UpdateRuntimeHeartbeat(id int64, version string, capabilities []string, status string, seen time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("runtime_instances"))
		raw := bucket.Get(itob(id))
		if raw == nil {
			return fmt.Errorf("no runtime instance with id %d", id)
		}
		var value RuntimeInstance
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.RevokedAt != "" {
			return errors.New("runtime instance is revoked")
		}
		if status != InstanceOnline && status != InstanceDegraded {
			return errors.New("heartbeat status must be online or degraded")
		}
		value.AgentVersion, value.Capabilities, value.Status = version, NormalizeStringSet(capabilities), status
		value.LastSeen, value.UpdatedAt = seen.UTC().Format(time.RFC3339), nowRFC3339()
		return putJSON(bucket, itob(id), value)
	})
}

func (s *Store) RevokeRuntimeInstance(id int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		instances := tx.Bucket([]byte("runtime_instances"))
		raw := instances.Get(itob(id))
		if raw == nil {
			return fmt.Errorf("no runtime instance with id %d", id)
		}
		var value RuntimeInstance
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		now := nowRFC3339()
		value.Status, value.RevokedAt, value.UpdatedAt = InstanceRevoked, now, now
		if err := putJSON(instances, itob(id), value); err != nil {
			return err
		}
		jobs := tx.Bucket([]byte("agent_jobs"))
		var updates []AgentJob
		if err := jobs.ForEach(func(_, raw []byte) error {
			var job AgentJob
			if err := json.Unmarshal(raw, &job); err != nil {
				return err
			}
			if job.InstanceID == id && (job.Status == agentproto.JobQueued || job.Status == agentproto.JobAccepted) {
				job.Status, job.UpdatedAt, job.FinishedAt = agentproto.JobCancelled, now, now
				updates = append(updates, job)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, job := range updates {
			if err := putJSON(jobs, []byte(job.ID), job); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetRuntimeObservation(instanceID int64) (RuntimeObservation, error) {
	var result RuntimeObservation
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("runtime_observations")).Get(itob(instanceID))
		if raw == nil {
			return fmt.Errorf("no observation for instance %d", instanceID)
		}
		return json.Unmarshal(raw, &result)
	})
	return result, err
}

func (s *Store) SaveRuntimeObservation(value RuntimeObservation) error {
	value.ObservedAt = nowRFC3339()
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket([]byte("runtime_observations")), itob(value.InstanceID), value)
	})
}

func (s *Store) CreateAgentJob(value AgentJob) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("agent_jobs"))
		if bucket.Get([]byte(value.ID)) != nil {
			return errors.New("agent job ID already exists")
		}
		now := nowRFC3339()
		value.CreatedAt, value.UpdatedAt = now, now
		return putJSON(bucket, []byte(value.ID), value)
	})
}

func (s *Store) GetAgentJob(id string) (AgentJob, error) {
	var result AgentJob
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("agent_jobs")).Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("no agent job with id %q", id)
		}
		return json.Unmarshal(raw, &result)
	})
	return result, err
}

func (s *Store) ListAgentJobs(instanceID int64) ([]AgentJob, error) {
	var result []AgentJob
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("agent_jobs")).ForEach(func(_, raw []byte) error {
			var value AgentJob
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if instanceID == 0 || value.InstanceID == instanceID {
				result = append(result, value)
			}
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt > result[j].CreatedAt })
	return result, err
}

func (s *Store) ListRunnableAgentJobs(instanceID int64, now time.Time) ([]AgentJob, error) {
	var result []AgentJob
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("agent_jobs"))
		var expired []AgentJob
		if err := bucket.ForEach(func(_, raw []byte) error {
			var value AgentJob
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.InstanceID != instanceID || (value.Status != agentproto.JobQueued && value.Status != agentproto.JobAccepted) {
				return nil
			}
			deadline, err := time.Parse(time.RFC3339, value.Deadline)
			if err != nil || !now.Before(deadline) {
				value.Status, value.UpdatedAt, value.FinishedAt = agentproto.JobExpired, nowRFC3339(), nowRFC3339()
				expired = append(expired, value)
				return nil
			}
			result = append(result, value)
			return nil
		}); err != nil {
			return err
		}
		for _, value := range expired {
			if err := putJSON(bucket, []byte(value.ID), value); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) TransitionAgentJob(id, status string, result json.RawMessage, safeError string) (AgentJob, error) {
	var updated AgentJob
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("agent_jobs"))
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("no agent job with id %q", id)
		}
		if err := json.Unmarshal(raw, &updated); err != nil {
			return err
		}
		if !agentproto.ValidTransition(updated.Status, status) {
			return fmt.Errorf("invalid job transition %s -> %s", updated.Status, status)
		}
		now := nowRFC3339()
		updated.Status, updated.UpdatedAt = status, now
		if status == agentproto.JobRunning {
			updated.StartedAt = now
		}
		if agentproto.TerminalJobStatus(status) {
			updated.Result, updated.Error, updated.FinishedAt = result, safeError, now
		}
		return putJSON(bucket, []byte(id), updated)
	})
	return updated, err
}

func (s *Store) AddAuditEvent(value AuditEvent) (AuditEvent, error) {
	var result AuditEvent
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("audit_events"))
		if value.RequestID != "" {
			if err := bucket.ForEach(func(_, raw []byte) error {
				var existing AuditEvent
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
				if existing.RequestID == value.RequestID && existing.InstanceID == value.InstanceID && existing.Action == value.Action {
					result = existing
					return errStopIteration
				}
				return nil
			}); err != nil && !errors.Is(err, errStopIteration) {
				return err
			}
			if result.ID != 0 {
				return nil
			}
		}
		seq, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		value.ID, value.CreatedAt = int64(seq), nowRFC3339()
		result = value
		return putJSON(bucket, itob(value.ID), value)
	})
	return result, err
}

func (s *Store) ListAuditEvents(instanceID int64, limit int) ([]AuditEvent, error) {
	var result []AuditEvent
	err := s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket([]byte("audit_events")).Cursor()
		for _, raw := cursor.Last(); raw != nil; _, raw = cursor.Prev() {
			var value AuditEvent
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if instanceID == 0 || value.InstanceID == instanceID {
				result = append(result, value)
				if limit > 0 && len(result) >= limit {
					break
				}
			}
		}
		return nil
	})
	return result, err
}

var errStopIteration = errors.New("stop iteration")
