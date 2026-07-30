//go:build darwin

package runtimenet

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"submux/internal/safepath"
)

const darwinNetworkSocket = "/var/run/submux-runtime-privileged/runtime-net.sock"

type darwinNetworkListener struct {
	*net.UnixListener
}

func Listen(endpoint string, runtimeGID uint32) (LocalListener, error) {
	if !validDarwinNetworkEndpoint(endpoint) {
		return nil, errors.New("privileged Runtime network Socket must use the fixed macOS endpoint")
	}
	parent := filepath.Dir(endpoint)
	if linked, err := safepath.ContainsLinkInExistingPath(parent); err != nil {
		return nil, err
	} else if linked {
		return nil, errors.New("privileged Runtime network Socket directory must not contain links")
	}
	if err := os.MkdirAll(parent, 0750); err != nil {
		return nil, err
	}
	if linked, err := safepath.ContainsLinkInExistingPath(parent); err != nil {
		return nil, err
	} else if linked {
		return nil, errors.New("privileged Runtime network Socket directory must not contain links")
	}
	if endpoint == darwinNetworkSocket {
		if os.Geteuid() != 0 {
			return nil, errors.New("privileged Runtime network Socket must be created by root")
		}
		if err := os.Chown(parent, 0, int(runtimeGID)); err != nil {
			return nil, err
		}
		if err := os.Chmod(parent, 0750); err != nil {
			return nil, err
		}
	}
	if info, err := os.Lstat(endpoint); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("privileged Runtime network endpoint is not a Socket")
		}
		if err := os.Remove(endpoint); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if endpoint == darwinNetworkSocket {
		if err := os.Chown(endpoint, 0, int(runtimeGID)); err != nil {
			_ = listener.Close()
			return nil, err
		}
	}
	if err := os.Chmod(endpoint, 0660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return &darwinNetworkListener{UnixListener: listener}, nil
}

func (listener *darwinNetworkListener) PeerUID(connection net.Conn) (uint32, error) {
	peer, err := darwinPeerCredential(connection)
	if err != nil {
		return 0, err
	}
	return peer.Uid, nil
}

func dialNetwork(ctx context.Context, endpoint string) (net.Conn, error) {
	if !validDarwinNetworkEndpoint(endpoint) {
		return nil, errors.New("privileged Runtime network Socket must use the fixed macOS endpoint")
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, err
	}
	peer, err := darwinPeerCredential(connection)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if endpoint == darwinNetworkSocket && peer.Uid != 0 {
		_ = connection.Close()
		return nil, errors.New("privileged Runtime network Socket peer is not root")
	}
	return connection, nil
}

func darwinPeerCredential(connection net.Conn) (*unix.Xucred, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return nil, errors.New("Runtime network connection is not a Unix Socket")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		credential *unix.Xucred
		controlErr error
	)
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptXucred(
			int(fd),
			unix.SOL_LOCAL,
			unix.LOCAL_PEERCRED,
		)
	}); err != nil {
		return nil, err
	}
	if controlErr != nil {
		return nil, controlErr
	}
	if credential == nil ||
		credential.Ngroups < 0 ||
		int(credential.Ngroups) > len(credential.Groups) {
		return nil, errors.New("Runtime network peer credential is invalid")
	}
	return credential, nil
}

func validDarwinNetworkEndpoint(endpoint string) bool {
	if endpoint == darwinNetworkSocket {
		return true
	}
	if !filepath.IsAbs(endpoint) ||
		!strings.HasPrefix(filepath.Base(endpoint), "submux-runtime-net-test-") ||
		filepath.Ext(endpoint) != ".sock" ||
		len(filepath.Base(endpoint)) > 96 {
		return false
	}
	return true
}
