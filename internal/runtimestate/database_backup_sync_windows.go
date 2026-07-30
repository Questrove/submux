//go:build windows

package runtimestate

// Windows has no supported equivalent of fsync for a directory opened by
// os.Open. BackupDatabase already calls File.Sync, which maps to
// FlushFileBuffers and commits both the file data and its NTFS metadata.
func syncDatabaseBackupParent(string) error {
	return nil
}
