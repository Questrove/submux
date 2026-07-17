//go:build !linux && !windows

package agentlocal

import (
	"errors"
	"net"
	"net/http"
)

func Listen(string) (net.Listener, error) {
	return nil, errors.New("local Agent IPC is not implemented on this platform")
}

func NewClient(string) (*http.Client, error) {
	return nil, errors.New("local Agent IPC is not implemented on this platform")
}
