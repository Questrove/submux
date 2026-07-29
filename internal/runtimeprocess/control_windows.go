//go:build windows

package runtimeprocess

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

func dialControl(ctx context.Context, endpoint string) (net.Conn, error) {
	if !strings.HasPrefix(endpoint, `\\.\pipe\submux-runtime-mihomo`) {
		return nil, errors.New("Mihomo Named Pipe must use the Runtime-owned fixed prefix")
	}
	return winio.DialPipeContext(ctx, endpoint)
}
