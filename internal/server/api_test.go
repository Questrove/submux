package server

import (
	"encoding/json"
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
	if len(list) != 1 || list[0]["name"] != "AirA" {
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

func TestOverrideAndSettingsAPI(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	c := initAndClient(t, srv)

	req, _ := http.NewRequest("PUT", srv.URL+"/api/override", strings.NewReader(`{"content":"prepend-rules: []"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := mustDo(t, c, req)
	rr.Body.Close()
	r := mustGet(t, c, srv.URL+"/api/override")
	var ov struct{ Content string }
	json.NewDecoder(r.Body).Decode(&ov)
	r.Body.Close()
	if ov.Content != "prepend-rules: []" {
		t.Fatalf("override mismatch: %q", ov.Content)
	}

	r2 := mustGet(t, c, srv.URL+"/api/settings")
	var s1 struct {
		OutputToken string `json:"output_token"`
	}
	json.NewDecoder(r2.Body).Decode(&s1)
	r2.Body.Close()

	r3 := mustPost(t, c, srv.URL+"/api/settings/reset-token", "")
	var rt struct {
		OutputToken string `json:"output_token"`
	}
	json.NewDecoder(r3.Body).Decode(&rt)
	r3.Body.Close()
	if rt.OutputToken == "" || rt.OutputToken == s1.OutputToken {
		t.Fatalf("token not reset: %q vs %q", s1.OutputToken, rt.OutputToken)
	}

	req2, _ := http.NewRequest("PUT", srv.URL+"/api/settings",
		strings.NewReader(`{"base_url":"https://sub.example.com","fetch_interval_sec":3600}`))
	req2.Header.Set("Content-Type", "application/json")
	r5 := mustDo(t, c, req2)
	r5.Body.Close()
	r6 := mustGet(t, c, srv.URL+"/api/settings")
	var s2 struct {
		SubURL  string `json:"sub_url"`
		BaseURL string `json:"base_url"`
	}
	json.NewDecoder(r6.Body).Decode(&s2)
	r6.Body.Close()
	if !strings.HasPrefix(s2.SubURL, "https://sub.example.com/sub/") {
		t.Fatalf("sub_url wrong: %q", s2.SubURL)
	}
}
