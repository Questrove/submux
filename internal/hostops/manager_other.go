//go:build !linux && !windows

package hostops

import (
	"errors"
	"net/http"
)

func DefaultLinuxManager(*http.Client) (CoreManager, error) {
	return nil, errors.New("Linux Mihomo host operations are unavailable on this platform")
}
