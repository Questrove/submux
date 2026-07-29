//go:build !windows

package runtimeprocess

import (
	"context"
	"errors"
	"net"
	"path/filepath"
)

func dialControl(ctx context.Context, endpoint string) (net.Conn, error) {
	if !filepath.IsAbs(endpoint) {
		return nil, errors.New("Mihomo Unix Socket must use a fixed absolute path")
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", endpoint)
}
