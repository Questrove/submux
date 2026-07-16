package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"submux/internal/source"
	"submux/internal/store"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSourcesAPICRUD(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	c := initAndClient(t, srv)

	r := mustPost(t, c, srv.URL+"/api/sources", `{"name":"AirA","url":"http://x","user_agent":"ua"}`)
	if r.StatusCode != 200 {
		t.Fatalf("create status %d", r.StatusCode)
	}
	var cr struct{ ID int64 }
	json.NewDecoder(r.Body).Decode(&cr)
	r.Body.Close()
	if cr.ID == 0 {
		t.Fatalf("no id returned")
	}

	r2 := mustGet(t, c, srv.URL+"/api/sources")
	var list []map[string]any
	json.NewDecoder(r2.Body).Decode(&list)
	r2.Body.Close()
	if len(list) != 1 || list[0]["name"] != "AirA" || list[0]["kind"] != "subscription" {
		t.Fatalf("list wrong: %v", list)
	}

	req, _ := http.NewRequest("PUT", srv.URL+"/api/sources/"+itoa(cr.ID),
		strings.NewReader(`{"name":"AirA2","url":"http://x","user_agent":"ua","enabled":true,"sort_order":0}`))
	req.Header.Set("Content-Type", "application/json")
	r3 := mustDo(t, c, req)
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("update status %d", r3.StatusCode)
	}

	req2, _ := http.NewRequest("DELETE", srv.URL+"/api/sources/"+itoa(cr.ID), nil)
	r4 := mustDo(t, c, req2)
	r4.Body.Close()
	if r4.StatusCode != 200 {
		t.Fatalf("delete status %d", r4.StatusCode)
	}

	r5 := mustGet(t, c, srv.URL+"/api/sources")
	var list2 []map[string]any
	json.NewDecoder(r5.Body).Decode(&list2)
	r5.Body.Close()
	if len(list2) != 0 {
		t.Fatalf("expected empty after delete, got %v", list2)
	}
}

func TestSourceLifecycleAPI(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	c := initAndClient(t, srv)

	created := mustPost(t, c, srv.URL+"/api/sources", `{"name":"Air","url":"http://x"}`)
	var result struct{ ID int64 }
	_ = json.NewDecoder(created.Body).Decode(&result)
	created.Body.Close()
	metadata := store.SubscriptionMetadata{
		ExpiresAt:  time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		Remaining:  5 * 1024 * 1024 * 1024,
		Provenance: map[string]string{"expires_at": "header", "remaining": "header"},
	}
	if err := st.CommitSourceRefreshV3(result.ID, nil, "", metadata, false); err != nil {
		t.Fatal(err)
	}

	update, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/sources/"+itoa(result.ID), strings.NewReader(`{"name":"Air","url":"http://x","enabled":true,"lifecycle_policy":"strict","warn_before_days":3,"trust_node_notices":true}`))
	update.Header.Set("Content-Type", "application/json")
	updated := mustDo(t, c, update)
	updated.Body.Close()
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("lifecycle update status %d", updated.StatusCode)
	}

	response := mustGet(t, c, srv.URL+"/api/sources")
	var sources []struct {
		LifecyclePolicy  string `json:"lifecycle_policy"`
		WarnBeforeDays   int    `json:"warn_before_days"`
		TrustNodeNotices bool   `json:"trust_node_notices"`
		Lifecycle        struct {
			Entitlement    string `json:"entitlement"`
			RemainingBytes int64  `json:"remaining_bytes"`
		} `json:"lifecycle"`
	}
	_ = json.NewDecoder(response.Body).Decode(&sources)
	response.Body.Close()
	if len(sources) != 1 || sources[0].LifecyclePolicy != "strict" || sources[0].WarnBeforeDays != 3 || !sources[0].TrustNodeNotices || sources[0].Lifecycle.Entitlement != "expiring" {
		t.Fatalf("wrong lifecycle DTO: %+v", sources)
	}

	events := mustGet(t, c, srv.URL+"/api/lifecycle-events")
	if events.StatusCode != http.StatusOK {
		events.Body.Close()
		t.Fatalf("events status %d", events.StatusCode)
	}
	eventBody, _ := io.ReadAll(events.Body)
	events.Body.Close()
	if strings.TrimSpace(string(eventBody)) != "[]" {
		t.Fatalf("empty lifecycle events must be a JSON array, got %s", eventBody)
	}
}

