//go:build !windows

package main

import "io"

func runPlatformService([]string, io.Writer) (bool, int) {
	return false, 0
}
