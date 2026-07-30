//go:build !windows

package main

import (
	"fmt"
	"os"
)

func securePrivateDirectory(name string) error {
	if err := os.Chmod(name, 0700); err != nil {
		return fmt.Errorf("restrict private key directory: %w", err)
	}
	return nil
}
