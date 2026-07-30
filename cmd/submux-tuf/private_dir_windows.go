package main

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

func securePrivateDirectory(name string) error {
	current, err := user.Current()
	if err != nil || current.Username == "" {
		return fmt.Errorf("resolve current Windows user for private key ACL: %w", err)
	}
	command := exec.Command(
		"icacls.exe",
		name,
		"/inheritance:r",
		"/grant:r",
		current.Username+":(OI)(CI)F",
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("restrict private key directory ACL: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
