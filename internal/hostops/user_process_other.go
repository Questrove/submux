//go:build !linux && !windows

package hostops

import "os/exec"

func configureUserChild(*exec.Cmd) {}

func guardUserChild(*exec.Cmd) (func() error, error) { return func() error { return nil }, nil }
