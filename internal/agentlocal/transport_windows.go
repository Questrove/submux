//go:build windows

package agentlocal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const windowsAgentPipe = `\\.\pipe\submux-agent`

func Listen(pipePath string) (net.Listener, error) {
	if pipePath != windowsAgentPipe {
		return nil, errors.New("Windows Agent IPC uses the fixed protected submux-agent named pipe")
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current Windows user SID: %w", err)
	}
	// The pipe belongs to the same unprivileged user that owns the Agent data.
	descriptor := "D:P(A;;GA;;;" + tokenUser.User.Sid.String() + ")"
	return winio.ListenPipe(pipePath, &winio.PipeConfig{
		SecurityDescriptor: descriptor,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

func NewClient(pipePath string) (*http.Client, error) {
	if pipePath != windowsAgentPipe {
		return nil, errors.New("Windows Agent IPC uses the fixed protected submux-agent named pipe")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, pipePath)
	}}
	return &http.Client{Transport: transport}, nil
}
