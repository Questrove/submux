//go:build darwin

package runtimeipc

import (
	"errors"
	"os/user"
	"strconv"

	"submux/internal/runtimeapi"
)

const darwinOperatorGroup = "submux-runtime-operators"

func authorizeCurrentPeer(current, peer runtimeapi.PeerIdentity) error {
	if peer.UID == current.UID || peer.UID == 0 {
		return nil
	}
	group, err := user.LookupGroup(darwinOperatorGroup)
	if err == nil {
		gid, parseErr := strconv.ParseUint(group.Gid, 10, 32)
		if parseErr == nil && containsGroupID(peer.GroupIDs, uint32(gid)) {
			return nil
		}
	}
	return errors.New("Runtime peer UID and groups are not authorized")
}
