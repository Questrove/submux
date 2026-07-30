//go:build windows

package runtimeipc

import (
	"errors"
	"fmt"
	"os/user"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	netAPIMaxPreferredLength = ^uint32(0)
	netAPIErrorMoreData      = 234
	maximumOperatorMembers   = 1024
)

var (
	netapi32                       = windows.NewLazySystemDLL("netapi32.dll")
	netLocalGroupGetMembers        = netapi32.NewProc("NetLocalGroupGetMembers")
	netApiBufferFree               = netapi32.NewProc("NetApiBufferFree")
	windowsPersistedOperatorMember = persistedWindowsOperatorMember
)

type localGroupMembersInfo0 struct {
	SID *windows.SID
}

func windowsOperatorMemberSIDs() ([]string, error) {
	groupName, err := windows.UTF16PtrFromString(windowsOperatorGroup)
	if err != nil {
		return nil, err
	}
	var resume uintptr
	members := make([]string, 0, 16)
	seen := make(map[string]struct{})
	for {
		var buffer *localGroupMembersInfo0
		var entriesRead uint32
		var totalEntries uint32
		status, _, _ := netLocalGroupGetMembers.Call(
			0,
			uintptr(unsafe.Pointer(groupName)),
			0,
			uintptr(unsafe.Pointer(&buffer)),
			uintptr(netAPIMaxPreferredLength),
			uintptr(unsafe.Pointer(&entriesRead)),
			uintptr(unsafe.Pointer(&totalEntries)),
			uintptr(unsafe.Pointer(&resume)),
		)
		if buffer != nil {
			defer netApiBufferFree.Call(uintptr(unsafe.Pointer(buffer)))
		}
		if status != 0 && status != netAPIErrorMoreData {
			return nil, fmt.Errorf("enumerate Runtime operator group members: Windows error %d", status)
		}
		if entriesRead > maximumOperatorMembers ||
			len(members)+int(entriesRead) > maximumOperatorMembers {
			return nil, errors.New("Runtime operator group has too many direct members")
		}
		if entriesRead > 0 {
			entries := unsafe.Slice(buffer, entriesRead)
			for _, entry := range entries {
				if entry.SID == nil || !entry.SID.IsValid() {
					return nil, errors.New("Runtime operator group contains an invalid SID")
				}
				value := entry.SID.String()
				if _, duplicate := seen[value]; duplicate {
					continue
				}
				seen[value] = struct{}{}
				members = append(members, value)
			}
		}
		if status == 0 {
			return members, nil
		}
	}
}

func persistedWindowsOperatorMember(sid string) bool {
	account, err := user.LookupId(sid)
	if err != nil {
		return false
	}
	operator, err := user.LookupGroup(windowsOperatorGroup)
	if err != nil {
		return false
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return false
	}
	return containsSID(groupIDs, operator.Gid)
}
