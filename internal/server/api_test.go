package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"submux/internal/source"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSourcesAPICRUD(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, source.NewFetcher(st)).Handler())
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

func TestV2SettingsAndLegacyRoutesRemoved(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	c := initAndClient(t, srv)

	for _, path := range []string{"/api/override", "/api/settings/reset-token"} {
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
		t.Fatalf("global output token must not exist in v2: %#v", s2)
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

func TestV2WorkflowBuildsMihomoAndSingBoxProfiles(t *testing.T) {
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

	nodeSet := mustPost(t, c, srv.URL+"/api/node-sets", `{"name":"Private VLESS","source_ids":[`+itoa(sourceResult.ID)+`],"protocols":["vless"]}`)
	var nodeSetResult struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(nodeSet.Body).Decode(&nodeSetResult)
	nodeSet.Body.Close()
	if nodeSet.StatusCode != http.StatusOK || nodeSetResult.ID == 0 {
		t.Fatalf("create node set failed: %d %#v", nodeSet.StatusCode, nodeSetResult)
	}

	templatesResponse := mustGet(t, c, srv.URL+"/api/templates")
	var templates []struct {
		ID               int64  `json:"id"`
		Engine           string `json:"engine"`
		CurrentVersionID int64  `json:"current_version_id"`
	}
	_ = json.NewDecoder(templatesResponse.Body).Decode(&templates)
	templatesResponse.Body.Close()
	if len(templates) != 4 {
		t.Fatalf("want four platform templates, got %#v", templates)
	}

	for _, engine := range []string{"mihomo", "sing-box"} {
		var versionID int64
		for _, template := range templates {
			if template.Engine == engine {
				versionID = template.CurrentVersionID
				break
			}
		}
		payload := `{"name":"` + engine + ` profile","template_version_id":` + itoa(versionID) + `,"bindings":[{"slot":"primary","node_set_id":` + itoa(nodeSetResult.ID) + `}]}`
		created := mustPost(t, c, srv.URL+"/api/profiles", payload)
		var profileResult struct {
			ID    int64  `json:"id"`
			Token string `json:"token"`
		}
		data, _ := io.ReadAll(created.Body)
		created.Body.Close()
		if created.StatusCode != http.StatusOK || json.Unmarshal(data, &profileResult) != nil || profileResult.Token == "" {
			t.Fatalf("create %s profile failed: %d %s", engine, created.StatusCode, data)
		}
		sub := mustGet(t, http.DefaultClient, srv.URL+"/sub/"+profileResult.Token)
		content, _ := io.ReadAll(sub.Body)
		sub.Body.Close()
		if sub.StatusCode != http.StatusOK || !strings.Contains(string(content), "node.example.com") {
			t.Fatalf("compiled %s subscription wrong: %d %s", engine, sub.StatusCode, content)
		}
		if engine == "sing-box" && !strings.Contains(sub.Header.Get("Content-Type"), "json") {
			t.Fatalf("sing-box profile has wrong content type: %q", sub.Header.Get("Content-Type"))
		}
	}
}
