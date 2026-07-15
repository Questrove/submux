package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

type NodeRecord struct {
	ID           int64           `json:"id"`
	SourceID     int64           `json:"source_id"`
	Origin       string          `json:"origin"`
	Name         string          `json:"name"`
	Alias        string          `json:"alias,omitempty"`
	Protocol     string          `json:"protocol"`
	Config       json.RawMessage `json:"config"`
	Fingerprint  string          `json:"fingerprint"`
	Tags         []string        `json:"tags,omitempty"`
	Enabled      bool            `json:"enabled"`
	Role         string          `json:"role,omitempty"`
	RoleOverride string          `json:"role_override,omitempty"`
	Notice       *NodeNotice     `json:"notice,omitempty"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

type NodeSet struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	SourceIDs      []int64  `json:"source_ids,omitempty"`
	NodeIDs        []int64  `json:"node_ids,omitempty"`
	ExcludeNodeIDs []int64  `json:"exclude_node_ids,omitempty"`
	Protocols      []string `json:"protocols,omitempty"`
	NameContains   string   `json:"name_contains,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Enabled        bool     `json:"enabled"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

type TemplateSlot struct {
	Key      string `json:"key"`
	Target   string `json:"target"`
	Mode     string `json:"mode,omitempty"`
	Required bool   `json:"required"`
}

type Template struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Engine           string `json:"engine"`
	Scenario         string `json:"scenario"`
	Description      string `json:"description,omitempty"`
	Status           string `json:"status"`
	CurrentVersionID int64  `json:"current_version_id,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type TemplateVersion struct {
	ID            int64          `json:"id"`
	TemplateID    int64          `json:"template_id"`
	Version       int            `json:"version"`
	EngineVersion string         `json:"engine_version,omitempty"`
	Content       string         `json:"content"`
	Slots         []TemplateSlot `json:"slots"`
	Checksum      string         `json:"checksum"`
	PublishedAt   string         `json:"published_at"`
}

type ProfileBinding struct {
	Slot      string `json:"slot"`
	NodeSetID int64  `json:"node_set_id"`
}

type Profile struct {
	ID                int64            `json:"id"`
	Name              string           `json:"name"`
	TemplateVersionID int64            `json:"template_version_id"`
	Engine            string           `json:"engine"`
	Bindings          []ProfileBinding `json:"bindings"`
	Token             string           `json:"token"`
	Enabled           bool             `json:"enabled"`
	ExpiresAt         string           `json:"expires_at,omitempty"`
	CreatedAt         string           `json:"created_at"`
	UpdatedAt         string           `json:"updated_at"`
}

type ProfileArtifact struct {
	ProfileID     int64    `json:"profile_id"`
	Body          []byte   `json:"body,omitempty"`
	ContentType   string   `json:"content_type,omitempty"`
	Revision      string   `json:"revision,omitempty"`
	LastSuccess   string   `json:"last_success,omitempty"`
	LastError     string   `json:"last_error,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	BlockedReason string   `json:"blocked_reason,omitempty"`
	UpdatedAt     string   `json:"updated_at"`
}

func (s *Store) ReplaceSourceNodes(sourceID int64, incoming []NodeRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return replaceSourceNodesTx(tx, sourceID, incoming)
	})
}

// CommitSourceRefresh makes the normalized node snapshot and source metadata
// visible together. A failed transaction leaves both previous snapshots intact.
func (s *Store) CommitSourceRefresh(sourceID int64, incoming []NodeRecord, userinfoJSON string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := replaceSourceNodesTx(tx, sourceID, incoming); err != nil {
			return err
		}
		now := nowRFC3339()
		cache := Cache{
			SourceID: sourceID, UserinfoJSON: userinfoJSON,
			LastSuccessAt: now, UpdatedAt: now,
		}
		return putJSON(tx.Bucket([]byte("source_cache")), itob(sourceID), cache)
	})
}

