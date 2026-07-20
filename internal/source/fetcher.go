package source

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"submux/internal/lifecycle"
	"submux/internal/node"
	"submux/internal/parse"
	"submux/internal/resourceproxy"
	"submux/internal/store"
)

const maxUpstreamBytes = 10 << 20

type Fetcher struct {
	store           *store.Store
	intervalChanged chan struct{}
	rebuilder       interface{ RebuildAll() error }
}

func (f *Fetcher) SetRebuilder(rebuilder interface{ RebuildAll() error }) { f.rebuilder = rebuilder }

func NewFetcher(s *store.Store) *Fetcher {
	return &Fetcher{
		store:           s,
		intervalChanged: make(chan struct{}, 1),
	}
}

// FetchOne 拉取单个源,成功写入缓存,失败记录 last_error 并返回错误。
func (f *Fetcher) FetchOne(ctx context.Context, src store.Source) error {
	if src.Kind != "" && src.Kind != store.SourceKindSubscription {
		return fmt.Errorf("source %d is not a subscription source", src.ID)
	}
	raw, userinfoHeader, route, directError, proxyError, err := f.download(ctx, src, false)
	if err != nil {
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		_ = f.store.SetCacheFetchResult(src.ID, "", directError, proxyError)
		return err
	}
	return f.commitDownloaded(ctx, src, raw, userinfoHeader, route, directError, proxyError)
}

func (f *Fetcher) commitDownloaded(_ context.Context, src store.Source, raw, userinfoHeader, route, directError, proxyError string) error {
	nodes, err := parse.ParseSubscription(raw)
	if err != nil || len(nodes) == 0 {
		if err == nil {
			err = fmt.Errorf("subscription contains no nodes")
		}
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		return err
	}
	records := make([]store.NodeRecord, 0, len(nodes))
	var notices []*store.NodeNotice
	proxyCount := 0
	for _, parsedNode := range nodes {
		record, convertErr := node.FromParsed(src.ID, store.SourceKindSubscription, parsedNode)
		if convertErr != nil {
			_ = f.store.UpsertCacheError(src.ID, convertErr.Error())
			return convertErr
		}
		record.Role = "proxy"
		if notice := lifecycle.ClassifyLabel(record.Name); notice != nil {
			record.Notice = notice
			notices = append(notices, notice)
			if notice.Confidence == "high" {
				record.Role = "notice"
				record.Fingerprint = "notice:" + notice.Type + ":" + record.Fingerprint
			}
		}
		if record.Role == "proxy" {
			proxyCount++
		}
		records = append(records, record)
	}
	previous := store.SubscriptionMetadata{}
	previousUserinfo := ""
	if cache, cacheErr := f.store.GetCache(src.ID); cacheErr == nil {
		previous = cache.Metadata
		previousUserinfo = cache.UserinfoJSON
	}
	headerMetadata, headerOK := lifecycle.ParseSubscriptionUserinfo(userinfoHeader, time.Now())
	metadata := lifecycle.MergeMetadata(previous, headerMetadata, headerOK, notices, time.Now())
	userinfoJSON := previousUserinfo
	if parsed, ok := ParseUserinfo(userinfoHeader); ok {
		encoded, _ := json.Marshal(parsed)
		userinfoJSON = string(encoded)
	}
	if err := f.store.CommitSourceRefreshV3(src.ID, records, userinfoJSON, metadata, proxyCount == 0); err != nil {
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		return err
	}
	_ = f.store.SetCacheFetchResult(src.ID, route, directError, proxyError)
	status := lifecycle.Evaluate(src, store.Cache{LastSuccessAt: time.Now().UTC().Format(time.RFC3339), Metadata: metadata}, time.Now())
	_, _ = f.store.RecordLifecycleState(src.ID, status.Entitlement)
	if proxyCount == 0 {
		err := fmt.Errorf("subscription contains no proxy nodes; previous proxy snapshot preserved")
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		if f.rebuilder != nil {
			_ = f.rebuilder.RebuildAll()
		}
		return err
	}
	if f.rebuilder != nil {
		_ = f.rebuilder.RebuildAll()
	}
	return nil
}

func (f *Fetcher) download(ctx context.Context, src store.Source, forceProxy bool) (raw, userinfoHeader, route, directError, proxyError string, err error) {
	if forceProxy {
		config, loadErr := resourceproxy.Load(f.store)
		if loadErr != nil || config.Mode == resourceproxy.ModeDirect {
			return "", "", "", "", "platform resource proxy is not configured", errors.New("platform resource proxy is not configured")
		}
		client, clientErr := sourceClient(config, src.URL)
		if clientErr != nil {
			return "", "", "", "", "platform resource proxy is invalid", clientErr
		}
		raw, userinfoHeader, err = f.downloadOnce(ctx, client, src)
		if err != nil {
			return "", "", "", "", err.Error(), err
		}
		return raw, userinfoHeader, "platform_proxy", "", "", nil
	}

	directClient, clientErr := sourceClient(resourceproxy.Config{Mode: resourceproxy.ModeDirect}, src.URL)
	if clientErr != nil {
		return "", "", "", "invalid upstream URL", "", clientErr
	}
	raw, userinfoHeader, err = f.downloadOnce(ctx, directClient, src)
	if err == nil {
		return raw, userinfoHeader, "direct", "", "", nil
	}
	directError = err.Error()
	var upstream *upstreamError
	parsed, _ := url.Parse(src.URL)
	if src.FetchMode != store.SourceFetchProxyBackup || parsed == nil || parsed.Scheme != "https" || !errors.As(err, &upstream) || !upstream.proxyFallback {
		return "", "", "", directError, "", err
	}
	config, loadErr := resourceproxy.Load(f.store)
	if loadErr != nil || config.Mode == resourceproxy.ModeDirect {
		return "", "", "", directError, "platform resource proxy is not configured", err
	}
	proxyClient, clientErr := sourceClient(config, src.URL)
	if clientErr != nil {
		return "", "", "", directError, "platform resource proxy is invalid", err
	}
	raw, userinfoHeader, proxyErr := f.downloadOnce(ctx, proxyClient, src)
	if proxyErr != nil {
		proxyError = proxyErr.Error()
		return "", "", "", directError, proxyError, fmt.Errorf("direct request failed; platform proxy request failed")
	}
	return raw, userinfoHeader, "platform_proxy", directError, "", nil
}

