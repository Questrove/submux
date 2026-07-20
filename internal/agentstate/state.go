package agentstate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"

	"submux/internal/agentproto"
	"submux/internal/safepath"
)

var (
	bucketConfig        = []byte("config")
	bucketJobs          = []byte("jobs")
	bucketAudits        = []byte("audits")
	bucketSubscriptions = []byte("subscriptions")
	keyIdentity         = []byte("identity")
	keyRuntime          = []byte("runtime")
)

type Identity struct {
	ServerURL  string `json:"server_url"`
	InstanceID int64  `json:"instance_id"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
	EnrolledAt string `json:"enrolled_at"`
}

func (i Identity) PrivateKeyBytes() ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(i.PrivateKey)
	if err != nil {
		return nil, errors.New("invalid stored device private key")
	}
	return raw, nil
}

type Runtime struct {
	RemoteRevision       string            `json:"remote_revision,omitempty"`
	AppliedRevision      string            `json:"applied_revision,omitempty"`
	RejectedRevision     string            `json:"rejected_revision,omitempty"`
	LastGoodRevision     string            `json:"last_good_revision,omitempty"`
	CoreVersion          string            `json:"core_version,omitempty"`
	PreviousCoreVersion  string            `json:"previous_core_version,omitempty"`
	CoreStatus           string            `json:"core_status"`
	MihomoSecret         string            `json:"mihomo_secret,omitempty"`
	ProxyPort            int               `json:"proxy_port,omitempty"`
	ProxyKind            string            `json:"proxy_kind,omitempty"`
	ControllerPort       int               `json:"controller_port,omitempty"`
	SelectedProxies      map[string]string `json:"selected_proxies,omitempty"`
	SelectionNotice      string            `json:"selection_notice,omitempty"`
	ResourceProxyMode    string            `json:"resource_proxy_mode,omitempty"`
	ResourceProxyURL     string            `json:"resource_proxy_url,omitempty"`
	RecentError          string            `json:"recent_error,omitempty"`
	LastUpdateAt         string            `json:"last_update_at,omitempty"`
	ActiveSubscriptionID string            `json:"active_subscription_id,omitempty"`
}

// RuntimeSubscription is Agent-owned state. External URLs and downloaded
// configuration are never returned in heartbeat observations.
type RuntimeSubscription struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	URL                    string `json:"url,omitempty"`
	PlatformSubscriptionID int64  `json:"platform_subscription_id,omitempty"`
	Host                   string `json:"host"`
	Config                 []byte `json:"config,omitempty"`
	Revision               string `json:"revision,omitempty"`
	ETag                   string `json:"etag,omitempty"`
	LastModified           string `json:"last_modified,omitempty"`
	UsedBytes              int64  `json:"used_bytes,omitempty"`
	TotalBytes             int64  `json:"total_bytes,omitempty"`
	ExpiresAt              string `json:"expires_at,omitempty"`
	LastUpdatedAt          string `json:"last_updated_at,omitempty"`
	LastError              string `json:"last_error,omitempty"`
	CreatedAt              string `json:"created_at"`
	UpdatedAt              string `json:"updated_at"`
}

type LocalJob struct {
	Job        agentproto.Job  `json:"job"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
	Reported   bool            `json:"reported"`
}

type LocalAudit struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Revision  string `json:"revision,omitempty"`
	Result    string `json:"result"`
	Summary   string `json:"summary,omitempty"`
	CreatedAt string `json:"created_at"`
	Reported  bool   `json:"reported"`
}

type Store struct {
	db *bolt.DB
}

