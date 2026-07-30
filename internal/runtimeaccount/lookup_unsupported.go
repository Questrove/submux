//go:build !linux && !darwin

package runtimeaccount

import "errors"

func Lookup(string) (uint32, uint32, error) {
	return 0, 0, errors.New("Runtime service accounts are supported only on Linux and macOS")
}
