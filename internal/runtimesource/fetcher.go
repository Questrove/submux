package runtimesource

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeapi"
)

const (
	FailureTemporary = "temporary"
	FailureConfig    = "config"
	FailureAuth      = "authentication"
	FailurePolicy    = "policy"
)

type ConditionalRequest struct {
	ETag         string
	LastModified string
}

type FetchResult struct {
	Body         []byte
	SHA256       string
	ETag         string
	LastModified string
	NotModified  bool
	Route        string
}

type FetchError struct {
	Class      string
	Result     string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Cause      error
}

func (e *FetchError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *FetchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Fetcher struct {
	Resolver      Resolver
	MihomoAddress string
	Now           func() time.Time

	DirectDialContext func(context.Context, string, string) (net.Conn, error)
}

func (f *Fetcher) Fetch(
	ctx context.Context,
	config SourceConfig,
	conditional ConditionalRequest,
) (FetchResult, error) {
	if ctx == nil {
		return FetchResult{}, sourceFetchError(FailureConfig, "invalid_request", "Remote source request is invalid", false, nil)
	}
	if config.URL == nil {
		return FetchResult{}, sourceFetchError(FailureConfig, "invalid_request", "Remote source request is invalid", false, nil)
	}
	connections := &connectionTargets{
		config:        config,
		resolver:      f.resolver(),
		initialScheme: config.URL.Scheme,
		targets:       make(map[string]authorizedConnection),
	}
	if err := connections.authorize(ctx, config.URL); err != nil {
		return FetchResult{}, sourceFetchError(FailurePolicy, "target_rejected", "Remote source target is outside the allowed network policy", false, err)
	}
	transport, err := f.transport(config, connections)
	if err != nil {
		return FetchResult{}, err
	}
	defer transport.CloseIdleConnections()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, config.URL.String(), nil)
	if err != nil {
		return FetchResult{}, sourceFetchError(FailureConfig, "invalid_request", "Remote source request is invalid", false, err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("Accept", "application/yaml, application/x-yaml, text/yaml, text/plain;q=0.8, */*;q=0.1")
	if config.UserAgent != "" {
		request.Header.Set("User-Agent", config.UserAgent)
	} else {
		request.Header.Set("User-Agent", "submux-runtime")
	}
	if validConditionalHeader(conditional.ETag, 1024) {
		request.Header.Set("If-None-Match", conditional.ETag)
	}
	if validHTTPDate(conditional.LastModified) {
		request.Header.Set("If-Modified-Since", conditional.LastModified)
	}
	if config.Username != "" {
		request.SetBasicAuth(config.Username, config.Password)
	}

	initialTarget := config.Target
	client := &http.Client{
		Transport: transport,
		Timeout:   config.Timeout,
		CheckRedirect: func(next *http.Request, previous []*http.Request) error {
			if len(previous) >= MaximumRedirects {
				return errors.New("remote source exceeded the redirect limit")
			}
			if err := connections.authorize(next.Context(), next.URL); err != nil {
				return err
			}
			nextTarget, err := targetForURL(next.URL)
			if err != nil {
				return err
			}
			if nextTarget != initialTarget {
				for _, name := range []string{
					"Authorization",
					"Cookie",
					"Proxy-Authorization",
					"Referer",
					"If-None-Match",
					"If-Modified-Since",
				} {
					next.Header.Del(name)
				}
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return FetchResult{}, classifyTransportError(err)
	}
	defer response.Body.Close()

	etag := safeETag(response.Header.Get("ETag"))
	lastModified := safeLastModified(response.Header.Get("Last-Modified"))
	if response.StatusCode == http.StatusNotModified {
		return FetchResult{
			ETag:         etag,
			LastModified: lastModified,
			NotModified:  true,
			Route:        config.Route,
		}, nil
	}
	if response.StatusCode != http.StatusOK {
		return FetchResult{}, classifyHTTPStatus(response, f.now(), config.RefreshIntervalSeconds)
	}
	body, err := readLimitedBody(response, config.MaxResponseBytes)
	if err != nil {
		return FetchResult{}, err
	}
	if len(body) == 0 {
		return FetchResult{}, sourceFetchError(FailureConfig, "empty_response", "Remote source returned an empty configuration", false, nil)
	}
	digest := sha256.Sum256(body)
	return FetchResult{
		Body:         body,
		SHA256:       hex.EncodeToString(digest[:]),
		ETag:         etag,
		LastModified: lastModified,
		Route:        config.Route,
	}, nil
}

func (f *Fetcher) transport(config SourceConfig, connections *connectionTargets) (*http.Transport, error) {
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if config.CustomCAPEM != "" && !rootCAs.AppendCertsFromPEM([]byte(config.CustomCAPEM)) {
		return nil, sourceFetchError(FailureConfig, "custom_ca_invalid", "Remote source custom CA is invalid", false, nil)
	}
	dialContext := func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("remote source connection address is invalid")
		}
		connection, err := connections.lookup(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolveTarget(ctx, f.resolver(), connection.host)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if err := config.authorizeAddress(connection.target, connection.scheme, address); err != nil {
				return nil, err
			}
		}
		var dialErrors []error
		for _, targetIP := range addresses {
			exactAddress := net.JoinHostPort(targetIP.String(), port)
			var conn net.Conn
			if config.Route == runtimeapi.SourceRouteMihomo {
				conn, err = dialSOCKS5(ctx, f.MihomoAddress, exactAddress)
			} else if f.DirectDialContext != nil {
				conn, err = f.DirectDialContext(ctx, network, exactAddress)
			} else {
				conn, err = (&net.Dialer{}).DialContext(ctx, network, exactAddress)
			}
			if err == nil {
				return conn, nil
			}
			dialErrors = append(dialErrors, err)
		}
		return nil, errors.Join(dialErrors...)
	}
	if config.Route == runtimeapi.SourceRouteMihomo {
		if err := validateMihomoAddress(f.MihomoAddress); err != nil {
			return nil, sourceFetchError(FailureConfig, "mihomo_route_unavailable", "Current Mihomo route is not configured", false, err)
		}
	}
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            dialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			RootCAs:            rootCAs,
			InsecureSkipVerify: config.SkipTLSVerify,
		},
		TLSHandshakeTimeout:   min(config.Timeout, 15*time.Second),
		ResponseHeaderTimeout: config.Timeout,
		ExpectContinueTimeout: time.Second,
	}, nil
}