func replaceSourceNodesTx(tx *bolt.Tx, sourceID int64, incoming []NodeRecord) error {
	b := tx.Bucket([]byte("nodes"))
	existing := map[string]NodeRecord{}
	var oldKeys [][]byte
	if err := b.ForEach(func(k, v []byte) error {
		var node NodeRecord
		if err := json.Unmarshal(v, &node); err != nil {
			return err
		}
		if node.SourceID == sourceID {
			existing[node.Fingerprint] = node
			oldKeys = append(oldKeys, append([]byte(nil), k...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range oldKeys {
		if err := b.Delete(key); err != nil {
			return err
		}
	}
	now := nowRFC3339()
	seen := map[string]bool{}
	for _, node := range incoming {
		if node.Fingerprint == "" || seen[node.Fingerprint] {
			continue
		}
		seen[node.Fingerprint] = true
		if old, ok := existing[node.Fingerprint]; ok {
			node.ID = old.ID
			node.Alias = old.Alias
			node.Tags = old.Tags
			node.Enabled = old.Enabled
			node.CreatedAt = old.CreatedAt
			node.RoleOverride = old.RoleOverride
		} else {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			node.ID = int64(seq)
			node.Enabled = true
			node.CreatedAt = now
		}
		node.SourceID = sourceID
		if node.Role == "" {
			node.Role = "proxy"
		}
		if node.RoleOverride != "" {
			node.Role = node.RoleOverride
		}
		node.UpdatedAt = now
		if err := putJSON(b, itob(node.ID), node); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateManualNode(node NodeRecord) (int64, error) {
	var id int64
	err := s.db.Update(func(tx *bolt.Tx) error {
		sourceValue := tx.Bucket([]byte("sources")).Get(itob(node.SourceID))
		if sourceValue == nil {
			return fmt.Errorf("no source with id %d", node.SourceID)
		}
		var source Source
		if err := json.Unmarshal(sourceValue, &source); err != nil {
			return err
		}
		if source.Kind != SourceKindManual {
			return fmt.Errorf("source %d is not manual", node.SourceID)
		}
		b := tx.Bucket([]byte("nodes"))
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		id = int64(seq)
		now := nowRFC3339()
		node.ID, node.Origin, node.Enabled = id, SourceKindManual, true
		node.CreatedAt, node.UpdatedAt = now, now
		return putJSON(b, itob(id), node)
	})
	return id, err
}

func (s *Store) CreateManualNodes(nodes []NodeRecord) ([]int64, error) {
	ids := make([]int64, 0, len(nodes))
	err := s.db.Update(func(tx *bolt.Tx) error {
		sources := tx.Bucket([]byte("sources"))
		b := tx.Bucket([]byte("nodes"))
		now := nowRFC3339()
		known := map[string]bool{}
		if err := b.ForEach(func(_, raw []byte) error {
			var existing NodeRecord
			if err := json.Unmarshal(raw, &existing); err != nil {
				return err
			}
			known[fmt.Sprintf("%d\x00%s", existing.SourceID, existing.Fingerprint)] = true
			return nil
		}); err != nil {
			return err
		}
		for _, node := range nodes {
			value := sources.Get(itob(node.SourceID))
			if value == nil {
				return fmt.Errorf("no source with id %d", node.SourceID)
			}
			var source Source
			if err := json.Unmarshal(value, &source); err != nil {
				return err
			}
			if source.Kind != SourceKindManual {
				return fmt.Errorf("source %d is not manual", node.SourceID)
			}
			key := fmt.Sprintf("%d\x00%s", node.SourceID, node.Fingerprint)
			if node.Fingerprint == "" {
				return fmt.Errorf("manual node fingerprint is required")
			}
			if known[key] {
				continue
			}
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			node.ID, node.Origin, node.Enabled = int64(seq), SourceKindManual, true
			node.CreatedAt, node.UpdatedAt = now, now
			if err := putJSON(b, itob(node.ID), node); err != nil {
				return err
			}
			known[key] = true
			ids = append(ids, node.ID)
		}
		return nil
	})
	return ids, err
}

func (s *Store) UpdateNodeMetadata(id int64, alias string, tags []string, enabled bool) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("nodes"))
		v := b.Get(itob(id))
		if v == nil {
			return fmt.Errorf("no node with id %d", id)
		}
		var node NodeRecord
		if err := json.Unmarshal(v, &node); err != nil {
			return err
		}
		node.Alias, node.Tags, node.Enabled, node.UpdatedAt = alias, tags, enabled, nowRFC3339()
		return putJSON(b, itob(id), node)
	})
}

// CommitSourceRefreshV3 atomically stores normalized nodes and structured
// lifecycle metadata. When preserveProxySnapshot is true only the notice
// portion is replaced; this keeps a last-known proxy snapshot when an expired
// provider returns informational pseudo-nodes only.
func (s *Store) CommitSourceRefreshV3(sourceID int64, incoming []NodeRecord, userinfoJSON string, metadata SubscriptionMetadata, preserveProxySnapshot bool) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := replaceSourceNodesV3Tx(tx, sourceID, incoming, preserveProxySnapshot); err != nil {
			return err
		}
		now := nowRFC3339()
		cache := Cache{
			SourceID: sourceID, UserinfoJSON: userinfoJSON, Metadata: metadata,
			LastSuccessAt: now, UpdatedAt: now,
		}
		return putJSON(tx.Bucket([]byte("source_cache")), itob(sourceID), cache)
	})
}

