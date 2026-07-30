//go:build !windows

package runtimestate

import "os"

func syncDatabaseBackupParent(parent string) error {
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
