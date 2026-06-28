package store

import "testing"

func TestCacheSuccessThenErrorKeepsData(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.CreateSource(Source{Name: "A", URL: "http://a"})

	if err := s.UpsertCacheSuccess(id, "RAW1", "[]", `{"total":100}`); err != nil {
		t.Fatalf("UpsertCacheSuccess: %v", err)
	}
	c, err := s.GetCache(id)
	if err != nil {
		t.Fatalf("GetCache: %v", err)
	}
	if c.Raw != "RAW1" || c.UserinfoJSON != `{"total":100}` {
		t.Fatalf("success not stored: %+v", c)
	}
	if c.LastSuccessAt == "" || c.LastError != "" {
		t.Fatalf("success markers wrong: %+v", c)
	}

	// 失败:必须保留上次 raw,只写 last_error(stale-while-error)
	if err := s.UpsertCacheError(id, "boom"); err != nil {
		t.Fatalf("UpsertCacheError: %v", err)
	}
	c, _ = s.GetCache(id)
	if c.Raw != "RAW1" {
		t.Fatalf("error wiped raw, want RAW1 got %q", c.Raw)
	}
	if c.LastError != "boom" {
		t.Fatalf("last_error not set: %q", c.LastError)
	}
	if c.LastSuccessAt == "" {
		t.Fatalf("last_success_at should be preserved")
	}
}

func TestCacheErrorFirstNoData(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.CreateSource(Source{Name: "A", URL: "http://a"})

	if err := s.UpsertCacheError(id, "boom"); err != nil {
		t.Fatalf("UpsertCacheError: %v", err)
	}
	c, _ := s.GetCache(id)
	if c.Raw != "" || c.LastError != "boom" {
		t.Fatalf("first-error state wrong: %+v", c)
	}
}