func replaceSourceNodesV3Tx(tx *bolt.Tx, sourceID int64, incoming []NodeRecord, preserveProxySnapshot bool) error {
	if !preserveProxySnapshot {
		return replaceSourceNodesTx(tx, sourceID, incoming)
	}
	b := tx.Bucket([]byte("nodes"))
	var kept, incomingNotices []NodeRecord
	if err := b.ForEach(func(_, raw []byte) error {
		var value NodeRecord
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.SourceID == sourceID && value.Role != "notice" {
			kept = append(kept, value)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, value := range incoming {
		if value.Role == "notice" {
			incomingNotices = append(incomingNotices, value)
		}
	}
	return replaceSourceNodesTx(tx, sourceID, append(incomingNotices, kept...))
}

func (s *Store) SetNodeRoleOverride(id int64, role string) error {
	if role != "" && role != "proxy" && role != "notice" {
		return fmt.Errorf("node role override must be proxy, notice or empty")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("nodes"))
		v := b.Get(itob(id))
		if v == nil {
			return fmt.Errorf("no node with id %d", id)
		}
		var node NodeRecord
		if err := json.Unmarshal(v, &node); err != nil {
			return err
		}
		if node.Origin != SourceKindSubscription {
			return fmt.Errorf("manual nodes are always proxy nodes")
		}
		node.RoleOverride = role
		if role != "" {
			node.Role = role
		} else if node.Notice != nil && node.Notice.Confidence == "high" {
			node.Role = "notice"
		} else {
			node.Role = "proxy"
		}
		node.UpdatedAt = nowRFC3339()
		return putJSON(b, itob(id), node)
	})
}

func (s *Store) DeleteNode(id int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("nodes"))
		v := b.Get(itob(id))
		if v == nil {
			return fmt.Errorf("no node with id %d", id)
		}
		var node NodeRecord
		if err := json.Unmarshal(v, &node); err != nil {
			return err
		}
		if node.Origin != SourceKindManual {
			return fmt.Errorf("subscription nodes are deleted by refreshing their source")
		}
		return b.Delete(itob(id))
	})
}

func (s *Store) ReplaceManualNode(id int64, replacement NodeRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("nodes"))
		v := b.Get(itob(id))
		if v == nil {
			return fmt.Errorf("no node with id %d", id)
		}
		var old NodeRecord
		if err := json.Unmarshal(v, &old); err != nil {
			return err
		}
		if old.Origin != SourceKindManual {
			return fmt.Errorf("subscription nodes cannot be edited directly")
		}
		if replacement.Fingerprint == "" {
			return fmt.Errorf("manual node fingerprint is required")
		}
		if err := b.ForEach(func(k, raw []byte) error {
			if bytes.Equal(k, itob(id)) {
				return nil
			}
			var other NodeRecord
			if err := json.Unmarshal(raw, &other); err != nil {
				return err
			}
			if other.SourceID == old.SourceID && other.Fingerprint == replacement.Fingerprint {
				return fmt.Errorf("manual source already contains this node")
			}
			return nil
		}); err != nil {
			return err
		}
		replacement.ID, replacement.SourceID, replacement.Origin = id, old.SourceID, old.Origin
		replacement.Alias, replacement.Tags, replacement.Enabled = old.Alias, old.Tags, old.Enabled
		replacement.CreatedAt, replacement.UpdatedAt = old.CreatedAt, nowRFC3339()
		return putJSON(b, itob(id), replacement)
	})
}

func (s *Store) GetNode(id int64) (NodeRecord, error) {
	var node NodeRecord
	err := getJSONByID(s.db, "nodes", id, &node)
	return node, err
}

