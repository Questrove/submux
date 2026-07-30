//go:build !windows

package owneracl

import "os"

func RestrictFile(name string) error {
	return os.Chmod(name, 0600)
}

func RestrictDirectory(name string) error {
	return os.Chmod(name, 0700)
}
