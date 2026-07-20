package rulecatalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"submux/internal/store"
)

func TestEmbeddedCatalogContainsFullMetaSnapshotAndCommonRules(t *testing.T) {
	catalog := Catalog()
	if catalog.Source != "MetaCubeX/meta-rules-dat" || len(catalog.Commit) != 40 {
		t.Fatalf("unexpected catalog source: %+v", catalog)
	}
	if len(catalog.Entries) < 2000 {
		t.Fatalf("catalog snapshot is incomplete: %d entries", len(catalog.Entries))
	}
	for _, key := range []string{
		"geosite/microsoft@cn", "geosite/apple-cn", "geosite/apple-music",
		"geosite/netflix", "geosite/steam@cn", "geosite/category-ads-all",
		"geoip/cn", "geoip/netflix",
	} {
		if _, ok := Lookup(key); !ok {
			t.Fatalf("catalog is missing %q", key)
		}
	}
}

func TestDefaultProfileUsesSpecificRulesBeforeBroadRules(t *testing.T) {
	profile := DefaultProfile()
	positions := map[string]int{}
	for index, rule := range profile.Rules {
		positions[rule.Key] = index
	}
	for specific, broad := range map[string]string{
		"geosite/microsoft@cn": "geosite/microsoft",
		"geosite/onedrive":     "geosite/microsoft",
		"geosite/apple-cn":     "geosite/apple",
		"geosite/apple-music":  "geosite/apple",
		"geosite/steam@cn":     "geosite/steam",
	} {
		if positions[specific] >= positions[broad] {
			t.Fatalf("specific rule %q must precede %q", specific, broad)
		}
	}
	if len(profile.CustomRules) != 2 || profile.CustomRules[0].Value != "example.com" || profile.CustomRules[1].Value != "example.net" {
		t.Fatalf("default direct-domain overrides are missing: %+v", profile.CustomRules)
	}
}

func TestRefreshStoresValidatedSnapshotAndMakesItActive(t *testing.T) {
	st := newCatalogStore(t)
	commit := strings.Repeat("a", 40)
	tree := completeCatalogTree()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "57")
		switch r.URL.Path {
		case "/branches/meta":
			w.Header().Set("ETag", `"catalog-a"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": commit}})
		case "/git/trees/" + commit:
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": tree, "truncated": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, state, err := refreshFrom(context.Background(), st, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	active, err := ActiveCatalog(st)
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit != commit || active.Commit != commit || result.Origin != "github" || len(result.Entries) < 100 {
		t.Fatalf("refreshed catalog was not activated: result=%+v active=%+v", result, active)
	}
	if state.ETag != `"catalog-a"` || state.CatalogCommit != commit || state.RateRemaining != 57 || state.LastSuccessAt == "" || state.LastError != "" {
		t.Fatalf("refresh state is incomplete: %+v", state)
	}
}

func TestRefreshKeepsSuccessfulValidatorsWhenTreeDownloadFails(t *testing.T) {
	st := newCatalogStore(t)
	previous := RefreshState{ETag: `"catalog-old"`, LastModified: "old-date", LastSuccessAt: "previous-success"}
	raw, _ := json.Marshal(previous)
	if err := st.SetSetting(refreshStateSetting, string(raw)); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/branches/meta":
			if r.Header.Get("If-None-Match") != previous.ETag {
				t.Errorf("previous validator was not sent: %q", r.Header.Get("If-None-Match"))
			}
			w.Header().Set("ETag", `"catalog-new"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": commit}})
		case "/git/trees/" + commit:
			http.Error(w, "temporary failure", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	current, state, err := refreshFrom(context.Background(), st, server.URL)
	if err == nil {
		t.Fatal("tree download failure was accepted")
	}
	if current.Commit != Commit() || state.ETag != previous.ETag || state.LastModified != previous.LastModified || state.LastSuccessAt != previous.LastSuccessAt {
		t.Fatalf("failed refresh replaced last-good state: current=%+v state=%+v", current, state)
	}
}

func newCatalogStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func completeCatalogTree() []map[string]string {
	seen := map[string]bool{}
	result := make([]map[string]string, 0, 160)
	add := func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		parts := strings.SplitN(key, "/", 2)
		result = append(result, map[string]string{"path": "geo/" + parts[0] + "/" + parts[1] + ".mrs", "type": "blob"})
	}
	for _, key := range defaultOrder {
		add(key)
	}
	for _, entry := range Catalog().Entries {
		add(entry.Key)
		if len(result) >= 160 {
			break
		}
	}
	return result
}
