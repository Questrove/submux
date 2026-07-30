//go:build darwin

package safepath

import (
	"path/filepath"
	"strings"
)

func containsLink(path string) (bool, error) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	return cleanDarwinSystemAlias(realPath) != cleanDarwinSystemAlias(path), nil
}

func cleanDarwinSystemAlias(path string) string {
	clean := filepath.Clean(path)
	if clean == "/var" || strings.HasPrefix(clean, "/var/") {
		return "/private" + clean
	}
	return clean
}