type authorizedConnection struct {
	host   string
	scheme string
	target string
}

type connectionTargets struct {
	config        SourceConfig
	resolver      Resolver
	initialScheme string
	mu            sync.RWMutex
	targets       map[string]authorizedConnection
}

func (c *connectionTargets) authorize(ctx context.Context, targetURL *url.URL) error {
	_, target, err := c.config.validateRequestTarget(ctx, c.resolver, targetURL, c.initialScheme)
	if err != nil {
		return err
	}
	normalized, _, _, err := normalizeSourceURL(targetURL.String())
	if err != nil {
		return err
	}
	key := strings.ToLower(normalized.Host)
	c.mu.Lock()
	c.targets[key] = authorizedConnection{
		host:   normalized.Hostname(),
		scheme: normalized.Scheme,
		target: target,
	}
	c.mu.Unlock()
	return nil
}

func (c *connectionTargets) lookup(address string) (authorizedConnection, error) {
	c.mu.RLock()
	connection, ok := c.targets[strings.ToLower(address)]
	c.mu.RUnlock()
	if !ok {
		return authorizedConnection{}, errors.New("remote source connection target was not authorized")
	}
	return connection, nil
}

func (f *Fetcher) resolver() Resolver {
	if f != nil && f.Resolver != nil {
		return f.Resolver
	}
	return net.DefaultResolver
}

func (f *Fetcher) now() time.Time {
	if f != nil && f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}

