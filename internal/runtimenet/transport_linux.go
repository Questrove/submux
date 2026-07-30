//go:build linux

package runtimenet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"submux/internal/safepath"
)

type unixListener struct {
	*net.UnixListener
	path string
}

const linuxNetworkSocket = "/run/submux-runtime-privileged/runtime-net.sock"

func Listen(endpoint string, runtimeGID int) (LocalListener, error) {
	if runtimeGID < 0 {
		return nil, errors.New("privileged Runtime network group ID is invalid")
	}
	absolute, err := validateNetworkEndpoint(endpoint, endpoint != linuxNetworkSocket)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(absolute)
	if endpoint == linuxNetworkSocket {
		if err := validateLinuxNetworkDirectory(parent, runtimeGID); err != nil {
			return nil, err
		}
	} else {
		if err := os.Chown(parent, 0, runtimeGID); err != nil {
			return nil, fmt.Errorf("assign privileged Runtime network Socket directory group: %w", err)
		}
		if err := os.Chmod(parent, 0750); err != nil {
			return nil, fmt.Errorf("secure privileged Runtime network Socket directory: %w", err)
		}
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("privileged Runtime network endpoint exists and is not a Unix Socket")
		}
		connection, dialErr := net.DialTimeout("unix", absolute, 250*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, errors.New("privileged Runtime network endpoint is already accepting connections")
		}
		if !errors.Is(dialErr, unix.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, fmt.Errorf("verify privileged Runtime network Socket: %w", dialErr)
		}
		if err := os.Remove(absolute); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale privileged Runtime network Socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect privileged Runtime network Socket: %w", err)
	}
	address, err := net.ResolveUnixAddr("unix", absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve privileged Runtime network Socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listen on privileged Runtime network Socket: %w", err)
	}
	if endpoint == linuxNetworkSocket {
		var stat unix.Stat_t
		if err := unix.Stat(absolute, &stat); err != nil ||
			stat.Uid != 0 || stat.Gid != uint32(runtimeGID) {
			_ = listener.Close()
			_ = os.Remove(absolute)
			return nil, errors.New("privileged Runtime network Socket ownership is invalid")
		}
	} else if err := os.Chown(absolute, 0, runtimeGID); err != nil {
		_ = listener.Close()
		_ = os.Remove(absolute)
		return nil, fmt.Errorf("assign privileged Runtime network Socket group: %w", err)
	}
	if err := os.Chmod(absolute, 0660); err != nil {
		_ = listener.Close()
		_ = os.Remove(absolute)
		return nil, fmt.Errorf("secure privileged Runtime network Socket: %w", err)
	}
	return &unixListener{UnixListener: listener, path: absolute}, nil
}

func validateLinuxNetworkDirectory(parent string, runtimeGID int) error {
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect privileged Runtime network Socket directory: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(parent, &stat); err != nil {
		return fmt.Errorf("inspect privileged Runtime network Socket directory ownership: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0750 ||
		stat.Uid != 0 || stat.Gid != uint32(runtimeGID) {
		return fmt.Errorf(
			"privileged Runtime network Socket directory ownership or permissions are invalid: uid=%d gid=%d mode=%#o expected_gid=%d",
			stat.Uid,
			stat.Gid,
			info.Mode().Perm(),
			runtimeGID,
		)
	}
	return nil
}

func (listener *unixListener) PeerUID(connection net.Conn) (uint32, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, errors.New("privileged Runtime network connection is not a Unix Socket")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("open privileged Runtime network peer credentials: %w", err)
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("inspect privileged Runtime network peer credentials: %w", err)
	}
	if socketErr != nil {
		return 0, fmt.Errorf("read privileged Runtime network peer credentials: %w", socketErr)
	}
	return credentials.Uid, nil
}

func (listener *unixListener) Close() error {
	if listener == nil || listener.UnixListener == nil {
		return nil
	}
	err := listener.UnixListener.Close()
	removeErr := os.Remove(listener.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(err, removeErr)
}

func dialNetwork(ctx context.Context, endpoint string) (net.Conn, error) {
	absolute, err := validateNetworkEndpoint(endpoint, false)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", absolute)
	if err != nil {
		return nil, err
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return nil, errors.New("privileged Runtime network service is not a Unix Socket")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if socketErr != nil || credentials == nil || credentials.Uid != 0 {
		_ = connection.Close()
		return nil, errors.New("privileged Runtime network service peer is not root")
	}
	return connection, nil
}

func validateNetworkEndpoint(endpoint string, createParent bool) (string, error) {
	if endpoint == "" || !filepath.IsAbs(endpoint) {
		return "", errors.New("privileged Runtime network Socket must use a fixed absolute path")
	}
	absolute := filepath.Clean(endpoint)
	if absolute == string(filepath.Separator) {
		return "", errors.New("privileged Runtime network Socket must not use the filesystem root")
	}
	parent := filepath.Dir(absolute)
	linked, err := safepath.ContainsLinkInExistingPath(parent)
	if err != nil {
		return "", fmt.Errorf("inspect privileged Runtime network Socket ancestors: %w", err)
	}
	if linked {
		return "", errors.New("privileged Runtime network Socket path must not contain links")
	}
	if createParent {
		if err := os.MkdirAll(parent, 0750); err != nil {
			return "", fmt.Errorf("create privileged Runtime network Socket directory: %w", err)
		}
	}
	linked, err = safepath.ContainsLinkInExistingPath(parent)
	if err != nil {
		return "", fmt.Errorf("inspect privileged Runtime network Socket directory: %w", err)
	}
	if linked {
		return "", errors.New("privileged Runtime network Socket path must not contain links")
	}
	return absolute, nil
}
