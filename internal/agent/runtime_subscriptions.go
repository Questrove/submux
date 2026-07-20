package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"submux/internal/agentproto"
	"submux/internal/agentstate"
	"submux/internal/mihomo"
	"submux/internal/store"
)

const runtimeSubscriptionSecretKind = "subscription_url"

func validateAgentSubscriptionURL(value string) (*url.URL, error) {
	if len(value) == 0 || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return nil, errors.New("subscription URL is invalid")
	}
	parsed, err := url.Parse(value)
	host := ""
	if parsed != nil {
		host = parsed.Hostname()
	}
	if err != nil || parsed.Scheme != "https" || host == "" || len(host) > 253 || strings.ContainsAny(host, "\r\n\x00") || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("subscription URL must be an HTTPS URL without credentials or a fragment")
	}
	return parsed, nil
}

func (d *Daemon) runtimeSubscriptionHTTPClient() *http.Client {
	if d.SubscriptionHTTP != nil {
		return d.SubscriptionHTTP
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("subscription address is invalid")
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("subscription host could not be resolved")
		}
		publicFound := false
		for _, candidate := range addresses {
			if publicSubscriptionIP(candidate) {
				publicFound = true
				connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port))
				if dialErr == nil {
					return connection, nil
				}
			}
		}
		if publicFound {
			return nil, errors.New("subscription host could not be reached")
		}
		return nil, errors.New("subscription host resolves only to local or private addresses")
	}
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many subscription redirects")
		}
		if request.URL.Scheme != "https" {
			return errors.New("subscription redirect must use HTTPS")
		}
		return nil
	}}
	d.SubscriptionHTTP = client
	return client
}

func publicSubscriptionIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	return !cgnat.Contains(ip)
}

func (d *Daemon) consumeSubscriptionURL(ctx context.Context, ref string) (string, error) {
	secret, err := d.Control.FetchRuntimeSecret(ctx, ref)
	if err != nil {
		return "", err
	}
	if secret.Kind != runtimeSubscriptionSecretKind {
		return "", errors.New("runtime secret kind is invalid")
	}
	if _, err := validateAgentSubscriptionURL(secret.Value); err != nil {
		return "", err
	}
	return secret.Value, nil
}

