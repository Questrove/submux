package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.Content) == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	if body.SourceID == 0 {
		var err error
		body.SourceID, err = s.store.EnsureDefaultManualSource()
		if err != nil {
			http.Error(w, "prepare built-in node group failed", http.StatusInternalServerError)
			return
		}
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
	writeJSON(w, map[string]any{"ids": ids, "count": len(ids), "source_id": body.SourceID})
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
	tags := current.Tags
	if body.Tags != nil {
		tags = store.NormalizeStringSet(*body.Tags)
	}
	resultingRole := current.Role
	if body.RoleOverride != nil {
		role := strings.TrimSpace(*body.RoleOverride)
		if role != "" {
			resultingRole = role
		} else if current.Notice != nil && current.Notice.Confidence == "high" {
			resultingRole = "notice"
		} else {
			resultingRole = "proxy"
		}
	}
	if resultingRole == "notice" {
		enabled = false
	}
	if err := s.store.UpdateNodeMetadata(id, tags, enabled); err != nil {
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
	subscriptions, _ := s.store.ListOutputSubscriptions()
	for _, subscription := range subscriptions {
		for _, binding := range subscription.Bindings {
			if containsInt64(binding.NodeIDs, id) {
				http.Error(w, fmt.Sprintf("node is used by output subscription %d", subscription.ID), http.StatusConflict)
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

func containsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
	subscriptions, _ := s.store.ListOutputSubscriptions()
	versions, _ := s.store.ListTemplateVersions(id)
	versionIDs := map[int64]bool{}
	for _, version := range versions {
		versionIDs[version.ID] = true
	}
	for _, subscription := range subscriptions {
		if versionIDs[subscription.TemplateVersionID] {
			http.Error(w, fmt.Sprintf("template is used by output subscription %d", subscription.ID), http.StatusConflict)
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
		EngineVersion string `json:"engine_version"`
		Content       string `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	slots, err := compiler.InferTemplateSlots(template.Engine, body.Content)
	if err != nil {
		http.Error(w, "invalid template: "+err.Error(), http.StatusBadRequest)
		return
	}
	version, err := s.store.PublishTemplateVersion(id, strings.TrimSpace(body.EngineVersion), body.Content, slots)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, version)
}

func (s *Server) handleListOutputSubscriptions(w http.ResponseWriter, _ *http.Request) {
	values, err := s.store.ListOutputSubscriptions()
	if err != nil {
		http.Error(w, "list output subscriptions failed", http.StatusInternalServerError)
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
		store.OutputSubscription
		Artifact *artifactStatus `json:"artifact,omitempty"`
		URL      string          `json:"url"`
	}
	base, _ := s.store.GetSetting("base_url")
	out := make([]item, 0, len(values))
	for _, value := range values {
		entry := item{OutputSubscription: value, URL: "/sub/" + value.Token}
		if artifact, err := s.store.GetSubscriptionArtifact(value.ID); err == nil {
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

func (s *Server) handleSaveOutputSubscription(w http.ResponseWriter, r *http.Request) {
	var value store.OutputSubscription
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
	if value.Engine == compiler.EngineMihomo {
		if value.RuleProfileID == 0 && value.ID != 0 {
			if old, oldErr := s.store.GetOutputSubscription(value.ID); oldErr == nil {
				value.RuleProfileID = old.RuleProfileID
			}
		}
		if value.RuleProfileID == 0 {
			if defaultProfile, defaultErr := s.store.GetRuleProfileByKey("default"); defaultErr == nil {
				value.RuleProfileID = defaultProfile.ID
			}
		}
		profile, profileErr := s.store.GetRuleProfile(value.RuleProfileID)
		if profileErr != nil {
			http.Error(w, "a valid rule profile is required for mihomo subscriptions", http.StatusBadRequest)
			return
		}
		if err := s.compiler.ValidateRuleProfile(profile); err != nil {
			http.Error(w, "invalid rule profile: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else if value.RuleProfileID != 0 {
		http.Error(w, "rule profiles are only supported by mihomo subscriptions", http.StatusBadRequest)
		return
	}
	seenSlots := map[string]bool{}
	for index := range value.Bindings {
		value.Bindings[index].Slot = strings.TrimSpace(value.Bindings[index].Slot)
		if value.Bindings[index].Slot == "" || seenSlots[value.Bindings[index].Slot] {
			http.Error(w, "each template slot may be selected once", http.StatusBadRequest)
			return
		}
		seenSlots[value.Bindings[index].Slot] = true
		seenNodes := map[int64]bool{}
		for _, nodeID := range value.Bindings[index].NodeIDs {
			if nodeID <= 0 || seenNodes[nodeID] {
				http.Error(w, fmt.Sprintf("slot %q contains an invalid or duplicate node", value.Bindings[index].Slot), http.StatusBadRequest)
				return
			}
			seenNodes[nodeID] = true
			nodeValue, nodeErr := s.store.GetNode(nodeID)
			if nodeErr != nil {
				http.Error(w, fmt.Sprintf("unknown node %d", nodeID), http.StatusBadRequest)
				return
			}
			if nodeValue.Role == "notice" {
				http.Error(w, fmt.Sprintf("node %d is an informational notice", nodeID), http.StatusBadRequest)
				return
			}
		}
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
		old, err := s.store.GetOutputSubscription(value.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		value.Token = old.Token
	}
	if _, err := s.compiler.Preview(value); err != nil {
		http.Error(w, "output subscription does not compile: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.store.SaveOutputSubscription(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.compiler.CompileAndStore(id); err != nil {
		http.Error(w, "output subscription saved but compile failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "token": value.Token})
}

func (s *Server) handlePreviewOutputSubscription(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	value, err := s.store.GetOutputSubscription(id)
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

func (s *Server) handlePublishOutputSubscription(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleResetOutputSubscriptionToken(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	value, err := s.store.GetOutputSubscription(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	value.Token = randomHex(24)
	if _, err := s.store.SaveOutputSubscription(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"token": value.Token})
}

func (s *Server) handleSetOutputSubscriptionEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	value, err := s.store.GetOutputSubscription(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if body.Enabled {
		value.Enabled = true
		if _, err := s.compiler.Preview(value); err != nil {
			http.Error(w, "output subscription cannot be enabled: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		value.Enabled = false
	}
	if _, err := s.store.SaveOutputSubscription(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if body.Enabled {
		if _, err := s.compiler.CompileAndStore(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteOutputSubscription(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteOutputSubscription(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func decodeJSON(r *http.Request, target any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, (10<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 10<<20 {
		return fmt.Errorf("JSON request exceeds 10 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
