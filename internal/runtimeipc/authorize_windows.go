//go:build windows

package runtimeipc

import (
	"errors"

	"golang.org/x/sys/windows"

	"submux/internal/runtimeapi"
)

const (
	windowsSystemSID         = "S-1-5-18"
	windowsAdministratorsSID = "S-1-5-32-544"
	windowsOperatorGroup     = "Submux Runtime Operators"
)

func authorizeCurrentPeer(current, peer runtimeapi.PeerIdentity) error {
	if peer.SID == current.SID || peer.SID == windowsSystemSID {
		return nil
	}
	if peer.Elevated && containsSID(peer.GroupSIDs, windowsAdministratorsSID) {
		return nil
	}
	if operatorSID, _, _, err := windows.LookupSID("", windowsOperatorGroup); err == nil &&
		containsSID(peer.GroupSIDs, operatorSID.String()) {
		return nil
	}
	return errors.New("Runtime peer SID is not authorized")
}

func containsSID(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