func (d *Daemon) fetchRuntimeSubscription(ctx context.Context, value agentstate.RuntimeSubscription) (agentstate.RuntimeSubscription, error) {
	if value.PlatformSubscriptionID > 0 {
		return d.fetchPlatformRuntimeSubscription(ctx, value)
	}
	parsed, err := validateAgentSubscriptionURL(value.URL)
	if err != nil {
		return value, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return value, err
	}
	request.Header.Set("Accept", "application/yaml, text/yaml, application/x-yaml, text/plain;q=0.8")
	request.Header.Set("User-Agent", "submux-agent/1")
	if value.ETag != "" {
		request.Header.Set("If-None-Match", value.ETag)
	}
	if value.LastModified != "" {
		request.Header.Set("If-Modified-Since", value.LastModified)
	}
	response, err := d.runtimeSubscriptionHTTPClient().Do(request)
	if err != nil {
		return value, fmt.Errorf("download subscription: %w", err)
	}
	defer response.Body.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	if response.StatusCode == http.StatusNotModified {
		if len(value.Config) == 0 {
			return value, errors.New("subscription returned not modified without a cached config")
		}
		value.LastUpdatedAt, value.LastError = now, ""
		return value, nil
	}
	if response.StatusCode != http.StatusOK {
		return value, fmt.Errorf("download subscription: server returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (10<<20)+1))
	if err != nil {
		return value, err
	}
	if len(body) == 0 || len(body) > 10<<20 {
		return value, errors.New("subscription config is empty or exceeds 10 MiB")
	}
	// ProxyEndpoint also performs the required top-level YAML mapping check.
	if _, _, err := mihomoProxyEndpoint(body); err != nil {
		return value, fmt.Errorf("validate subscription config: %w", err)
	}
	digest := sha256.Sum256(body)
	value.Config, value.Revision = body, hex.EncodeToString(digest[:])
	value.ETag, value.LastModified = response.Header.Get("ETag"), response.Header.Get("Last-Modified")
	value.Host, value.LastUpdatedAt, value.LastError = parsed.Hostname(), now, ""
	value.UsedBytes, value.TotalBytes, value.ExpiresAt = parseSubscriptionUserInfo(response.Header.Get("Subscription-Userinfo"))
	return value, nil
}

func (d *Daemon) fetchPlatformRuntimeSubscription(ctx context.Context, value agentstate.RuntimeSubscription) (agentstate.RuntimeSubscription, error) {
	if value.PlatformSubscriptionID <= 0 || value.URL != "" {
		return value, errors.New("platform subscription identity is invalid")
	}
	published, err := d.Control.FetchPlatformSubscription(ctx, value.PlatformSubscriptionID)
	if err != nil {
		return value, fmt.Errorf("download platform subscription: %w", err)
	}
	if _, _, err := mihomoProxyEndpoint(published.Body); err != nil {
		return value, fmt.Errorf("validate subscription config: %w", err)
	}
	digest := sha256.Sum256(published.Body)
	now := time.Now().UTC().Format(time.RFC3339)
	value.Config, value.Revision = published.Body, hex.EncodeToString(digest[:])
	value.Host, value.LastUpdatedAt, value.LastError = "平台订阅", now, ""
	value.ETag, value.LastModified = "", ""
	value.UsedBytes, value.TotalBytes, value.ExpiresAt = 0, 0, ""
	return value, nil
}

// Kept behind a variable to make the structural check easy to isolate in tests.
var mihomoProxyEndpoint = func(body []byte) (int, string, error) {
	return mihomo.ProxyEndpoint(body)
}

func parseSubscriptionUserInfo(header string) (used, total int64, expiresAt string) {
	values := map[string]int64{}
	for _, field := range strings.Split(header, ";") {
		parts := strings.SplitN(strings.TrimSpace(field), "=", 2)
		if len(parts) != 2 {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err == nil && value >= 0 {
			values[strings.ToLower(strings.TrimSpace(parts[0]))] = value
		}
	}
	upload, download := values["upload"], values["download"]
	if upload <= (1<<63-1)-download {
		used = upload + download
	}
	total = values["total"]
	if expiry := values["expire"]; expiry > 0 {
		expiresAt = time.Unix(expiry, 0).UTC().Format(time.RFC3339)
	}
	return used, total, expiresAt
}

func runtimeSubscriptionSummary(value agentstate.RuntimeSubscription, activeID string) agentproto.RuntimeSubscriptionSummary {
	return agentproto.RuntimeSubscriptionSummary{
		ID: value.ID, Name: value.Name, Host: value.Host, PlatformSubscriptionID: value.PlatformSubscriptionID, Revision: value.Revision,
		UsedBytes: value.UsedBytes, TotalBytes: value.TotalBytes, ExpiresAt: value.ExpiresAt,
		LastUpdatedAt: value.LastUpdatedAt, LastError: value.LastError, Active: value.ID == activeID,
	}
}

func (d *Daemon) runtimeSubscriptionSummaries() ([]agentproto.RuntimeSubscriptionSummary, string, error) {
	values, err := d.State.ListRuntimeSubscriptions()
	if err != nil {
		return nil, "", err
	}
	runtimeState, err := d.State.Runtime()
	if err != nil {
		return nil, "", err
	}
	result := make([]agentproto.RuntimeSubscriptionSummary, 0, len(values))
	for _, value := range values {
		result = append(result, runtimeSubscriptionSummary(value, runtimeState.ActiveSubscriptionID))
	}
	return result, runtimeState.ActiveSubscriptionID, nil
}

func (d *Daemon) saveSubscriptionFetchError(value agentstate.RuntimeSubscription, err error) {
	value.LastError = safeOperationError(err)
	_, _ = d.State.SaveRuntimeSubscription(value)
}

func (d *Daemon) applyRuntimeSubscription(ctx context.Context, value agentstate.RuntimeSubscription) error {
	if len(value.Config) == 0 || value.Revision == "" {
		return errors.New("subscription has no downloaded Mihomo config")
	}
	coreStatus, err := d.Core.Status(ctx)
	if err != nil {
		return err
	}
	wasRunning := coreStatus.State == store.RuntimeRunning
	d.setOperationPhase("applying_config", 0, 0)
	result, applyErr := d.Deployer.Apply(ctx, value.Revision, value.Revision, value.Config)
	if !wasRunning {
		d.setOperationPhase("restoring_stopped_state", 0, 0)
		applyErr = errors.Join(applyErr, d.Core.Stop(ctx))
	}
	if applyErr != nil {
		_, _ = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.RejectedRevision, runtime.RecentError = value.Revision, safeOperationError(applyErr)
			return nil
		})
		return applyErr
	}
	selectionNotice := d.restoreProxySelections(ctx)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.ActiveSubscriptionID = value.ID
		runtime.AppliedRevision, runtime.RemoteRevision = value.Revision, value.Revision
		runtime.RejectedRevision, runtime.LastUpdateAt, runtime.RecentError = "", now, ""
		runtime.LastGoodRevision, runtime.SelectionNotice = result.PreviousRevision, selectionNotice
		runtime.ProxyPort, runtime.ProxyKind = result.ProxyPort, result.ProxyKind
		return nil
	})
	return err
}
