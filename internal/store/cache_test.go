package store

import "testing"

func TestCacheSuccessThenErrorKeepsData(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.CreateSource(Source{Name: "A", URL: "http://a"})

	if err := s.UpsertCacheSuccess(id, `{"total":100}`); err != nil {
		t.Fatalf("UpsertCacheSuccess: %v", err)
	}
	c, err := s.GetCache(id)
	if err != nil {
		t.Fatalf("GetCache: %v", err)
	}
	if c.UserinfoJSON != `{"total":100}` {
		t.Fatalf("success not stored: %+v", c)
	}
	if c.LastSuccessAt == "" || c.LastError != "" {
		t.Fatalf("success markers wrong: %+v", c)
	}

	// 失败:必须保留上次成功元数据，只写 last_error。
	if err := s.UpsertCacheError(id, "boom"); err != nil {
		t.Fatalf("UpsertCacheError: %v", err)
	}
	c, _ = s.GetCache(id)
	if c.UserinfoJSON != `{"total":100}` {
		t.Fatalf("error wiped userinfo: %+v", c)
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
	if c.LastError != "boom" {
		t.Fatalf("first-error state wrong: %+v", c)
	}
}

func TestLifecycleEventsRecordTransitionsOnly(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.CreateSource(Source{Name: "A", URL: "http://a"})
	if changed, err := s.RecordLifecycleState(id, "active"); err != nil || changed {
		t.Fatalf("initial state should only establish baseline: changed=%v err=%v", changed, err)
	}
	if changed, err := s.RecordLifecycleState(id, "expired"); err != nil || !changed {
		t.Fatalf("transition was not recorded: changed=%v err=%v", changed, err)
	}
	if changed, err := s.RecordLifecycleState(id, "expired"); err != nil || changed {
		t.Fatalf("duplicate transition was recorded: changed=%v err=%v", changed, err)
	}
	events, err := s.ListLifecycleEvents(10)
	if err != nil || len(events) != 1 || events[0].FromState != "active" || events[0].ToState != "expired" {
		t.Fatalf("wrong events: %+v err=%v", events, err)
	}
}
