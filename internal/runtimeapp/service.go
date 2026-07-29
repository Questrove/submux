package runtimeapp

import (
	"context"
	"errors"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

type Service struct {
	State   *runtimestate.Store
	Version string
	Now     func() time.Time
}

func (s *Service) Observe(ctx context.Context, peer runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.Snapshot{}, err
	}
	if s == nil || s.State == nil {
		return runtimeapi.Snapshot{}, errors.New("Runtime application state is unavailable")
	}
	if peer.Platform == "" || peer.Key() == "" {
		return runtimeapi.Snapshot{}, errors.New("Runtime peer identity is unavailable")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	version := s.Version
	if version == "" {
		version = "dev"
	}
	return s.State.Observe(version, now())
}