func TestCreateSubscriptionRefreshesImmediately(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Subscription-Userinfo", "upload=0; download=0; total=1073741824")
		_, _ = io.WriteString(w, "vless://00000000-0000-0000-0000-000000000001@node.example.com:443?encryption=none&type=tcp#HK")
	}))
	defer upstream.Close()

	st := newTestStore(t)
	srv := httptest.NewServer(New(st, source.NewFetcher(st)).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)

	created := mustPost(t, client, srv.URL+"/api/sources", `{"name":"Air","url":"`+upstream.URL+`"}`)
	defer created.Body.Close()
	var result struct {
		ID        int64 `json:"id"`
		RefreshOK bool  `json:"refresh_ok"`
	}
	_ = json.NewDecoder(created.Body).Decode(&result)
	if created.StatusCode != http.StatusOK || result.ID == 0 || !result.RefreshOK {
		t.Fatalf("source was not created and refreshed: status=%d result=%+v", created.StatusCode, result)
	}

	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 || nodes[0].SourceID != result.ID || nodes[0].Name != "HK" {
		t.Fatalf("initial refresh did not persist nodes: err=%v nodes=%+v", err, nodes)
	}
	cache, err := st.GetCache(result.ID)
	if err != nil || cache.LastSuccessAt == "" || cache.LastError != "" {
		t.Fatalf("initial refresh did not persist success metadata: err=%v cache=%+v", err, cache)
	}
}

func TestCreateSubscriptionKeepsSourceWhenInitialRefreshFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	st := newTestStore(t)
	srv := httptest.NewServer(New(st, source.NewFetcher(st)).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)

	created := mustPost(t, client, srv.URL+"/api/sources", `{"name":"Air","url":"`+upstream.URL+`"}`)
	defer created.Body.Close()
	var result struct {
		ID           int64  `json:"id"`
		RefreshOK    bool   `json:"refresh_ok"`
		RefreshError string `json:"refresh_error"`
	}
	_ = json.NewDecoder(created.Body).Decode(&result)
	if created.StatusCode != http.StatusOK || result.ID == 0 || result.RefreshOK || result.RefreshError == "" {
		t.Fatalf("failed refresh result is incomplete: status=%d result=%+v", created.StatusCode, result)
	}
	if _, err := st.GetSource(result.ID); err != nil {
		t.Fatalf("source was deleted after initial refresh failure: %v", err)
	}
	cache, err := st.GetCache(result.ID)
	if err != nil || cache.LastError == "" || cache.LastSuccessAt != "" {
		t.Fatalf("initial refresh failure was not recorded: err=%v cache=%+v", err, cache)
	}
}

func TestCreateManualSourceDoesNotRefresh(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, source.NewFetcher(st)).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)

	created := mustPost(t, client, srv.URL+"/api/sources", `{"kind":"manual","name":"Local"}`)
	defer created.Body.Close()
	var result map[string]any
	_ = json.NewDecoder(created.Body).Decode(&result)
	if created.StatusCode != http.StatusOK || result["id"] == nil {
		t.Fatalf("manual source creation failed: status=%d result=%+v", created.StatusCode, result)
	}
	if _, exists := result["refresh_ok"]; exists {
		t.Fatalf("manual source unexpectedly reported a network refresh: %+v", result)
	}
}

func TestNodeMetadataAPIHasNoAliasAndPersistsDialogFields(t *testing.T) {
	st := newTestStore(t)
	sourceID, err := st.CreateSource(store.Source{Kind: store.SourceKindManual, Name: "Local"})
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := st.CreateManualNode(store.NodeRecord{
		SourceID: sourceID, Name: "Original name", Protocol: "vless",
		Config: json.RawMessage(`{"name":"Original name","type":"vless"}`), Fingerprint: "node-fingerprint",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)

	request, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/nodes/"+itoa(nodeID), strings.NewReader(`{"tags":["private","hk"],"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	response := mustDo(t, client, request)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("update node metadata: status %d", response.StatusCode)
	}
	stored, err := st.GetNode(nodeID)
	if err != nil || stored.Name != "Original name" || stored.Enabled || fmt.Sprint(stored.Tags) != "[hk private]" {
		t.Fatalf("node metadata was not persisted cleanly: value=%+v err=%v", stored, err)
	}

	legacy, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/nodes/"+itoa(nodeID), strings.NewReader(`{"alias":"removed"}`))
	legacy.Header.Set("Content-Type", "application/json")
	legacyResponse := mustDo(t, client, legacy)
	legacyResponse.Body.Close()
	if legacyResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("removed alias field should be rejected, got %d", legacyResponse.StatusCode)
	}
}

