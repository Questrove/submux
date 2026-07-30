//go:build linux || darwin

package productupdate

import "golang.org/x/sys/unix"

func availableDiskBytes(path string) (uint64, error) {
	var status unix.Statfs_t
	if err := unix.Statfs(path, &status); err != nil {
		return 0, err
	}
	return uint64(status.Bavail) * uint64(status.Bsize), nil
}
