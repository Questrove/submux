//go:build !linux

package runtimenet

import (
	"context"
	"errors"
	"net"
)

func Listen(string, int) (LocalListener, error) {
	return nil, errors.New("privileged Runtime network service is only available on Linux")
}

func dialNetwork(context.Context, string) (net.Conn, error) {
	return nil, errors.New("privileged Runtime network service is only available on Linux")
}