func TestManualNodeImportUsesProtectedBuiltinGroupByDefault(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)

	response := mustPost(t, client, srv.URL+"/api/nodes/import", `{"content":"vless://00000000-0000-0000-0000-000000000001@node.example.com:443?encryption=none&type=tcp#Self"}`)
	var result struct {
		SourceID int64 `json:"source_id"`
		Count    int   `json:"count"`
	}
	_ = json.NewDecoder(response.Body).Decode(&result)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || result.SourceID == 0 || result.Count != 1 {
		t.Fatalf("default manual import failed: status=%d result=%+v", response.StatusCode, result)
	}
	sourceValue, err := st.GetSource(result.SourceID)
	if err != nil || !sourceValue.Builtin || sourceValue.Name != store.DefaultManualSourceName || sourceValue.Kind != store.SourceKindManual {
		t.Fatalf("built-in group is wrong: value=%+v err=%v", sourceValue, err)
	}

	update, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/sources/"+itoa(result.SourceID), strings.NewReader(`{"kind":"manual","name":"Changed","enabled":false}`))
	update.Header.Set("Content-Type", "application/json")
	updateResponse := mustDo(t, client, update)
	updateResponse.Body.Close()
	if updateResponse.StatusCode != http.StatusConflict {
		t.Fatalf("built-in group update should be rejected, got %d", updateResponse.StatusCode)
	}
	remove, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/sources/"+itoa(result.SourceID), nil)
	removeResponse := mustDo(t, client, remove)
	removeResponse.Body.Close()
	if removeResponse.StatusCode != http.StatusConflict {
		t.Fatalf("built-in group delete should be rejected, got %d", removeResponse.StatusCode)
	}
}

func TestOutputSubscriptionAPIRejectsInformationalNode(t *testing.T) {
	st := newTestStore(t)
	sourceID, _ := st.CreateSource(store.Source{Name: "Air", URL: "http://x"})
	if err := st.ReplaceSourceNodes(sourceID, []store.NodeRecord{{
		SourceID: sourceID, Origin: store.SourceKindSubscription, Name: "剩余流量：1 GB",
		Protocol: "vless", Config: json.RawMessage(`{"name":"剩余流量：1 GB","type":"vless"}`),
		Fingerprint: "notice:traffic_remaining:test", Role: "notice",
	}}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := st.ListNodes()
	srv := httptest.NewServer(New(st, source.NewFetcher(st)).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)
	templates, _ := st.ListTemplates()

	response := mustPost(t, client, srv.URL+"/api/subscriptions", fmt.Sprintf(`{"name":"bad","template_version_id":%d,"bindings":[{"slot":"primary","node_ids":[%d]}]}`, templates[0].CurrentVersionID, nodes[0].ID))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("informational node accepted into output subscription: status %d", response.StatusCode)
	}
}

func TestV4SettingsAndLegacyRoutesRemoved(t *testing.T) {
	st := newTestStore(t)
	app := New(st, nil)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c := initAndClient(t, srv)

	for _, path := range []string{"/api/override", "/api/settings/reset-token", "/api/node-sets", "/api/profiles"} {
		r := mustGet(t, c, srv.URL+path)
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("legacy route %s should be gone, got %d", path, r.StatusCode)
		}
	}

	req2, _ := http.NewRequest("PUT", srv.URL+"/api/settings",
		strings.NewReader(`{"base_url":"https://sub.example.com","fetch_interval_sec":3600}`))
	req2.Header.Set("Content-Type", "application/json")
	r5 := mustDo(t, c, req2)
	r5.Body.Close()
	r6 := mustGet(t, c, srv.URL+"/api/settings")
	var s2 map[string]any
	json.NewDecoder(r6.Body).Decode(&s2)
	r6.Body.Close()
	if s2["base_url"] != "https://sub.example.com" || s2["fetch_interval_sec"] != float64(3600) {
		t.Fatalf("settings wrong: %#v", s2)
	}
	if _, exists := s2["output_token"]; exists {
		t.Fatalf("global output token must not exist in v4: %#v", s2)
	}
	if token, _ := st.GetSetting("output_token"); token != "" {
		t.Fatalf("initialization created legacy output token %q", token)
	}
}

