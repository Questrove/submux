//go:build !windows

package safepath

import "path/filepath"

func containsLink(path string) (bool, error) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	return filepath.Clean(realPath) != filepath.Clean(path), nil
}
