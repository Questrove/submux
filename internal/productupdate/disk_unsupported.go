//go:build !linux && !darwin && !windows

package productupdate

import "errors"

func availableDiskBytes(string) (uint64, error) {
	return 0, errors.New("Runtime product updates are unsupported on this platform")
}
