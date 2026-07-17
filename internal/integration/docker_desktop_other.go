//go:build !windows

package integration

import "errors"

func DefaultDockerDesktopManager() (DockerDesktopManager, error) {
	return nil, errors.New("Docker Desktop integration is available only on Windows")
}
