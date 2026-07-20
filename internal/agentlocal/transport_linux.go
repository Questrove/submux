//go:build linux

package agentlocal

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"submux/internal/safepath"
)

type peerListener struct {
	*net.UnixListener
	socketPath  string
	allowedUIDs map[uint32]bool
}

func Listen(socketPath string) (net.Listener, error) {
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) == string(filepath.Separator) {
		return nil, errors.New("local socket path must be an absolute non-root path")
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to replace a non-socket local API path")
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	directory := filepath.Dir(socketPath)
	linked, err := safepath.ContainsLinkInExistingPath(directory)
	if err != nil || linked {
		return nil, errors.New("local socket directory must not contain symbolic links")
	}
	if err := os.MkdirAll(directory, 0750); err != nil {
		return nil, err
	}
	linked, err = safepath.ContainsLink(directory)
	if err != nil || linked {
		return nil, errors.New("local socket directory must not contain symbolic links")
	}
	allowedUIDs := map[uint32]bool{uint32(os.Geteuid()): true}
	address, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return &peerListener{UnixListener: listener, socketPath: socketPath, allowedUIDs: allowedUIDs}, nil
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(connection)
		if err == nil && l.allowedUIDs[uid] {
			return connection, nil
		}
		_ = connection.Close()
	}
}

func (l *peerListener) Close() error {
	err := l.UnixListener.Close()
	_ = os.Remove(l.socketPath)
	return err
}

func peerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Ucred
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	return credentials.Uid, nil
}

func NewClient(socketPath string) (*http.Client, error) {
	if !filepath.IsAbs(socketPath) {
		return nil, errors.New("local socket path must be absolute")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	return &http.Client{Transport: transport}, nil
}
