package runtimeipc

import (
	"errors"

	"submux/internal/runtimeapi"
)

type Authorizer interface {
	Authorize(runtimeapi.PeerIdentity) error
}

type AuthorizeFunc func(runtimeapi.PeerIdentity) error

func (function AuthorizeFunc) Authorize(peer runtimeapi.PeerIdentity) error {
	return function(peer)
}

func CurrentUserAuthorizer() (Authorizer, error) {
	current, err := currentIdentity()
	if err != nil {
		return nil, err
	}
	return AuthorizeFunc(func(peer runtimeapi.PeerIdentity) error {
		if peer.Platform == "" || peer.Key() == "" {
			return errors.New("Runtime peer identity is unavailable")
		}
		if peer.Platform != current.Platform {
			return errors.New("Runtime peer platform does not match the host")
		}
		return authorizeCurrentPeer(current, peer)
	}), nil
}