func Open(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || path == "" || !filepath.IsAbs(path) || filepath.Clean(absolute) == filepath.VolumeName(absolute)+string(filepath.Separator) {
		return nil, errors.New("Agent state path must be an absolute non-root file path")
	}
	path = filepath.Clean(absolute)
	directory := filepath.Dir(path)
	linked, err := safepath.ContainsLinkInExistingPath(directory)
	if err != nil || linked {
		return nil, errors.New("Agent state directory must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	linked, err = safepath.ContainsLink(directory)
	if err != nil || linked {
		return nil, errors.New("Agent state directory must not contain symbolic or reparse links")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("Agent state database must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		db.Close()
		return nil, errors.New("Agent state database changed to an invalid file")
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketConfig, bucketJobs, bucketAudits, bucketSubscriptions} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		config := tx.Bucket(bucketConfig)
		if raw := config.Get(keyRuntime); raw != nil {
			value := defaultRuntime()
			if err := decodeRuntime(raw, &value); err != nil {
				return err
			}
			if value.SelectedProxies == nil {
				value.SelectedProxies = map[string]string{}
			}
			if err := putJSON(config, keyRuntime, value); err != nil {
				return err
			}
		}
		jobs := tx.Bucket(bucketJobs)
		var staleJobs [][]byte
		type jobUpdate struct {
			key []byte
			raw []byte
		}
		var jobUpdates []jobUpdate
		if err := jobs.ForEach(func(key, raw []byte) error {
			var value LocalJob
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Job.Type == "update_subscription" || value.Job.Type == "collect_diagnostics" {
				staleJobs = append(staleJobs, append([]byte(nil), key...))
			} else if value.Job.Type == "configure_download_proxy" {
				value.Job.Type = agentproto.JobConfigureResourceProxy
				value.Job.Params = migrateProxyObject(value.Job.Params)
				value.Result = migrateProxyObject(value.Result)
				encoded, err := json.Marshal(value)
				if err != nil {
					return err
				}
				jobUpdates = append(jobUpdates, jobUpdate{key: append([]byte(nil), key...), raw: encoded})
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range staleJobs {
			if err := jobs.Delete(key); err != nil {
				return err
			}
		}
		for _, update := range jobUpdates {
			if err := jobs.Put(update.key, update.raw); err != nil {
				return err
			}
		}
		audits := tx.Bucket(bucketAudits)
		var staleAudits [][]byte
		if err := audits.ForEach(func(key, raw []byte) error {
			var value LocalAudit
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Action == "subscription.update" {
				staleAudits = append(staleAudits, append([]byte(nil), key...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range staleAudits {
			if err := audits.Delete(key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SaveIdentity(value Identity) error {
	if value.ServerURL == "" || value.InstanceID <= 0 || value.PublicKey == "" || value.PrivateKey == "" {
		return errors.New("complete enrolled identity is required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketConfig)
		if bucket.Get(keyIdentity) != nil {
			return errors.New("agent is already enrolled; revoke it locally before replacing identity")
		}
		return putJSON(bucket, keyIdentity, value)
	})
}

func (s *Store) Identity() (Identity, error) {
	var value Identity
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketConfig).Get(keyIdentity)
		if raw == nil {
			return errors.New("agent is not enrolled")
		}
		return json.Unmarshal(raw, &value)
	})
	return value, err
}

func (s *Store) ClearEnrollment() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		config := tx.Bucket(bucketConfig)
		if err := config.Delete(keyIdentity); err != nil {
			return err
		}
		if err := config.Delete(keyRuntime); err != nil {
			return err
		}
		for _, name := range [][]byte{bucketJobs, bucketAudits, bucketSubscriptions} {
			if err := tx.DeleteBucket(name); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
				return err
			}
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListRuntimeSubscriptions() ([]RuntimeSubscription, error) {
	values := make([]RuntimeSubscription, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSubscriptions).ForEach(func(_, raw []byte) error {
			var value RuntimeSubscription
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			values = append(values, value)
			return nil
		})
	})
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt == values[j].CreatedAt {
			return values[i].ID < values[j].ID
		}
		return values[i].CreatedAt < values[j].CreatedAt
	})
	return values, err
}

func (s *Store) RuntimeSubscription(id string) (RuntimeSubscription, error) {
	var value RuntimeSubscription
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketSubscriptions).Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("no runtime subscription %q", id)
		}
		return json.Unmarshal(raw, &value)
	})
	return value, err
}

func (s *Store) SaveRuntimeSubscription(value RuntimeSubscription) (RuntimeSubscription, error) {
	hasURL, hasPlatform := value.URL != "", value.PlatformSubscriptionID > 0
	if value.ID == "" || value.Name == "" || value.Host == "" || hasURL == hasPlatform {
		return RuntimeSubscription{}, errors.New("complete runtime subscription identity is required")
	}
	if len(value.Config) > 10<<20 {
		return RuntimeSubscription{}, errors.New("runtime subscription config exceeds 10 MiB")
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketSubscriptions)
		now := time.Now().UTC().Format(time.RFC3339)
		if raw := bucket.Get([]byte(value.ID)); raw != nil {
			var existing RuntimeSubscription
			if err := json.Unmarshal(raw, &existing); err != nil {
				return err
			}
			value.CreatedAt = existing.CreatedAt
		} else {
			count := 0
			if err := bucket.ForEach(func(_, _ []byte) error { count++; return nil }); err != nil {
				return err
			}
			if count >= 32 {
				return errors.New("runtime subscription limit reached")
			}
			value.CreatedAt = now
		}
		value.UpdatedAt = now
		return putJSON(bucket, []byte(value.ID), value)
	})
	return value, err
}

func (s *Store) DeleteRuntimeSubscription(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketSubscriptions)
		if bucket.Get([]byte(id)) == nil {
			return fmt.Errorf("no runtime subscription %q", id)
		}
		return bucket.Delete([]byte(id))
	})
}

