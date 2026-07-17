//go:build windows

package agentlocal

import (
	"context"
	"errors"
	"net"
	"net/http"

	winio "github.com/Microsoft/go-winio"
)

const windowsAgentPipe = `\\.\pipe\submux-agent`

// The protected DACL allows only LocalSystem (the service identity) and the
// built-in Administrators group. Windows performs peer authorization before a
// connection is handed to the HTTP server.
const windowsAgentPipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"

func Listen(pipePath string) (net.Listener, error) {
	if pipePath != windowsAgentPipe {
		return nil, errors.New("Windows Agent IPC uses the fixed protected submux-agent named pipe")
	}
	return winio.ListenPipe(pipePath, &winio.PipeConfig{
		SecurityDescriptor: windowsAgentPipeSDDL,
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
