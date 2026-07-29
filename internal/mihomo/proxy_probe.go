package mihomo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type LocalHTTPProxyProbe struct {
	Timeout time.Duration
}

func (p LocalHTTPProxyProbe) Probe(ctx context.Context, proxyAddress string) error {
	host, _, err := net.SplitHostPort(proxyAddress)
	if err != nil {
		return fmt.Errorf("parse Mihomo proxy address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("Mihomo proxy probe requires a loopback proxy address")
	}
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nonceBytes[:])

	targetListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open local Mihomo probe target: %w", err)
	}
	defer targetListener.Close()
	targetServer := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/plain")
			writer.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(writer, nonce)
		}),
	}
	targetDone := make(chan error, 1)
	go func() {
		err := targetServer.Serve(targetListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		targetDone <- err
	}()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = targetServer.Shutdown(shutdownContext)
		<-targetDone
	}()

	proxyURL := &url.URL{Scheme: "http", Host: proxyAddress}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		DisableCompression:  true,
		DisableKeepAlives:   true,
		MaxIdleConnsPerHost: 1,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		"http://"+targetListener.Addr().String()+"/submux-runtime-probe",
		nil,
	)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send HTTP request through Mihomo: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return fmt.Errorf("read Mihomo proxy probe response: %w", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != nonce {
		return errors.New("Mihomo proxy returned an unexpected local probe response")
	}
	return nil
}
