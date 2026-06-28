package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustGet(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	r, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return r
}

func mustPost(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := c.Post(url, "application/json", rd)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return r
}

func mustDo(t *testing.T, c *http.Client, req *http.Request) *http.Response {
	t.Helper()
	r, err := c.Do(req)
	if err != nil {
		t.Fatalf("DO %s: %v", req.URL, err)
	}
	return r
}

// initAndClient 在测试服务器上完成首次初始化,返回带 session cookie 的 client。
func initAndClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	r := mustPost(t, c, srv.URL+"/api/init", `{"password":"pw12345"}`)
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("init status %d", r.StatusCode)
	}
	return c
}

func TestStatusThenInitGrantsAccess(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()

	r := mustGet(t, http.DefaultClient, srv.URL+"/api/status")
	var s struct{ Initialized, Authed bool }
	json.NewDecoder(r.Body).Decode(&s)
	r.Body.Close()
	if s.Initialized {
		t.Fatalf("should start uninitialized")
	}

	c := initAndClient(t, srv)
	r2 := mustGet(t, c, srv.URL+"/api/sources")
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("authed /api/sources want 200, got %d", r2.StatusCode)
	}
}

func TestUnauthorizedWithoutCookie(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	r := mustGet(t, http.DefaultClient, srv.URL+"/api/sources")
	defer r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("want 401, got %d", r.StatusCode)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	r := mustPost(t, http.DefaultClient, srv.URL+"/api/init", `{"password":"pw12345"}`)
	r.Body.Close()
	r2 := mustPost(t, http.DefaultClient, srv.URL+"/api/login", `{"password":"wrong"}`)
	defer r2.Body.Close()
	if r2.StatusCode != 401 {
		t.Fatalf("want 401, got %d", r2.StatusCode)
	}
}

func TestInitTwiceConflict(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	r := mustPost(t, http.DefaultClient, srv.URL+"/api/init", `{"password":"pw12345"}`)
	r.Body.Close()
	r2 := mustPost(t, http.DefaultClient, srv.URL+"/api/init", `{"password":"another"}`)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", r2.StatusCode)
	}
}
