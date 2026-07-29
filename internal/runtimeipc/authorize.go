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
		if current.Platform == "windows" {
			if peer.SID != current.SID && peer.SID != "S-1-5-18" {
				return errors.New("Runtime peer SID is not authorized")
			}
			return nil
		}
		if peer.UID != current.UID && peer.UID != 0 {
			return errors.New("Runtime peer UID is not authorized")
		}
		return nil
	}), nil
}
