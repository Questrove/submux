package store

import "testing"

func TestSourcesCRUD(t *testing.T) {
	s := newTestStore(t)

	id, err := s.CreateSource(Source{Name: "A", URL: "http://a", SortOrder: 2})
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	got, err := s.GetSource(id)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.Name != "A" || got.URL != "http://a" {
		t.Fatalf("GetSource mismatch: %+v", got)
	}
	if got.UserAgent != "clash-verge/v2.0.0" {
		t.Fatalf("default UA not applied: %q", got.UserAgent)
	}
	if !got.Enabled {
		t.Fatalf("expected enabled by default")
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("timestamps not set")
	}

	// 第二个源,sort_order 更小 → 应排在前
	id2, _ := s.CreateSource(Source{Name: "B", URL: "http://b", SortOrder: 1})

	list, err := s.ListSources()
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(list) != 2 || list[0].ID != id2 {
		t.Fatalf("ordering wrong: %+v", list)
	}

	// Update
	got.Name = "A2"
	if err := s.UpdateSource(got); err != nil {
		t.Fatalf("UpdateSource: %v", err)
	}
	reread, _ := s.GetSource(id)
	if reread.Name != "A2" {
		t.Fatalf("update not persisted: %q", reread.Name)
	}
	createdAt := reread.CreatedAt
	if err := s.UpdateSource(Source{ID: id, Name: "A3", URL: "http://a", Enabled: true}); err != nil {
		t.Fatalf("minimal UpdateSource: %v", err)
	}
	reread, _ = s.GetSource(id)
	if reread.CreatedAt != createdAt {
		t.Fatalf("created_at changed during update: %q -> %q", createdAt, reread.CreatedAt)
	}

	// SetSourceEnabled + ListEnabledSources
	if err := s.SetSourceEnabled(id, false); err != nil {
		t.Fatalf("SetSourceEnabled: %v", err)
	}
	enabled, _ := s.ListEnabledSources()
	if len(enabled) != 1 || enabled[0].ID != id2 {
		t.Fatalf("ListEnabledSources wrong: %+v", enabled)
	}

	// Delete
	if err := s.DeleteSource(id); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if _, err := s.GetSource(id); err == nil {
		t.Fatalf("expected error getting deleted source")
	}
}

func TestEnsureDefaultManualSourceAdoptsExistingGroupAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	existingID, err := s.CreateSource(Source{Kind: SourceKindManual, Name: DefaultManualSourceName})
	if err != nil {
		t.Fatal(err)
	}

	firstID, err := s.EnsureDefaultManualSource()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := s.EnsureDefaultManualSource()
	if err != nil {
		t.Fatal(err)
	}
	if firstID != existingID || secondID != existingID {
		t.Fatalf("default manual source was duplicated: existing=%d first=%d second=%d", existingID, firstID, secondID)
	}
	source, err := s.GetSource(existingID)
	if err != nil || !source.Builtin || !source.Enabled || source.Kind != SourceKindManual {
		t.Fatalf("existing group was not adopted as built-in: value=%+v err=%v", source, err)
	}
	values, _ := s.ListSources()
	if len(values) != 1 {
		t.Fatalf("expected one source, got %+v", values)
	}
}