func TestAPIRejectsInvalidConfiguration(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, source.NewFetcher(st)).Handler())
	defer srv.Close()
	c := initAndClient(t, srv)

	badSource := mustPost(t, c, srv.URL+"/api/sources", `{"name":"local","url":"file:///etc/passwd"}`)
	badSource.Body.Close()
	if badSource.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid source url: want 400, got %d", badSource.StatusCode)
	}

	req, _ := http.NewRequest("PUT", srv.URL+"/api/settings",
		strings.NewReader(`{"base_url":"javascript:alert(1)","fetch_interval_sec":10}`))
	req.Header.Set("Content-Type", "application/json")
	badSettings := mustDo(t, c, req)
	badSettings.Body.Close()
	if badSettings.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid settings: want 400, got %d", badSettings.StatusCode)
	}
	badManual := mustPost(t, c, srv.URL+"/api/sources", `{"kind":"manual","name":"manual","url":"https://should-not-exist"}`)
	badManual.Body.Close()
	if badManual.StatusCode != http.StatusBadRequest {
		t.Fatalf("manual source with URL: want 400, got %d", badManual.StatusCode)
	}
}

func TestOutputSubscriptionWorkflowBuildsMihomoAndSingBox(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	c := initAndClient(t, srv)

	manual := mustPost(t, c, srv.URL+"/api/sources", `{"kind":"manual","name":"My nodes","tags":["private"]}`)
	var sourceResult struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(manual.Body).Decode(&sourceResult)
	manual.Body.Close()
	if manual.StatusCode != http.StatusOK || sourceResult.ID == 0 {
		t.Fatalf("create manual source failed: %d %#v", manual.StatusCode, sourceResult)
	}

	importBody := `{"source_id":` + itoa(sourceResult.ID) + `,"content":"vless://00000000-0000-0000-0000-000000000001@node.example.com:443?encryption=none&type=tcp#HK"}`
	imported := mustPost(t, c, srv.URL+"/api/nodes/import", importBody)
	var importResult struct {
		Count int `json:"count"`
	}
	_ = json.NewDecoder(imported.Body).Decode(&importResult)
	imported.Body.Close()
	if imported.StatusCode != http.StatusOK || importResult.Count != 1 {
		t.Fatalf("import node failed: %d %#v", imported.StatusCode, importResult)
	}

	nodes, _ := st.ListNodes()
	if len(nodes) != 1 {
		t.Fatalf("imported node missing: %#v", nodes)
	}

	templatesResponse := mustGet(t, c, srv.URL+"/api/templates")
	var templates []struct {
		ID               int64  `json:"id"`
		Engine           string `json:"engine"`
		CurrentVersionID int64  `json:"current_version_id"`
	}
	_ = json.NewDecoder(templatesResponse.Body).Decode(&templates)
	templatesResponse.Body.Close()
	if len(templates) != 5 {
		t.Fatalf("want five platform templates, got %#v", templates)
	}

	for _, engine := range []string{"mihomo", "sing-box"} {
		var versionID int64
		for _, template := range templates {
			if template.Engine == engine {
				versionID = template.CurrentVersionID
				break
			}
		}
		payload := `{"name":"` + engine + ` subscription","template_version_id":` + itoa(versionID) + `,"bindings":[{"slot":"primary","node_ids":[` + itoa(nodes[0].ID) + `]}]}`
		created := mustPost(t, c, srv.URL+"/api/subscriptions", payload)
		var subscriptionResult struct {
			ID    int64  `json:"id"`
			Token string `json:"token"`
		}
		data, _ := io.ReadAll(created.Body)
		created.Body.Close()
		if created.StatusCode != http.StatusOK || json.Unmarshal(data, &subscriptionResult) != nil || subscriptionResult.Token == "" {
			t.Fatalf("create %s output subscription failed: %d %s", engine, created.StatusCode, data)
		}
		sub := mustGet(t, http.DefaultClient, srv.URL+"/sub/"+subscriptionResult.Token)
		content, _ := io.ReadAll(sub.Body)
		sub.Body.Close()
		if sub.StatusCode != http.StatusOK || !strings.Contains(string(content), "node.example.com") {
			t.Fatalf("compiled %s subscription wrong: %d %s", engine, sub.StatusCode, content)
		}
		if engine == "sing-box" && !strings.Contains(sub.Header.Get("Content-Type"), "json") {
			t.Fatalf("sing-box subscription has wrong content type: %q", sub.Header.Get("Content-Type"))
		}
	}
}
