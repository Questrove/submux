//go:build !windows && !darwin

package runtimeipc

import (
	"errors"

	"submux/internal/runtimeapi"
)

func authorizeCurrentPeer(current, peer runtimeapi.PeerIdentity) error {
	if peer.UID != current.UID && peer.UID != 0 {
		return errors.New("Runtime peer UID is not authorized")
	}
	return nil
}
