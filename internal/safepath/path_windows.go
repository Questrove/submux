//go:build windows

package safepath

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func containsLink(path string) (bool, error) {
	for current := path; ; current = filepath.Dir(current) {
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return false, err
		}
		attributes, err := windows.GetFileAttributes(pointer)
		if err != nil {
			return false, err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return true, nil
		}
		parent := filepath.Dir(current)
		if strings.EqualFold(parent, current) {
			return false, nil
		}
	}
}
