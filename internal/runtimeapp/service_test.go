package runtimeapp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

func TestServiceObserveUsesVerifiedPeerAndClock(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	now := time.Date(2026, 7, 30, 4, 5, 6, 0, time.UTC)
	service := &Service{
		State:   state,
		Version: "v9.0.0",
		Now:     func() time.Time { return now },
	}
	snapshot, err := service.Observe(context.Background(), runtimeapi.PeerIdentity{
		Platform: "test",
		UID:      1000,
	})
	if err != nil {
		t.Fatalf("observe Runtime: %v", err)
	}
	if snapshot.Runtime.Version != "v9.0.0" || !snapshot.ObservedAt.Equal(now) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestServiceObserveRejectsMissingPeerIdentity(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	service := &Service{State: state}
	if _, err := service.Observe(context.Background(), runtimeapi.PeerIdentity{}); err == nil {
		t.Fatal("Observe accepted missing peer identity")
	}
}
