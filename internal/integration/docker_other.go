//go:build !linux

package integration

import "errors"

func DefaultDockerDaemonManager() (DockerDaemonManager, error) {
	return nil, errors.New("Docker daemon integration is implemented only for rootful Linux Docker Engine")
}
