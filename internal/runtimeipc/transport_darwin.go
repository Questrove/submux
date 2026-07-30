//go:build darwin

package runtimeipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"submux/internal/runtimeapi"
	"submux/internal/safepath"
)

const darwinManagementSocket = "/var/run/submux-runtime/runtime.sock"

type unixListener struct {
	*net.UnixListener
	path string
}

func listenLocal(endpoint string) (LocalListener, error) {
	absolute, err := validateUnixEndpoint(endpoint, endpoint != darwinManagementSocket)
	if err != nil {
		return nil, err
	}
	if endpoint == darwinManagementSocket {
		if err := validateDarwinManagementDirectory(filepath.Dir(absolute)); err != nil {
			return nil, err
		}
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
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
	if endpoint == darwinManagementSocket {
		gid, err := darwinOperatorGID()
		if err != nil {
			_ = listener.Close()
			_ = os.Remove(absolute)
			return nil, err
		}
		if err := os.Chown(absolute, os.Getuid(), int(gid)); err != nil {
			_ = listener.Close()
			_ = os.Remove(absolute)
			return nil, fmt.Errorf("assign Runtime Socket operator group: %w", err)
		}
	}
	if err := os.Chmod(absolute, 0660); err != nil {
		_ = listener.Close()
		_ = os.Remove(absolute)
		return nil, fmt.Errorf("secure Runtime Socket: %w", err)
	}
	return &unixListener{UnixListener: listener, path: absolute}, nil
}

func (listener *unixListener) PeerIdentity(connection net.Conn) (runtimeapi.PeerIdentity, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return runtimeapi.PeerIdentity{}, errors.New("Runtime connection is not a Unix Socket")
	}
	return platformPeerIdentity(unixConnection)
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

func validateDarwinManagementDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect macOS Runtime Socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0007 != 0 {
		return errors.New("macOS Runtime Socket directory permissions are invalid")
	}
	gid, err := darwinOperatorGID()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Gid != gid {
		return errors.New("macOS Runtime Socket directory ownership is invalid")
	}
	return nil
}

func darwinOperatorGID() (uint32, error) {
	group, err := user.LookupGroup(darwinOperatorGroup)
	if err != nil {
		return 0, fmt.Errorf("resolve macOS Runtime operator group: %w", err)
	}
	value, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return 0, errors.New("macOS Runtime operator group GID is invalid")
	}
	return uint32(value), nil
}