func (s *Store) Runtime() (Runtime, error) {
	value := defaultRuntime()
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketConfig).Get(keyRuntime)
		if raw == nil {
			return nil
		}
		return decodeRuntime(raw, &value)
	})
	if value.SelectedProxies == nil {
		value.SelectedProxies = map[string]string{}
	}
	return value, err
}

func (s *Store) UpdateRuntime(update func(*Runtime) error) (Runtime, error) {
	var value Runtime
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketConfig)
		value = defaultRuntime()
		if raw := bucket.Get(keyRuntime); raw != nil {
			if err := decodeRuntime(raw, &value); err != nil {
				return err
			}
		}
		if value.SelectedProxies == nil {
			value.SelectedProxies = map[string]string{}
		}
		if err := update(&value); err != nil {
			return err
		}
		return putJSON(bucket, keyRuntime, value)
	})
	return value, err
}

func defaultRuntime() Runtime {
	return Runtime{CoreStatus: "not_installed", ControllerPort: 9090, ResourceProxyMode: agentproto.ResourceProxyDirect, SelectedProxies: map[string]string{}}
}

// decodeRuntime migrates the development-only download_proxy fields in place.
// The next write keeps only resource_proxy fields.
func decodeRuntime(raw []byte, value *Runtime) error {
	if err := json.Unmarshal(raw, value); err != nil {
		return err
	}
	var proxyFields struct {
		ResourceMode *string `json:"resource_proxy_mode"`
		LegacyMode   *string `json:"download_proxy_mode"`
		LegacyURL    string  `json:"download_proxy_url"`
	}
	if err := json.Unmarshal(raw, &proxyFields); err != nil {
		return err
	}
	if proxyFields.ResourceMode == nil && proxyFields.LegacyMode != nil {
		value.ResourceProxyMode, value.ResourceProxyURL = *proxyFields.LegacyMode, proxyFields.LegacyURL
	}
	if value.ResourceProxyMode == "" {
		value.ResourceProxyMode = agentproto.ResourceProxyDirect
	}
	return nil
}

func migrateProxyObject(raw json.RawMessage) json.RawMessage {
	var object map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &object) != nil {
		return raw
	}
	value, ok := object["download_proxy"]
	if !ok {
		return raw
	}
	if _, exists := object["resource_proxy"]; !exists {
		object["resource_proxy"] = value
	}
	delete(object, "download_proxy")
	encoded, err := json.Marshal(object)
	if err != nil {
		return raw
	}
	return encoded
}

// BeginJob durably records running before any external side effect. Repeated
// IDs return the saved record so callers can report it without executing again.
func (s *Store) BeginJob(job agentproto.Job) (LocalJob, bool, error) {
	var result LocalJob
	started := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketJobs)
		if raw := bucket.Get([]byte(job.ID)); raw != nil {
			return json.Unmarshal(raw, &result)
		}
		result = LocalJob{Job: job, Status: agentproto.JobRunning, StartedAt: time.Now().UTC().Format(time.RFC3339)}
		started = true
		return putJSON(bucket, []byte(job.ID), result)
	})
	return result, started, err
}

func (s *Store) SaveUnstartedJob(job agentproto.Job, status, safeError string) (LocalJob, error) {
	if status != agentproto.JobExpired && status != agentproto.JobCancelled && status != agentproto.JobFailed {
		return LocalJob{}, errors.New("invalid unstarted terminal status")
	}
	var result LocalJob
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketJobs)
		if raw := bucket.Get([]byte(job.ID)); raw != nil {
			return json.Unmarshal(raw, &result)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		result = LocalJob{Job: job, Status: status, Error: safeError, FinishedAt: now}
		return putJSON(bucket, []byte(job.ID), result)
	})
	return result, err
}

