package store

import "testing"

func TestSettingsSetGet(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetSetting("missing")
	if err != nil || got != "" {
		t.Fatalf("missing key: got %q err %v, want \"\" nil", got, err)
	}

	if err := s.SetSetting("k", "v1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, _ = s.GetSetting("k")
	if got != "v1" {
		t.Fatalf("after set: got %q want v1", got)
	}

	if err := s.SetSetting("k", "v2"); err != nil {
		t.Fatalf("SetSetting overwrite: %v", err)
	}
	got, _ = s.GetSetting("k")
	if got != "v2" {
		t.Fatalf("after overwrite: got %q want v2", got)
	}
}

func TestGetSettingInt(t *testing.T) {
	s := newTestStore(t)
	n, err := s.GetSettingInt("interval", 10800)
	if err != nil || n != 10800 {
		t.Fatalf("default: got %d err %v, want 10800 nil", n, err)
	}
	s.SetSetting("interval", "60")
	n, _ = s.GetSettingInt("interval", 10800)
	if n != 60 {
		t.Fatalf("set: got %d want 60", n)
	}
}