func classifyTransportError(err error) error {
	var fetchError *FetchError
	if errors.As(err, &fetchError) {
		return fetchError
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var certificateInvalid x509.CertificateInvalidError
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostnameError) ||
		errors.As(err, &certificateInvalid) ||
		errors.As(err, &recordHeader) {
		return sourceFetchError(FailureConfig, "tls_error", "Remote source TLS verification failed", false, err)
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "network policy") ||
		strings.Contains(lower, "target-bound authorization") ||
		strings.Contains(lower, "forbidden metadata") ||
		strings.Contains(lower, "cannot redirect to http") ||
		strings.Contains(lower, "another target") {
		return sourceFetchError(FailurePolicy, "target_rejected", "Remote source target is outside the allowed network policy", false, err)
	}
	return sourceFetchError(FailureTemporary, "network_error", "Remote source could not be reached using the selected route", true, err)
}

func classifyHTTPStatus(response *http.Response, now time.Time, normalIntervalSeconds int64) error {
	status := response.StatusCode
	class := FailureConfig
	result := "http_error"
	message := "Remote source returned an unsuccessful HTTP status"
	retryable := false
	var retryAfter time.Duration
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		class = FailureAuth
		result = "authentication_failed"
		message = "Remote source authentication was rejected"
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		class = FailureTemporary
		result = "server_unavailable"
		message = "Remote source server is temporarily unavailable"
		retryable = true
		retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), now, time.Duration(normalIntervalSeconds)*time.Second)
	}
	return &FetchError{
		Class:      class,
		Result:     result,
		Message:    message,
		Retryable:  retryable,
		RetryAfter: retryAfter,
	}
}

func readLimitedBody(response *http.Response, maximum int64) ([]byte, error) {
	if maximum < 1 || maximum > MaximumResponseBytes {
		return nil, sourceFetchError(FailureConfig, "response_limit_invalid", "Remote source response limit is invalid", false, nil)
	}
	if response.ContentLength > MaximumCompressedBytes {
		return nil, sourceFetchError(FailureConfig, "response_too_large", "Remote source response exceeds the configured size limit", false, nil)
	}
	compressed := &countingReader{reader: io.LimitReader(response.Body, MaximumCompressedBytes+1)}
	var reader io.Reader = compressed
	switch strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gzipReader, err := gzip.NewReader(compressed)
		if err != nil {
			return nil, sourceFetchError(FailureConfig, "invalid_compression", "Remote source returned invalid compressed content", false, err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	default:
		return nil, sourceFetchError(FailureConfig, "unsupported_compression", "Remote source used an unsupported content encoding", false, nil)
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, sourceFetchError(FailureTemporary, "read_error", "Remote source response could not be read", true, err)
	}
	if int64(len(body)) > maximum || compressed.count > MaximumCompressedBytes {
		return nil, sourceFetchError(FailureConfig, "response_too_large", "Remote source response exceeds the configured size limit", false, nil)
	}
	return body, nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.count += int64(count)
	return count, err
}

func validateMihomoAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("Mihomo route address is invalid")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return errors.New("Mihomo route must use a loopback address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("Mihomo route port is invalid")
	}
	return nil
}

func targetForURL(targetURL *url.URL) (string, error) {
	_, target, _, err := normalizeSourceURL(targetURL.String())
	return target, err
}

func parseRetryAfter(value string, now time.Time, normalInterval time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" || normalInterval <= 0 {
		return 0
	}
	var delay time.Duration
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		delay = time.Duration(seconds) * time.Second
	} else if retryAt, err := http.ParseTime(value); err == nil {
		delay = retryAt.Sub(now)
	}
	if delay < time.Second {
		return 0
	}
	maximum := min(normalInterval, 24*time.Hour)
	if delay > maximum {
		return 0
	}
	return delay
}

func safeETag(value string) string {
	if !validConditionalHeader(value, 1024) {
		return ""
	}
	return value
}

func safeLastModified(value string) string {
	if !validHTTPDate(value) {
		return ""
	}
	return value
}

func validConditionalHeader(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !hasControl(value)
}

func validHTTPDate(value string) bool {
	if value == "" || len(value) > 128 || hasControl(value) {
		return false
	}
	_, err := http.ParseTime(value)
	return err == nil
}

func sourceFetchError(class, result, message string, retryable bool, cause error) *FetchError {
	return &FetchError{
		Class:     class,
		Result:    result,
		Message:   message,
		Retryable: retryable,
		Cause:     cause,
	}
}