func (s *Store) ListNodes() ([]NodeRecord, error) {
	out := make([]NodeRecord, 0)
	err := listJSON(s.db, "nodes", func(v []byte) error {
		var node NodeRecord
		if err := json.Unmarshal(v, &node); err != nil {
			return err
		}
		out = append(out, node)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func (s *Store) SaveNodeSet(value NodeSet) (int64, error) {
	return saveWithID(s.db, "node_sets", value.ID, func(id int64, createdAt string) any {
		now := nowRFC3339()
		value.ID, value.UpdatedAt = id, now
		if createdAt == "" {
			value.CreatedAt = now
		} else {
			value.CreatedAt = createdAt
		}
		return value
	})
}

func (s *Store) GetNodeSet(id int64) (NodeSet, error) {
	var value NodeSet
	err := getJSONByID(s.db, "node_sets", id, &value)
	return value, err
}

func (s *Store) ListNodeSets() ([]NodeSet, error) {
	out := make([]NodeSet, 0)
	err := listJSON(s.db, "node_sets", func(v []byte) error {
		var value NodeSet
		if err := json.Unmarshal(v, &value); err != nil {
			return err
		}
		out = append(out, value)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func (s *Store) DeleteNodeSet(id int64) error { return deleteByID(s.db, "node_sets", id) }

func (s *Store) SaveTemplate(value Template) (int64, error) {
	return saveWithID(s.db, "templates", value.ID, func(id int64, createdAt string) any {
		now := nowRFC3339()
		value.ID, value.UpdatedAt = id, now
		if createdAt == "" {
			value.CreatedAt = now
		} else {
			value.CreatedAt = createdAt
		}
		return value
	})
}

func (s *Store) GetTemplate(id int64) (Template, error) {
	var value Template
	err := getJSONByID(s.db, "templates", id, &value)
	return value, err
}

func (s *Store) ListTemplates() ([]Template, error) {
	out := make([]Template, 0)
	err := listJSON(s.db, "templates", func(v []byte) error {
		var value Template
		if err := json.Unmarshal(v, &value); err != nil {
			return err
		}
		out = append(out, value)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func (s *Store) DeleteTemplate(id int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("templates"))
		if b.Get(itob(id)) == nil {
			return fmt.Errorf("no template with id %d", id)
		}
		versions := tx.Bucket([]byte("template_versions"))
		var keys [][]byte
		if err := versions.ForEach(func(k, v []byte) error {
			var item TemplateVersion
			if err := json.Unmarshal(v, &item); err != nil {
				return err
			}
			if item.TemplateID == id {
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range keys {
			if err := versions.Delete(key); err != nil {
				return err
			}
		}
		return b.Delete(itob(id))
	})
}

func (s *Store) PublishTemplateVersion(templateID int64, engineVersion, content string, slots []TemplateSlot) (TemplateVersion, error) {
	var result TemplateVersion
	err := s.db.Update(func(tx *bolt.Tx) error {
		tb := tx.Bucket([]byte("templates"))
		v := tb.Get(itob(templateID))
		if v == nil {
			return fmt.Errorf("no template with id %d", templateID)
		}
		var template Template
		if err := json.Unmarshal(v, &template); err != nil {
			return err
		}
		versions := tx.Bucket([]byte("template_versions"))
		version := 1
		if err := versions.ForEach(func(_, raw []byte) error {
			var item TemplateVersion
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if item.TemplateID == templateID && item.Version >= version {
				version = item.Version + 1
			}
			return nil
		}); err != nil {
			return err
		}
		seq, err := versions.NextSequence()
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(content + "\x00" + mustJSON(slots)))
		result = TemplateVersion{ID: int64(seq), TemplateID: templateID, Version: version, EngineVersion: engineVersion, Content: content, Slots: slots, Checksum: hex.EncodeToString(sum[:]), PublishedAt: nowRFC3339()}
		if err := putJSON(versions, itob(result.ID), result); err != nil {
			return err
		}
		template.CurrentVersionID, template.Status, template.UpdatedAt = result.ID, "published", nowRFC3339()
		return putJSON(tb, itob(template.ID), template)
	})
	return result, err
}

func (s *Store) GetTemplateVersion(id int64) (TemplateVersion, error) {
	var value TemplateVersion
	err := getJSONByID(s.db, "template_versions", id, &value)
	return value, err
}

func (s *Store) ListTemplateVersions(templateID int64) ([]TemplateVersion, error) {
	out := make([]TemplateVersion, 0)
	err := listJSON(s.db, "template_versions", func(v []byte) error {
		var value TemplateVersion
		if err := json.Unmarshal(v, &value); err != nil {
			return err
		}
		if value.TemplateID == templateID {
			out = append(out, value)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, err
}

func (s *Store) SaveProfile(value Profile) (int64, error) {
	var id int64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("profiles"))
		now, created := nowRFC3339(), ""
		oldToken := ""
		if value.ID == 0 {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			id = int64(seq)
		} else {
			id = value.ID
			v := b.Get(itob(id))
			if v == nil {
				return fmt.Errorf("no profile with id %d", id)
			}
			var old Profile
			if err := json.Unmarshal(v, &old); err != nil {
				return err
			}
			created, oldToken = old.CreatedAt, old.Token
		}
		if value.Token == "" {
			return fmt.Errorf("profile token is required")
		}
		index := tx.Bucket([]byte("token_index"))
		if owner := index.Get([]byte(value.Token)); owner != nil && !bytes.Equal(owner, itob(id)) {
			return fmt.Errorf("profile token already exists")
		}
		if oldToken != "" && oldToken != value.Token {
			if err := index.Delete([]byte(oldToken)); err != nil {
				return err
			}
		}
		value.ID, value.UpdatedAt = id, now
		if created == "" {
			value.CreatedAt = now
		} else {
			value.CreatedAt = created
		}
		if err := putJSON(b, itob(id), value); err != nil {
			return err
		}
		return index.Put([]byte(value.Token), itob(id))
	})
	return id, err
}

func (s *Store) GetProfile(id int64) (Profile, error) {
	var value Profile
	err := getJSONByID(s.db, "profiles", id, &value)
	return value, err
}

func (s *Store) GetProfileByToken(token string) (Profile, error) {
	var value Profile
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("token_index")).Get([]byte(token))
		if id == nil {
			return fmt.Errorf("profile token not found")
		}
		v := tx.Bucket([]byte("profiles")).Get(id)
		if v == nil {
			return fmt.Errorf("profile token index is stale")
		}
		return json.Unmarshal(v, &value)
	})
	return value, err
}

func (s *Store) ListProfiles() ([]Profile, error) {
	out := make([]Profile, 0)
	err := listJSON(s.db, "profiles", func(v []byte) error {
		var value Profile
		if err := json.Unmarshal(v, &value); err != nil {
			return err
		}
		out = append(out, value)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func (s *Store) DeleteProfile(id int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("profiles"))
		v := b.Get(itob(id))
		if v == nil {
			return fmt.Errorf("no profile with id %d", id)
		}
		var profile Profile
		if err := json.Unmarshal(v, &profile); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("token_index")).Delete([]byte(profile.Token)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("profile_artifacts")).Delete(itob(id)); err != nil {
			return err
		}
		return b.Delete(itob(id))
	})
}

func (s *Store) PutProfileArtifact(value ProfileArtifact) error {
	value.UpdatedAt = nowRFC3339()
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket([]byte("profile_artifacts")), itob(value.ProfileID), value)
	})
}

func (s *Store) GetProfileArtifact(profileID int64) (ProfileArtifact, error) {
	var value ProfileArtifact
	err := getJSONByID(s.db, "profile_artifacts", profileID, &value)
	return value, err
}

func saveWithID(db *bolt.DB, bucket string, id int64, build func(int64, string) any) (int64, error) {
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		created := ""
		if id == 0 {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			id = int64(seq)
		} else {
			v := b.Get(itob(id))
			if v == nil {
				return fmt.Errorf("no %s record with id %d", bucket, id)
			}
			var stamp struct {
				CreatedAt string `json:"created_at"`
			}
			if err := json.Unmarshal(v, &stamp); err != nil {
				return err
			}
			created = stamp.CreatedAt
		}
		return putJSON(b, itob(id), build(id, created))
	})
	return id, err
}

func getJSONByID(db *bolt.DB, bucket string, id int64, target any) error {
	found := false
	err := db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucket)).Get(itob(id))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, target)
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no %s record with id %d", bucket, id)
	}
	return nil
}

func listJSON(db *bolt.DB, bucket string, fn func([]byte) error) error {
	return db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucket)).ForEach(func(_, v []byte) error { return fn(v) })
	})
}

func deleteByID(db *bolt.DB, bucket string, id int64) error {
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b.Get(itob(id)) == nil {
			return fmt.Errorf("no %s record with id %d", bucket, id)
		}
		return b.Delete(itob(id))
	})
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func NormalizeStringSet(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func NormalizeIntSet(values []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value > 0 && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
