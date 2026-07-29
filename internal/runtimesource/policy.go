package runtimesource

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/idna"

	"submux/internal/runtimeapi"
)

const (
	DefaultRefreshInterval = 6 * time.Hour
	MinimumRefreshInterval = 15 * time.Minute
	MaximumRefreshInterval = 7 * 24 * time.Hour
	DefaultFetchTimeout    = 30 * time.Second
	MinimumFetchTimeout    = 5 * time.Second
	MaximumFetchTimeout    = 2 * time.Minute
	DefaultResponseBytes   = 8 << 20
	MaximumResponseBytes   = 10 << 20
	MaximumCompressedBytes = 10 << 20
	MaximumCustomCABytes   = 256 << 10
	MaximumRedirects       = 5
)

var (
	sharedAddressPrefix = netip.MustParsePrefix("100.64.0.0/10")
	aliyunMetadataIP    = netip.MustParseAddr("100.100.100.200")
	azureWireServerIP   = netip.MustParseAddr("168.63.129.16")
	awsMetadataIPv6     = netip.MustParseAddr("fd00:ec2::254")
)

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type SourceConfig struct {
	Type                   string
	Name                   string
	URL                    *url.URL
	Target                 string
	RedactedTarget         string
	Route                  string
	UserAgent              string
	Username               string
	Password               string
	AuthorizedTarget       string
	AllowPrivate           bool
	AllowHTTP              bool
	CustomCAPEM            string
	SkipTLSVerify          bool
	RefreshIntervalSeconds int64
	Timeout                time.Duration
	MaxResponseBytes       int64
}

func NormalizeDraft(draft runtimeapi.RemoteSourceDraft) (SourceConfig, error) {
	sourceType := draft.Type
	if sourceType == "" {
		sourceType = runtimeapi.SourceTypeRemoteHTTP
	}
	if sourceType != runtimeapi.SourceTypeRemoteHTTP &&
		sourceType != runtimeapi.SourceTypeSubmuxOutput {
		return SourceConfig{}, errors.New("remote source type is invalid")
	}
	name := strings.TrimSpace(draft.Name)
	if name == "" || len(name) > 128 || hasControl(name) {
		return SourceConfig{}, errors.New("remote source name is invalid")
	}
	if len(draft.URL) == 0 || len(draft.URL) > 4096 {
		return SourceConfig{}, errors.New("remote source URL is invalid")
	}
	parsed, target, redacted, err := normalizeSourceURL(draft.URL)
	if err != nil {
		return SourceConfig{}, err
	}
	route := draft.Route
	if route == "" {
		route = runtimeapi.SourceRouteDirect
	}
	if route != runtimeapi.SourceRouteDirect && route != runtimeapi.SourceRouteMihomo {
		return SourceConfig{}, errors.New("remote source route is invalid")
	}
	if len(draft.UserAgent) > 256 || hasControl(draft.UserAgent) {
		return SourceConfig{}, errors.New("remote source User-Agent is invalid")
	}
	if len(draft.Username) > 256 || hasControl(draft.Username) {
		return SourceConfig{}, errors.New("remote source username is invalid")
	}
	if len(draft.Password) > 4096 || hasControl(draft.Password) {
		return SourceConfig{}, errors.New("remote source password is invalid")
	}
	if draft.Password != "" && draft.Username == "" {
		return SourceConfig{}, errors.New("remote source password requires a username")
	}

	refreshInterval := DefaultRefreshInterval
	if draft.RefreshIntervalSeconds != nil {
		refreshInterval = time.Duration(*draft.RefreshIntervalSeconds) * time.Second
	}
	if refreshInterval != 0 &&
		(refreshInterval < MinimumRefreshInterval || refreshInterval > MaximumRefreshInterval) {
		return SourceConfig{}, errors.New("remote source refresh interval is outside the allowed range")
	}
	timeout := DefaultFetchTimeout
	if draft.TimeoutSeconds != 0 {
		timeout = time.Duration(draft.TimeoutSeconds) * time.Second
	}
	if timeout < MinimumFetchTimeout || timeout > MaximumFetchTimeout {
		return SourceConfig{}, errors.New("remote source timeout is outside the allowed range")
	}
	maxResponseBytes := int64(DefaultResponseBytes)
	if draft.MaxResponseBytes != 0 {
		maxResponseBytes = draft.MaxResponseBytes
	}
	if maxResponseBytes < 1024 || maxResponseBytes > MaximumResponseBytes {
		return SourceConfig{}, errors.New("remote source response limit is outside the allowed range")
	}

	if len(draft.CustomCAPEM) > MaximumCustomCABytes {
		return SourceConfig{}, errors.New("remote source custom CA exceeds the allowed size")
	}
	if draft.CustomCAPEM != "" && draft.SkipTLSVerify {
		return SourceConfig{}, errors.New("custom CA and skipped TLS verification cannot be enabled together")
	}
	if (draft.CustomCAPEM != "" || draft.SkipTLSVerify) && parsed.Scheme != "https" {
		return SourceConfig{}, errors.New("TLS compatibility settings require HTTPS")
	}
	if draft.CustomCAPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(draft.CustomCAPEM)) {
			return SourceConfig{}, errors.New("remote source custom CA is invalid")
		}
	}

	authorizationRequired := draft.AllowPrivate ||
		draft.AllowHTTP ||
		draft.CustomCAPEM != "" ||
		draft.SkipTLSVerify
	authorizedTarget := strings.TrimSpace(draft.AuthorizedTarget)
	if authorizationRequired {
		normalizedAuthorization, err := normalizeAuthorizationTarget(authorizedTarget)
		if err != nil || normalizedAuthorization != target {
			return SourceConfig{}, errors.New("high-risk source settings require the exact normalized target")
		}
		authorizedTarget = normalizedAuthorization
	} else if authorizedTarget != "" {
		return SourceConfig{}, errors.New("remote source authorization target is unnecessary")
	}
	if draft.AllowHTTP && parsed.Scheme != "http" {
		return SourceConfig{}, errors.New("HTTP authorization is only valid for an HTTP source")
	}

	return SourceConfig{
		Type:                   sourceType,
		Name:                   name,
		URL:                    parsed,
		Target:                 target,
		RedactedTarget:         redacted,
		Route:                  route,
		UserAgent:              draft.UserAgent,
		Username:               draft.Username,
		Password:               draft.Password,
		AuthorizedTarget:       authorizedTarget,
		AllowPrivate:           draft.AllowPrivate,
		AllowHTTP:              draft.AllowHTTP,
		CustomCAPEM:            draft.CustomCAPEM,
		SkipTLSVerify:          draft.SkipTLSVerify,
		RefreshIntervalSeconds: int64(refreshInterval / time.Second),
		Timeout:                timeout,
		MaxResponseBytes:       maxResponseBytes,
	}, nil
}

