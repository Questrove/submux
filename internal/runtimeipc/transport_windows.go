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

const windowsRuntimeServiceName = `NT SERVICE\SubmuxRuntime`

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
	descriptor, err := windowsManagementPipeDescriptor(endpoint, identity)
	if err != nil {
		return nil, err
	}
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
	identity, err := processIdentity(process, pid)
	if err != nil {
		return runtimeapi.PeerIdentity{}, err
	}
	return identity, nil
}

func dialLocal(ctx context.Context, endpoint string) (net.Conn, error) {
	if !validPipeEndpoint(endpoint) {
		return nil, errors.New(`Runtime Named Pipe must use \\.\pipe\submux-runtime or a test-specific child name`)
	}
	return winio.DialPipeContext(ctx, endpoint)
}

func currentIdentity() (runtimeapi.PeerIdentity, error) {
	return processIdentity(windows.CurrentProcess(), uint32(windows.GetCurrentProcessId()))
}

func processIdentity(process windows.Handle, pid uint32) (runtimeapi.PeerIdentity, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("open Runtime peer process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("read Runtime peer SID: %w", err)
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("read Runtime peer groups: %w", err)
	}
	groupSIDs := make([]string, 0, groups.GroupCount)
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_ENABLED == 0 ||
			group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY != 0 {
			continue
		}
		groupSIDs = append(groupSIDs, group.Sid.String())
	}
	return runtimeapi.PeerIdentity{
		Platform:  "windows",
		PID:       pid,
		SID:       user.User.Sid.String(),
		GroupSIDs: groupSIDs,
		Elevated:  token.IsElevated(),
	}, nil
}

func windowsManagementPipeDescriptor(
	endpoint string,
	identity runtimeapi.PeerIdentity,
) (string, error) {
	aces := []string{"(A;;GA;;;SY)", "(A;;GA;;;BA)"}
	if endpoint == `\\.\pipe\submux-runtime` {
		serviceSID, _, _, err := windows.LookupSID("", windowsRuntimeServiceName)
		if err != nil {
			return "", fmt.Errorf("resolve Submux Runtime service SID: %w", err)
		}
		operatorSID, _, _, err := windows.LookupSID("", windowsOperatorGroup)
		if err != nil {
			return "", fmt.Errorf("resolve Submux Runtime operator group SID: %w", err)
		}
		aces = append(
			aces,
			"(A;;GA;;;"+serviceSID.String()+")",
			"(A;;GA;;;"+operatorSID.String()+")",
		)
		memberSIDs, err := windowsOperatorMemberSIDs()
		if err != nil {
			return "", err
		}
		for _, memberSID := range memberSIDs {
			aces = append(aces, "(A;;GA;;;"+memberSID+")")
		}
	} else {
		aces = append(aces, "(A;;GA;;;"+identity.SID+")")
	}
	return "D:P" + strings.Join(aces, ""), nil
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
