//go:build !windows

package productupdate

import "os"

func replaceStateFile(source, destination string) error {
	return os.Rename(source, destination)
}