func (s *Store) CompleteJob(id, status string, result json.RawMessage, safeError string) (LocalJob, error) {
	if status != agentproto.JobSucceeded && status != agentproto.JobFailed && status != agentproto.JobOutcomeUnknown {
		return LocalJob{}, errors.New("invalid completed job status")
	}
	var updated LocalJob
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketJobs)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("job %q was not started", id)
		}
		if err := json.Unmarshal(raw, &updated); err != nil {
			return err
		}
		if updated.Status != agentproto.JobRunning {
			return fmt.Errorf("job %q is already %s", id, updated.Status)
		}
		updated.Status, updated.Result, updated.Error = status, result, safeError
		updated.FinishedAt, updated.Reported = time.Now().UTC().Format(time.RFC3339), false
		if err := putJSON(bucket, []byte(id), updated); err != nil {
			return err
		}
		return pruneJobs(bucket, 1000)
	})
	return updated, err
}

func (s *Store) RecoverInterruptedJobs() ([]LocalJob, error) {
	var recovered []LocalJob
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketJobs)
		var updates []LocalJob
		if err := bucket.ForEach(func(_, raw []byte) error {
			var value LocalJob
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == agentproto.JobRunning {
				value.Status, value.Error = agentproto.JobOutcomeUnknown, "agent restarted while the operation outcome was not durably known"
				value.FinishedAt, value.Reported = time.Now().UTC().Format(time.RFC3339), false
				updates = append(updates, value)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, value := range updates {
			if err := putJSON(bucket, []byte(value.Job.ID), value); err != nil {
				return err
			}
			recovered = append(recovered, value)
		}
		return nil
	})
	return recovered, err
}

func (s *Store) UnreportedJobs() ([]LocalJob, error) {
	var values []LocalJob
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketJobs).ForEach(func(_, raw []byte) error {
			var value LocalJob
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if agentproto.TerminalJobStatus(value.Status) && !value.Reported {
				values = append(values, value)
			}
			return nil
		})
	})
	sort.Slice(values, func(i, j int) bool { return values[i].FinishedAt < values[j].FinishedAt })
	return values, err
}

func (s *Store) MarkJobReported(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketJobs)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("no local job %q", id)
		}
		var value LocalJob
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		value.Reported = true
		return putJSON(bucket, []byte(id), value)
	})
}

func (s *Store) AddLocalAudit(value LocalAudit) error {
	if value.ID == "" || value.RequestID == "" || value.Action == "" || value.Result == "" {
		return errors.New("complete local audit identity is required")
	}
	value.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketAudits)
		if bucket.Get([]byte(value.ID)) != nil {
			return errors.New("local audit ID already exists")
		}
		if err := putJSON(bucket, []byte(value.ID), value); err != nil {
			return err
		}
		return pruneAudits(bucket, 1000)
	})
}

func (s *Store) UnreportedLocalAudits() ([]LocalAudit, error) {
	var values []LocalAudit
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAudits).ForEach(func(_, raw []byte) error {
			var value LocalAudit
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if !value.Reported {
				values = append(values, value)
			}
			return nil
		})
	})
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt < values[j].CreatedAt })
	return values, err
}

func (s *Store) MarkLocalAuditReported(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketAudits)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("no local audit %q", id)
		}
		var value LocalAudit
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		value.Reported = true
		return putJSON(bucket, []byte(id), value)
	})
}

func putJSON(bucket *bolt.Bucket, key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, raw)
}

func pruneJobs(bucket *bolt.Bucket, keep int) error {
	type item struct{ id, finished string }
	var items []item
	if err := bucket.ForEach(func(key, raw []byte) error {
		var value LocalJob
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.FinishedAt != "" {
			items = append(items, item{id: string(key), finished: value.FinishedAt})
		}
		return nil
	}); err != nil {
		return err
	}
	if len(items) <= keep {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].finished < items[j].finished })
	for _, value := range items[:len(items)-keep] {
		if err := bucket.Delete([]byte(value.id)); err != nil {
			return err
		}
	}
	return nil
}

func pruneAudits(bucket *bolt.Bucket, keep int) error {
	type item struct{ id, created string }
	var items []item
	if err := bucket.ForEach(func(key, raw []byte) error {
		var value LocalAudit
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		items = append(items, item{id: string(key), created: value.CreatedAt})
		return nil
	}); err != nil {
		return err
	}
	if len(items) <= keep {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].created < items[j].created })
	for _, value := range items[:len(items)-keep] {
		if err := bucket.Delete([]byte(value.id)); err != nil {
			return err
		}
	}
	return nil
}