func (f *Fetcher) FetchOneViaPlatformProxy(ctx context.Context, src store.Source) error {
	if src.Kind != store.SourceKindSubscription {
		return errors.New("only subscription sources can be refreshed")
	}
	parsed, _ := url.Parse(src.URL)
	if parsed == nil || parsed.Scheme != "https" {
		return errors.New("platform proxy retry requires an HTTPS subscription URL")
	}
	originalMode := src.FetchMode
	src.FetchMode = store.SourceFetchDirectOnly
	raw, userinfo, route, directError, proxyError, err := f.download(ctx, src, true)
	src.FetchMode = originalMode
	if err != nil {
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		_ = f.store.SetCacheFetchResult(src.ID, "", directError, proxyError)
		return err
	}
	return f.commitDownloaded(ctx, src, raw, userinfo, route, directError, proxyError)
}

type upstreamError struct {
	message       string
	proxyFallback bool
}

func (e *upstreamError) Error() string { return e.message }

func (f *Fetcher) downloadOnce(ctx context.Context, client *http.Client, src store.Source) (raw, userinfoHeader string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return "", "", &upstreamError{message: "invalid upstream URL"}
	}
	req.Header.Set("User-Agent", defUA(src.UserAgent))
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", &upstreamError{message: "upstream request canceled or timed out"}
		}
		return "", "", &upstreamError{message: "upstream request failed", proxyFallback: networkFallbackAllowed(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", &upstreamError{message: fmt.Sprintf("upstream status %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBytes+1))
	if err != nil {
		return "", "", &upstreamError{message: "upstream body read failed", proxyFallback: networkFallbackAllowed(err) || errors.Is(err, io.ErrUnexpectedEOF)}
	}
	if len(body) > maxUpstreamBytes {
		return "", "", &upstreamError{message: fmt.Sprintf("upstream body exceeds %d bytes", maxUpstreamBytes)}
	}
	return string(body), resp.Header.Get("Subscription-Userinfo"), nil
}

func sourceClient(config resourceproxy.Config, sourceURL string) (*http.Client, error) {
	client, err := resourceproxy.NewClient(config, 30*time.Second)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("invalid upstream URL")
	}
	if parsed.Scheme == "https" {
		client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("subscription redirect must remain HTTPS")
			}
			return nil
		}
	}
	return client, nil
}

func networkFallbackAllowed(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalidCertificate) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func defUA(ua string) string {
	if ua == "" {
		return "clash-verge/v2.0.0"
	}
	return ua
}

// RunOnce 拉取所有启用的源。单源失败只记录,不中断其余源。
func (f *Fetcher) RunOnce(ctx context.Context) error {
	srcs, err := f.store.ListEnabledSources()
	if err != nil {
		return err
	}
	for _, src := range srcs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if src.Kind == store.SourceKindManual {
			continue
		}
		_ = f.FetchOne(ctx, src) // 错误已写入缓存的 last_error
	}
	return nil
}

// NotifyIntervalChanged 让后台循环立即重新读取拉取间隔。
func (f *Fetcher) NotifyIntervalChanged() {
	select {
	case f.intervalChanged <- struct{}{}:
	default:
	}
}

// Loop 立即跑一次,然后按可动态更新的间隔运行,直到 ctx 取消。
func (f *Fetcher) Loop(ctx context.Context, fallback time.Duration) {
	_ = f.RunOnce(ctx)
	_ = f.SweepLifecycle()
	timer := time.NewTimer(f.currentInterval(fallback))
	defer timer.Stop()
	lifecycleTicker := time.NewTicker(time.Hour)
	defer lifecycleTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = f.RunOnce(ctx)
			resetTimer(timer, f.currentInterval(fallback))
		case <-f.intervalChanged:
			resetTimer(timer, f.currentInterval(fallback))
		case <-lifecycleTicker.C:
			_ = f.SweepLifecycle()
		}
	}
}

// SweepLifecycle detects time-driven entitlement transitions even when no
// network refresh happens at the exact expiry boundary.
func (f *Fetcher) SweepLifecycle() error {
	sources, err := f.store.ListSources()
	if err != nil {
		return err
	}
	changed := false
	for _, source := range sources {
		if source.Kind != store.SourceKindSubscription {
			continue
		}
		cache, _ := f.store.GetCache(source.ID)
		status := lifecycle.Evaluate(source, cache, time.Now())
		transitioned, recordErr := f.store.RecordLifecycleState(source.ID, status.Entitlement)
		if recordErr != nil {
			return recordErr
		}
		changed = changed || transitioned
	}
	if changed && f.rebuilder != nil {
		return f.rebuilder.RebuildAll()
	}
	return nil
}

func (f *Fetcher) currentInterval(fallback time.Duration) time.Duration {
	seconds, err := f.store.GetSettingInt("fetch_interval_sec", int(fallback/time.Second))
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if fallback <= 0 {
		return 3 * time.Hour
	}
	return fallback
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
