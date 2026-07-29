//go:build !windows

package runtimeipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"submux/internal/runtimeapi"
	"submux/internal/safepath"
)

type unixListener struct {
	*net.UnixListener
	path string
}

func listenLocal(endpoint string) (LocalListener, error) {
	absolute, err := validateUnixEndpoint(endpoint, true)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("Runtime endpoint already exists and is not a Unix Socket")
		}
		connection, dialErr := net.DialTimeout("unix", absolute, 250*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, errors.New("Runtime endpoint is already accepting connections")
		}
		if !errors.Is(dialErr, unix.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, fmt.Errorf("verify existing Runtime Socket: %w", dialErr)
		}
		if err := os.Remove(absolute); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale Runtime Socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Runtime Socket: %w", err)
	}
	address, err := net.ResolveUnixAddr("unix", absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve Runtime Socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listen on Runtime Socket: %w", err)
	}
	if err := os.Chmod(absolute, 0660); err != nil {
		_ = listener.Close()
		_ = os.Remove(absolute)
		return nil, fmt.Errorf("secure Runtime Socket: %w", err)
	}
	return &unixListener{UnixListener: listener, path: absolute}, nil
}

func (l *unixListener) PeerIdentity(connection net.Conn) (runtimeapi.PeerIdentity, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return runtimeapi.PeerIdentity{}, errors.New("Runtime connection is not a Unix Socket")
	}
	return platformPeerIdentity(unixConnection)
}

func (l *unixListener) Close() error {
	if l == nil || l.UnixListener == nil {
		return nil
	}
	err := l.UnixListener.Close()
	removeErr := os.Remove(l.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(err, removeErr)
}

func dialLocal(ctx context.Context, endpoint string) (net.Conn, error) {
	absolute, err := validateUnixEndpoint(endpoint, false)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", absolute)
}

func validateUnixEndpoint(endpoint string, createParent bool) (string, error) {
	if endpoint == "" || !filepath.IsAbs(endpoint) {
		return "", errors.New("Runtime Socket must use a fixed absolute path")
	}
	absolute := filepath.Clean(endpoint)
	if absolute == string(filepath.Separator) {
		return "", errors.New("Runtime Socket must not use the filesystem root")
	}
	parent := filepath.Dir(absolute)
	linked, err := safepath.ContainsLinkInExistingPath(parent)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime Socket ancestors: %w", err)
	}
	if linked {
		return "", errors.New("Runtime Socket path must not contain symbolic links")
	}
	if createParent {
		if err := os.MkdirAll(parent, 0750); err != nil {
			return "", fmt.Errorf("create Runtime Socket directory: %w", err)
		}
	}
	linked, err = safepath.ContainsLinkInExistingPath(parent)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime Socket directory: %w", err)
	}
	if linked {
		return "", errors.New("Runtime Socket path must not contain symbolic links")
	}
	return absolute, nil
}
