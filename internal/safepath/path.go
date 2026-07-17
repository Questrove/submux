package safepath

import (
	"errors"
	"os"
	"path/filepath"
)

// ContainsLink reports whether path or one of its existing ancestors is a
// symbolic link, junction, or other platform reparse point.
func ContainsLink(path string) (bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	return containsLink(filepath.Clean(absolute))
}

// ContainsLinkInExistingPath checks the nearest existing path and all of its
// ancestors. It is intended for a pre-MkdirAll guard and must be followed by a
// second ContainsLink check after creation.
func ContainsLinkInExistingPath(path string) (bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	for current := filepath.Clean(absolute); ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			return containsLink(current)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, errors.New("no existing path ancestor is available")
		}
	}
}
