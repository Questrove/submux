package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"submux/internal/compiler"
	"submux/internal/node"
	"submux/internal/store"
)

func (s *Server) handleListNodes(w http.ResponseWriter, _ *http.Request) {
	values, err := s.store.ListNodes()
	if err != nil {
		http.Error(w, "list nodes failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, values)
}

func (s *Server) handleImportNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceID int64  `json:"source_id"`
		Content  string `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil || body.SourceID == 0 || strings.TrimSpace(body.Content) == "" {
		http.Error(w, "source_id and content are required", http.StatusBadRequest)
		return
	}
	records, err := node.Import(body.SourceID, store.SourceKindManual, body.Content)
	if err != nil {
		http.Error(w, "invalid node input: "+err.Error(), http.StatusBadRequest)
		return
	}
	ids, err := s.store.CreateManualNodes(records)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.compiler.RebuildAll()
	writeJSON(w, map[string]any{"ids": ids, "count": len(ids)})
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	current, err := s.store.GetNode(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var body struct {
		Alias        *string   `json:"alias"`
		Tags         *[]string `json:"tags"`
		Enabled      *bool     `json:"enabled"`
		RoleOverride *string   `json:"role_override"`
		Content      string    `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.RoleOverride != nil {
		role := strings.TrimSpace(*body.RoleOverride)
		if role != "" && role != "proxy" && role != "notice" {
			http.Error(w, "role_override must be proxy, notice or empty", http.StatusBadRequest)
			return
		}
		if current.Origin != store.SourceKindSubscription {
			http.Error(w, "manual nodes are always proxy nodes", http.StatusBadRequest)
			return
		}
	}
	if strings.TrimSpace(body.Content) != "" {
		records, parseErr := node.Import(current.SourceID, store.SourceKindManual, body.Content)
		if parseErr != nil || len(records) != 1 {
			http.Error(w, "content must contain exactly one valid node", http.StatusBadRequest)
			return
		}
		if err := s.store.ReplaceManualNode(id, records[0]); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		current, _ = s.store.GetNode(id)
	}
	enabled := current.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	alias, tags := current.Alias, current.Tags
	if body.Alias != nil {
		alias = strings.TrimSpace(*body.Alias)
	}
	if body.Tags != nil {
		tags = store.NormalizeStringSet(*body.Tags)
	}
	if err := s.store.UpdateNodeMetadata(id, alias, tags, enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.RoleOverride != nil {
		if err := s.store.SetNodeRoleOverride(id, strings.TrimSpace(*body.RoleOverride)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	_ = s.compiler.RebuildAll()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	nodeSets, _ := s.store.ListNodeSets()
	for _, nodeSet := range nodeSets {
		for _, nodeID := range append(append([]int64(nil), nodeSet.NodeIDs...), nodeSet.ExcludeNodeIDs...) {
			if nodeID == id {
				http.Error(w, fmt.Sprintf("node is used by node set %d", nodeSet.ID), http.StatusConflict)
				return
			}
		}
	}
	if err := s.store.DeleteNode(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.compiler.RebuildAll()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleListNodeSets(w http.ResponseWriter, _ *http.Request) {
	values, err := s.store.ListNodeSets()
	if err != nil {
		http.Error(w, "list node sets failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, values)
}

func (s *Server) handleSaveNodeSet(w http.ResponseWriter, r *http.Request) {
	var value store.NodeSet
	if err := decodeJSON(r, &value); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if routeID := chi.URLParam(r, "id"); routeID != "" {
		id, err := strconv.ParseInt(routeID, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		value.ID = id
	}
	value.Name = strings.TrimSpace(value.Name)
	if value.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	value.SourceIDs = store.NormalizeIntSet(value.SourceIDs)
	value.NodeIDs = store.NormalizeIntSet(value.NodeIDs)
	value.ExcludeNodeIDs = store.NormalizeIntSet(value.ExcludeNodeIDs)
	value.Protocols, value.Tags = store.NormalizeStringSet(value.Protocols), store.NormalizeStringSet(value.Tags)
	for _, sourceID := range value.SourceIDs {
		if _, err := s.store.GetSource(sourceID); err != nil {
			http.Error(w, fmt.Sprintf("unknown source %d", sourceID), http.StatusBadRequest)
			return
		}
	}
	for _, nodeID := range append(append([]int64(nil), value.NodeIDs...), value.ExcludeNodeIDs...) {
		node, err := s.store.GetNode(nodeID)
		if err != nil {
			http.Error(w, fmt.Sprintf("unknown node %d", nodeID), http.StatusBadRequest)
			return
		}
		if node.Role == "notice" && containsInt64(value.NodeIDs, nodeID) {
			http.Error(w, fmt.Sprintf("node %d is an informational notice", nodeID), http.StatusBadRequest)
			return
		}
	}
	if value.ID == 0 {
		value.Enabled = true
	}
	id, err := s.store.SaveNodeSet(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.compiler.RebuildAll()
	writeJSON(w, map[string]any{"id": id})
}

func containsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Server) handleDeleteNodeSet(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	profiles, _ := s.store.ListProfiles()
	for _, profile := range profiles {
		for _, binding := range profile.Bindings {
			if binding.NodeSetID == id {
				http.Error(w, fmt.Sprintf("node set is used by profile %d", profile.ID), http.StatusConflict)
				return
			}
		}
	}
	if err := s.store.DeleteNodeSet(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleListTemplates(w http.ResponseWriter, _ *http.Request) {
	values, err := s.store.ListTemplates()
	if err != nil {
		http.Error(w, "list templates failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, values)
}

func (s *Server) handleSaveTemplate(w http.ResponseWriter, r *http.Request) {
	var value store.Template
	if err := decodeJSON(r, &value); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if routeID := chi.URLParam(r, "id"); routeID != "" {
		id, err := strconv.ParseInt(routeID, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		value.ID = id
	}
	value.Name, value.Engine, value.Scenario = strings.TrimSpace(value.Name), strings.TrimSpace(value.Engine), strings.TrimSpace(value.Scenario)
	if value.Name == "" || (value.Engine != compiler.EngineMihomo && value.Engine != compiler.EngineSingBox) || value.Scenario == "" {
		http.Error(w, "name, supported engine, and scenario are required", http.StatusBadRequest)
		return
	}
	if value.ID == 0 {
		value.Status = "draft"
	} else {
		old, err := s.store.GetTemplate(value.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		value.CurrentVersionID, value.Status = old.CurrentVersionID, old.Status
		if old.Engine != value.Engine && old.CurrentVersionID != 0 {
			http.Error(w, "published template engine cannot be changed", http.StatusConflict)
			return
		}
	}
	id, err := s.store.SaveTemplate(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	profiles, _ := s.store.ListProfiles()
	versions, _ := s.store.ListTemplateVersions(id)
	versionIDs := map[int64]bool{}
	for _, version := range versions {
		versionIDs[version.ID] = true
	}
	for _, profile := range profiles {
		if versionIDs[profile.TemplateVersionID] {
			http.Error(w, fmt.Sprintf("template is used by profile %d", profile.ID), http.StatusConflict)
			return
		}
	}
	if err := s.store.DeleteTemplate(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleListTemplateVersions(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	values, err := s.store.ListTemplateVersions(id)
	if err != nil {
		http.Error(w, "list versions failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, values)
}

func (s *Server) handlePublishTemplateVersion(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	template, err := s.store.GetTemplate(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var body struct {
		EngineVersion string               `json:"engine_version"`
		Content       string               `json:"content"`
		Slots         []store.TemplateSlot `json:"slots"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for index := range body.Slots {
		body.Slots[index].Key = strings.TrimSpace(body.Slots[index].Key)
		body.Slots[index].Target = strings.TrimSpace(body.Slots[index].Target)
		body.Slots[index].Mode = strings.TrimSpace(body.Slots[index].Mode)
	}
	if err := compiler.ValidateTemplate(template.Engine, body.Content, body.Slots); err != nil {
		http.Error(w, "invalid template: "+err.Error(), http.StatusBadRequest)
		return
	}
	version, err := s.store.PublishTemplateVersion(id, strings.TrimSpace(body.EngineVersion), body.Content, body.Slots)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, version)
}

func (s *Server) handleListProfiles(w http.ResponseWriter, _ *http.Request) {
	values, err := s.store.ListProfiles()
	if err != nil {
		http.Error(w, "list profiles failed", http.StatusInternalServerError)
		return
	}
	type artifactStatus struct {
		ContentType   string   `json:"content_type,omitempty"`
		Revision      string   `json:"revision,omitempty"`
		LastSuccess   string   `json:"last_success,omitempty"`
		LastError     string   `json:"last_error,omitempty"`
		UpdatedAt     string   `json:"updated_at,omitempty"`
		Warnings      []string `json:"warnings,omitempty"`
		BlockedReason string   `json:"blocked_reason,omitempty"`
	}
	type item struct {
		store.Profile
		Artifact *artifactStatus `json:"artifact,omitempty"`
		URL      string          `json:"url"`
	}
	base, _ := s.store.GetSetting("base_url")
	out := make([]item, 0, len(values))
	for _, value := range values {
		entry := item{Profile: value, URL: "/sub/" + value.Token}
		if artifact, err := s.store.GetProfileArtifact(value.ID); err == nil {
			entry.Artifact = &artifactStatus{
				ContentType: artifact.ContentType, Revision: artifact.Revision,
				LastSuccess: artifact.LastSuccess, LastError: artifact.LastError, UpdatedAt: artifact.UpdatedAt,
				Warnings: artifact.Warnings, BlockedReason: artifact.BlockedReason,
			}
		}
		if base != "" {
			entry.URL = strings.TrimRight(base, "/") + "/sub/" + value.Token
		}
		out = append(out, entry)
	}
	writeJSON(w, out)
}

func (s *Server) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	var value store.Profile
	if err := decodeJSON(r, &value); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if routeID := chi.URLParam(r, "id"); routeID != "" {
		id, err := strconv.ParseInt(routeID, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		value.ID = id
	}
	value.Name = strings.TrimSpace(value.Name)
	if value.Name == "" || value.TemplateVersionID == 0 {
		http.Error(w, "name and template_version_id are required", http.StatusBadRequest)
		return
	}
	version, err := s.store.GetTemplateVersion(value.TemplateVersionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	template, err := s.store.GetTemplate(version.TemplateID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	value.Engine = template.Engine
	for index := range value.Bindings {
		value.Bindings[index].Slot = strings.TrimSpace(value.Bindings[index].Slot)
	}
	if value.ExpiresAt != "" {
		expires, parseErr := time.Parse(time.RFC3339, value.ExpiresAt)
		if parseErr != nil || !expires.After(time.Now()) {
			http.Error(w, "expires_at must be a future RFC3339 timestamp", http.StatusBadRequest)
			return
		}
		value.ExpiresAt = expires.UTC().Format(time.RFC3339)
	}
	if value.ID == 0 {
		value.Token, value.Enabled = randomHex(24), true
	} else {
		old, err := s.store.GetProfile(value.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		value.Token = old.Token
	}
	if _, err := s.compiler.Preview(value); err != nil {
		http.Error(w, "profile does not compile: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.store.SaveProfile(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.compiler.CompileAndStore(id); err != nil {
		http.Error(w, "profile saved but compile failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "token": value.Token})
}

func (s *Server) handlePreviewProfile(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	value, err := s.store.GetProfile(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	result, err := s.compiler.Preview(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"content": string(result.Body), "content_type": result.ContentType, "revision": result.Revision, "node_count": result.NodeCount, "slot_counts": result.SlotCounts, "warnings": result.Warnings})
}

func (s *Server) handlePublishProfile(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	result, err := s.compiler.CompileAndStore(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "revision": result.Revision, "node_count": result.NodeCount, "warnings": result.Warnings})
}

func (s *Server) handleResetProfileToken(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	value, err := s.store.GetProfile(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	value.Token = randomHex(24)
	if _, err := s.store.SaveProfile(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"token": value.Token})
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteProfile(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