func normalizeSourceURL(raw string) (*url.URL, string, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil ||
		parsed.Opaque != "" ||
		parsed.User != nil ||
		parsed.Fragment != "" ||
		parsed.Hostname() == "" {
		return nil, "", "", errors.New("remote source URL is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, "", "", errors.New("remote source URL must use HTTP or HTTPS")
	}
	host, err := normalizeHostname(parsed.Hostname())
	if err != nil || blockedMetadataHostname(host) {
		return nil, "", "", errors.New("remote source target is forbidden")
	}
	port, err := normalizedPort(scheme, parsed.Port())
	if err != nil {
		return nil, "", "", err
	}
	parsed.Scheme = scheme
	parsed.Host = net.JoinHostPort(host, port)
	target := scheme + "://" + parsed.Host
	return parsed, target, target + "/…", nil
}

func normalizeAuthorizationTarget(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("authorization target is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil ||
		parsed.Opaque != "" ||
		parsed.User != nil ||
		parsed.Fragment != "" ||
		parsed.RawQuery != "" ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.Hostname() == "" {
		return "", errors.New("authorization target is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("authorization target is invalid")
	}
	host, err := normalizeHostname(parsed.Hostname())
	if err != nil {
		return "", errors.New("authorization target is invalid")
	}
	port, err := normalizedPort(scheme, parsed.Port())
	if err != nil {
		return "", errors.New("authorization target is invalid")
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

func normalizeHostname(host string) (string, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" || hasControl(host) {
		return "", errors.New("remote source hostname is invalid")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Unmap().String(), nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 {
		return "", errors.New("remote source hostname is invalid")
	}
	return strings.ToLower(ascii), nil
}

func normalizedPort(scheme, rawPort string) (string, error) {
	if rawPort == "" {
		if scheme == "https" {
			return "443", nil
		}
		return "80", nil
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("remote source port is invalid")
	}
	return strconv.Itoa(port), nil
}

func (config SourceConfig) validateRequestTarget(
	ctx context.Context,
	resolver Resolver,
	targetURL *url.URL,
	initialScheme string,
) ([]netip.Addr, string, error) {
	normalized, target, _, err := normalizeSourceURL(targetURL.String())
	if err != nil {
		return nil, "", err
	}
	if initialScheme == "https" && normalized.Scheme != "https" {
		return nil, "", errors.New("HTTPS remote source cannot redirect to HTTP")
	}
	if target != config.Target && (config.CustomCAPEM != "" || config.SkipTLSVerify) {
		return nil, "", errors.New("TLS compatibility settings cannot be reused for another target")
	}
	addresses, err := resolveTarget(ctx, resolver, normalized.Hostname())
	if err != nil {
		return nil, "", err
	}
	for _, address := range addresses {
		if err := config.authorizeAddress(target, normalized.Scheme, address); err != nil {
			return nil, "", err
		}
	}
	return addresses, target, nil
}

func (config SourceConfig) authorizeAddress(target, scheme string, address netip.Addr) error {
	address = address.Unmap()
	if address == aliyunMetadataIP ||
		address == azureWireServerIP ||
		address == awsMetadataIPv6 ||
		address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() {
		return errors.New("remote source target is a forbidden metadata or link-local address")
	}
	if address.IsUnspecified() || address.IsMulticast() {
		return errors.New("remote source target is not connectable")
	}
	if scheme == "http" && !address.IsLoopback() &&
		!(config.AllowHTTP && target == config.AuthorizedTarget) {
		return errors.New("non-loopback HTTP source requires target-bound authorization")
	}
	if address.IsLoopback() {
		return nil
	}
	private := address.IsPrivate() || sharedAddressPrefix.Contains(address)
	if private && !(config.AllowPrivate && target == config.AuthorizedTarget) {
		return errors.New("private remote source target requires target-bound authorization")
	}
	return nil
}

func resolveTarget(ctx context.Context, resolver Resolver, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address.Unmap()}, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve remote source target: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("remote source target resolved to no addresses")
	}
	result := make([]netip.Addr, 0, len(addresses))
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	return result, nil
}

func blockedMetadataHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	switch host {
	case "metadata.google.internal",
		"metadata.goog",
		"instance-data.ec2.internal",
		"metadata.azure.com",
		"metadata.azure.internal",
		"metadata.oraclecloud.com":
		return true
	default:
		return false
	}
}

func hasControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
