//go:build linux || darwin

package runtimeaccount

import (
	"errors"
	"os/user"
	"strconv"
)

func Lookup(name string) (uint32, uint32, error) {
	if name != "submux-runtime" && name != "_submux-runtime" {
		return 0, 0, errors.New("Runtime service account name is invalid")
	}
	account, err := user.Lookup(name)
	if err != nil {
		return 0, 0, errors.New("Runtime service account is unavailable")
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid == 0 {
		return 0, 0, errors.New("Runtime service account UID is invalid")
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil || gid == 0 {
		return 0, 0, errors.New("Runtime service account GID is invalid")
	}
	return uint32(uid), uint32(gid), nil
}
