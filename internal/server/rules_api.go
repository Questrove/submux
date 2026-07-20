package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"submux/internal/rulecatalog"
	"submux/internal/store"
)

func (s *Server) handleRuleCatalog(w http.ResponseWriter, r *http.Request) {
	commit := strings.TrimSpace(r.URL.Query().Get("commit"))
	var catalog rulecatalog.Snapshot
	var err error
	if commit == "" {
		catalog, err = rulecatalog.ActiveCatalog(s.store)
	} else {
		catalog, err = rulecatalog.CatalogAt(s.store, commit)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	active, _ := rulecatalog.ActiveCatalog(s.store)
	writeJSON(w, struct {
		rulecatalog.Snapshot
		ActiveCommit   string                   `json:"active_commit"`
		EmbeddedCommit string                   `json:"embedded_commit"`
		Refresh        rulecatalog.RefreshState `json:"refresh"`
	}{Snapshot: catalog, ActiveCommit: active.Commit, EmbeddedCommit: rulecatalog.Commit(), Refresh: rulecatalog.LoadRefreshState(s.store)})
}

func (s *Server) handleRefreshRuleCatalog(w http.ResponseWriter, r *http.Request) {
	catalog, state, err := rulecatalog.Refresh(r.Context(), s.store)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "catalog": catalog, "refresh": state})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "catalog": catalog, "refresh": state})
}

func (s *Server) handleListRuleProfiles(w http.ResponseWriter, _ *http.Request) {
	profiles, err := s.store.ListRuleProfiles()
	if err != nil {
		http.Error(w, "list rule profiles failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, profiles)
}

func (s *Server) handleSaveRuleProfile(w http.ResponseWriter, r *http.Request) {
	var value store.RuleProfile
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
		old, err := s.store.GetRuleProfile(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		value.ID, value.Key, value.Builtin, value.CatalogCommit = id, old.Key, old.Builtin, old.CatalogCommit
	} else {
		value.ID, value.Key, value.Builtin = 0, "", false
		catalog, err := rulecatalog.ActiveCatalog(s.store)
		if err != nil {
			http.Error(w, "load active rule catalog failed", http.StatusInternalServerError)
			return
		}
		value.CatalogCommit = catalog.Commit
	}
	value.Name = strings.TrimSpace(value.Name)
	value.Description = strings.TrimSpace(value.Description)
	value.FallbackAction = strings.TrimSpace(value.FallbackAction)
	if value.Name == "" {
		http.Error(w, "rule profile name is required", http.StatusBadRequest)
		return
	}
	for index := range value.Rules {
		value.Rules[index].Key = strings.TrimSpace(value.Rules[index].Key)
		value.Rules[index].Action = strings.TrimSpace(value.Rules[index].Action)
	}
	for index := range value.CustomRules {
		value.CustomRules[index].Type = strings.ToUpper(strings.TrimSpace(value.CustomRules[index].Type))
		value.CustomRules[index].Value = strings.TrimSpace(value.CustomRules[index].Value)
		value.CustomRules[index].Action = strings.TrimSpace(value.CustomRules[index].Action)
	}
	if err := s.compiler.ValidateRuleProfile(value); err != nil {
		http.Error(w, "invalid rule profile: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.store.SaveRuleProfile(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rebuildErr := s.compiler.RebuildAll()
	result := map[string]any{"id": id}
	if rebuildErr != nil {
		result["rebuild_error"] = rebuildErr.Error()
	}
	writeJSON(w, result)
}

func (s *Server) handleUpdateRuleProfileCatalog(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	profile, err := s.store.GetRuleProfile(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	catalog, err := rulecatalog.ActiveCatalog(s.store)
	if err != nil {
		http.Error(w, "load active rule catalog failed", http.StatusInternalServerError)
		return
	}
	missing := make([]string, 0)
	for _, selection := range profile.Rules {
		if _, ok := rulecatalog.LookupIn(catalog, selection.Key); !ok {
			missing = append(missing, selection.Key)
		}
	}
	if len(missing) > 0 {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"ok": false, "missing": missing, "current_commit": profile.CatalogCommit, "target_commit": catalog.Commit})
		return
	}
	previous := profile.CatalogCommit
	profile.CatalogCommit = catalog.Commit
	if _, err := s.store.SaveRuleProfile(profile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rebuildErr := s.compiler.RebuildAll()
	result := map[string]any{"ok": true, "previous_commit": previous, "catalog_commit": catalog.Commit}
	if rebuildErr != nil {
		result["rebuild_error"] = rebuildErr.Error()
	}
	writeJSON(w, result)
}

func (s *Server) handleDeleteRuleProfile(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	profile, err := s.store.GetRuleProfile(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if profile.Builtin {
		http.Error(w, "the built-in rule profile cannot be deleted", http.StatusConflict)
		return
	}
	subscriptions, _ := s.store.ListOutputSubscriptions()
	for _, subscription := range subscriptions {
		if subscription.RuleProfileID == id {
			http.Error(w, fmt.Sprintf("rule profile is used by output subscription %d", subscription.ID), http.StatusConflict)
			return
		}
	}
	if err := s.store.DeleteRuleProfile(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
