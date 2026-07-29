package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"submux/internal/compiler"
	"submux/internal/node"
	"submux/internal/outputsubscription"
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
	result := map[string]any{"ids": ids, "count": len(ids), "source_id": body.SourceID}
	addOutputUpdateResult(result, s.updater.AttemptPending())
	writeJSON(w, result)
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
	result := map[string]any{"ok": true}
	addOutputUpdateResult(result, s.updater.AttemptPending())
	writeJSON(w, result)
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteNode(id); err != nil {
		writeResourceDeletionError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "outcome": "completed"})
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
	result := map[string]any{"id": id}
	addOutputUpdateResult(result, s.updater.AttemptPending())
	writeJSON(w, result)
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteTemplate(id); err != nil {
		writeResourceDeletionError(w, err)
		return
	}
	result := map[string]any{"ok": true}
	addOutputUpdateResult(result, s.updater.AttemptPending())
	writeJSON(w, result)
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
	writeJSON(w, struct {
		store.TemplateVersion
		Outcome string `json:"outcome"`
	}{TemplateVersion: version, Outcome: "completed"})
}

func (s *Server) handleListOutputSubscriptions(w http.ResponseWriter, _ *http.Request) {
	values, err := s.store.ListOutputSubscriptions()
	if err != nil {
		http.Error(w, "list output subscriptions failed", http.StatusInternalServerError)
		return
	}
	type artifactStatus struct {
		ContentType string `json:"content_type,omitempty"`
		Revision    string `json:"revision,omitempty"`
		LastSuccess string `json:"last_success,omitempty"`
		UpdatedAt   string `json:"updated_at,omitempty"`
	}
	type item struct {
		store.OutputSubscription
		Artifact *artifactStatus                `json:"artifact,omitempty"`
		Update   *store.SubscriptionUpdateState `json:"update,omitempty"`
		URL      string                         `json:"url"`
		Scenario string                         `json:"scenario,omitempty"`
	}
	base, _ := s.store.GetSetting("base_url")
	out := make([]item, 0, len(values))
	for _, value := range values {
		entry := item{OutputSubscription: value, URL: "/sub/" + value.Token}
		if version, versionErr := s.store.GetTemplateVersion(value.TemplateVersionID); versionErr == nil {
			if template, templateErr := s.store.GetTemplate(version.TemplateID); templateErr == nil {
				entry.Scenario = template.Scenario
			}
		}
		if artifact, err := s.store.GetSubscriptionArtifact(value.ID); err == nil {
			entry.Artifact = &artifactStatus{
				ContentType: artifact.ContentType, Revision: artifact.Revision,
				LastSuccess: artifact.LastSuccess, UpdatedAt: artifact.UpdatedAt,
			}
		}
		if update, err := s.store.GetSubscriptionUpdate(value.ID); err == nil {
			entry.Update = &update
		}
		if base != "" {
			entry.URL = strings.TrimRight(base, "/") + "/sub/" + value.Token
		}
		out = append(out, entry)
	}
	writeJSON(w, out)
}

type outputSubscriptionSaveRequest struct {
	outputsubscription.SaveIntent
	Engine        string `json:"engine,omitempty"`
	Token         string `json:"token,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	RecordVersion uint64 `json:"record_version,omitempty"`
}

func (s *Server) handleSaveOutputSubscription(w http.ResponseWriter, r *http.Request) {
	var body outputSubscriptionSaveRequest
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	intent := body.SaveIntent
	if routeID := chi.URLParam(r, "id"); routeID != "" {
		id, err := strconv.ParseInt(routeID, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		intent.ID = id
	}
	result, err := s.publisher.Save(intent)
	if err != nil {
		writeOutputSubscriptionError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"id": result.SubscriptionID, "token": result.Token,
		"outcome": result.Outcome, "revision": result.Revision,
	})
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
	publication, err := s.publisher.Republish(id)
	if err != nil {
		writeOutputSubscriptionError(w, err)
		return
	}
	outcome := publication.Outcome
	if outcome != outputsubscription.OutcomeCompleted {
		outcome = outputsubscription.OutcomeDegraded
	}
	result := map[string]any{"ok": true, "outcome": outcome}
	if publication.Outcome != outputsubscription.OutcomeCompleted || publication.Detail != "" {
		result["output_update"] = map[string]any{
			"results": []map[string]any{{
				"subscription_id": publication.SubscriptionID,
				"outcome":         publication.Outcome,
				"error":           publication.Detail,
			}},
		}
	}
	writeJSON(w, result)
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
	if err := s.store.UpdateOutputSubscriptionToken(id, value.Token); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"token": value.Token, "outcome": "completed"})
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
	result, err := s.publisher.SetEnabled(id, body.Enabled)
	if err != nil {
		writeOutputSubscriptionError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "outcome": result.Outcome, "revision": result.Revision,
	})
}

func writeOutputSubscriptionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, outputsubscription.ErrInvalidIntent):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, outputsubscription.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, outputsubscription.ErrConflict):
		http.Error(w, "output subscription changed concurrently; reload and retry", http.StatusConflict)
	default:
		http.Error(w, "output subscription operation failed", http.StatusInternalServerError)
	}
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
