//go:build windows

package runtimenet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	windowsNetworkPipe        = `\\.\pipe\submux-runtime-net`
	windowsRuntimeServiceName = `NT SERVICE\SubmuxRuntime`
)

type windowsNetworkListener struct {
	net.Listener
	allowedSID string
}

func Listen(endpoint string, runtimeUID uint32) (LocalListener, error) {
	if !validNetworkPipeEndpoint(endpoint) {
		return nil, errors.New(`privileged Runtime network Pipe must use \\.\pipe\submux-runtime-net or a test-specific child name`)
	}
	if runtimeUID == 0 {
		return nil, errors.New("privileged Runtime network Pipe requires a non-zero Runtime identity")
	}

	sid, err := allowedNetworkClientSID(endpoint)
	if err != nil {
		return nil, err
	}
	descriptor := "D:P(A;;GA;;;SY)(A;;GA;;;" + sid + ")"
	// go-winio adds FILE_PIPE_REJECT_REMOTE_CLIENTS to every server instance.
	listener, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: descriptor,
		MessageMode:        false,
		InputBufferSize:    maxIPCRequestBytes,
		OutputBufferSize:   maxIPCRequestBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("listen on privileged Runtime network Pipe: %w", err)
	}
	return &windowsNetworkListener{Listener: listener, allowedSID: sid}, nil
}

func (listener *windowsNetworkListener) PeerUID(connection net.Conn) (uint32, error) {
	handleProvider, ok := connection.(interface{ Fd() uintptr })
	if !ok {
		return 0, errors.New("Runtime network connection does not expose its Named Pipe handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(handleProvider.Fd()), &pid); err != nil {
		return 0, fmt.Errorf("read Runtime network Pipe client process: %w", err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, fmt.Errorf("open Runtime network Pipe client process: %w", err)
	}
	defer windows.CloseHandle(process)
	sid, err := windowsProcessSID(process)
	if err != nil {
		return 0, err
	}
	if sid != listener.allowedSID && sid != "S-1-5-18" {
		return 0, errors.New("Runtime network Pipe peer SID is not authorized")
	}
	// The manager's authorization model uses one non-root numeric identity.
	// Windows authenticates the real SID above and maps it to this internal value.
	return 1, nil
}

func dialNetwork(ctx context.Context, endpoint string) (net.Conn, error) {
	if !validNetworkPipeEndpoint(endpoint) {
		return nil, errors.New(`privileged Runtime network Pipe must use \\.\pipe\submux-runtime-net or a test-specific child name`)
	}
	return winio.DialPipeContext(ctx, endpoint)
}

func allowedNetworkClientSID(endpoint string) (string, error) {
	if endpoint != windowsNetworkPipe {
		return windowsProcessSID(windows.CurrentProcess())
	}
	sid, _, _, err := windows.LookupSID("", windowsRuntimeServiceName)
	if err != nil {
		return "", fmt.Errorf("resolve Submux Runtime service SID: %w", err)
	}
	return sid.String(), nil
}

func windowsProcessSID(process windows.Handle) (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return "", fmt.Errorf("open Runtime network peer token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read Runtime network peer SID: %w", err)
	}
	return user.User.Sid.String(), nil
}

func validNetworkPipeEndpoint(endpoint string) bool {
	if endpoint == windowsNetworkPipe {
		return true
	}
	const testPrefix = windowsNetworkPipe + "-test-"
	return strings.HasPrefix(endpoint, testPrefix) &&
		len(endpoint) <= len(testPrefix)+64 &&
		len(endpoint) > len(testPrefix) &&
		!strings.ContainsAny(endpoint[len(testPrefix):], `\/:`)
}
