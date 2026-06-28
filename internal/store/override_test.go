package store

import (
	"bytes"
	"testing"
)

func TestOverrideGetSet(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetOverride()
	if err != nil || got != "" {
		t.Fatalf("empty override: got %q err %v", got, err)
	}
	if err := s.SetOverride("prepend-rules: []\n"); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	got, _ = s.GetOverride()
	if got != "prepend-rules: []\n" {
		t.Fatalf("after set: %q", got)
	}
	if err := s.SetOverride("changed"); err != nil {
		t.Fatalf("SetOverride 2: %v", err)
	}
	got, _ = s.GetOverride()
	if got != "changed" {
		t.Fatalf("after overwrite: %q", got)
	}
}

func TestLastGoodGetSet(t *testing.T) {
	s := newTestStore(t)
	b, err := s.GetLastGood("clash")
	if err != nil || b != nil {
		t.Fatalf("empty last_good: got %v err %v", b, err)
	}
	if err := s.SetLastGood("clash", []byte("BODY1")); err != nil {
		t.Fatalf("SetLastGood: %v", err)
	}
	b, _ = s.GetLastGood("clash")
	if !bytes.Equal(b, []byte("BODY1")) {
		t.Fatalf("got %q", b)
	}
	s.SetLastGood("clash", []byte("BODY2"))
	b, _ = s.GetLastGood("clash")
	if !bytes.Equal(b, []byte("BODY2")) {
		t.Fatalf("overwrite got %q", b)
	}
}
