package runtimetraffic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"submux/internal/runtimeprocess"
)

type MihomoController struct {
	Endpoint string
	Timeout  time.Duration
	Dial     func(context.Context) (net.Conn, error)
}

var ErrMihomoConnectionControl = errors.New("Mihomo connection control failed")

func (controller MihomoController) CloseConnection(ctx context.Context, connectionID string) (bool, error) {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return false, errors.New("Mihomo connection ID is required")
	}
	response, err := controller.do(ctx, http.MethodDelete, "/connections/"+url.PathEscape(connectionID))
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrMihomoConnectionControl, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return false, nil
	case http.StatusNotFound:
		return true, nil
	default:
		return false, fmt.Errorf("%w: close endpoint returned HTTP %d", ErrMihomoConnectionControl, response.StatusCode)
	}
}

func (controller MihomoController) do(ctx context.Context, method string, path string) (*http.Response, error) {
	if ctx == nil {
		return nil, errors.New("Mihomo connection controller context is required")
	}
	if controller.Endpoint == "" && controller.Dial == nil {
		return nil, errors.New("Mihomo local control endpoint is required")
	}
	timeout := controller.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if controller.Dial != nil {
				return controller.Dial(ctx)
			}
			return runtimeprocess.DialControl(ctx, controller.Endpoint)
		},
		DisableCompression: true,
		DisableKeepAlives:  true,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, method, "http://mihomo"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("control Mihomo connections: %w", err)
	}
	return response, nil
}
