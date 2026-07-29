//go:build windows

package runtimeipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"submux/internal/runtimeapi"
)

type windowsListener struct {
	net.Listener
}

func listenLocal(endpoint string) (LocalListener, error) {
	if !validPipeEndpoint(endpoint) {
		return nil, errors.New(`Runtime Named Pipe must use \\.\pipe\submux-runtime or a test-specific child name`)
	}
	identity, err := currentIdentity()
	if err != nil {
		return nil, err
	}
	descriptor := "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + identity.SID + ")"
	// go-winio's ListenPipe always creates each pipe instance with
	// FILE_PIPE_REJECT_REMOTE_CLIENTS. The ACL below is the second gate.
	listener, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: descriptor,
		MessageMode:        false,
		InputBufferSize:    MaxRequestBytes,
		OutputBufferSize:   MaxResponseBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("listen on Runtime Named Pipe: %w", err)
	}
	return &windowsListener{Listener: listener}, nil
}

func (l *windowsListener) PeerIdentity(connection net.Conn) (runtimeapi.PeerIdentity, error) {
	handleProvider, ok := connection.(interface{ Fd() uintptr })
	if !ok {
		return runtimeapi.PeerIdentity{}, errors.New("Runtime connection does not expose its Named Pipe handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(handleProvider.Fd()), &pid); err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("read Runtime Named Pipe client process: %w", err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("open Runtime Named Pipe client process: %w", err)
	}
	defer windows.CloseHandle(process)
	sid, err := processSID(process)
	if err != nil {
		return runtimeapi.PeerIdentity{}, err
	}
	return runtimeapi.PeerIdentity{Platform: "windows", PID: pid, SID: sid}, nil
}

func dialLocal(ctx context.Context, endpoint string) (net.Conn, error) {
	if !validPipeEndpoint(endpoint) {
		return nil, errors.New(`Runtime Named Pipe must use \\.\pipe\submux-runtime or a test-specific child name`)
	}
	return winio.DialPipeContext(ctx, endpoint)
}

func currentIdentity() (runtimeapi.PeerIdentity, error) {
	sid, err := processSID(windows.CurrentProcess())
	if err != nil {
		return runtimeapi.PeerIdentity{}, err
	}
	return runtimeapi.PeerIdentity{
		Platform: "windows",
		PID:      uint32(windows.GetCurrentProcessId()),
		SID:      sid,
	}, nil
}

func processSID(process windows.Handle) (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return "", fmt.Errorf("open Runtime peer process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read Runtime peer SID: %w", err)
	}
	return user.User.Sid.String(), nil
}

func validPipeEndpoint(endpoint string) bool {
	const prefix = `\\.\pipe\submux-runtime`
	if endpoint == prefix {
		return true
	}
	return strings.HasPrefix(endpoint, prefix+"-test-") &&
		len(endpoint) <= len(prefix)+6+64 &&
		!strings.ContainsAny(endpoint[len(prefix)+6:], `\/:`)
}
