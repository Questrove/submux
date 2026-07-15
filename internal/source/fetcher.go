package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"submux/internal/lifecycle"
	"submux/internal/node"
	"submux/internal/parse"
	"submux/internal/store"
)

const maxUpstreamBytes = 10 << 20

type Fetcher struct {
	store           *store.Store
	client          *http.Client
	intervalChanged chan struct{}
	rebuilder       interface{ RebuildAll() error }
}

func (f *Fetcher) SetRebuilder(rebuilder interface{ RebuildAll() error }) { f.rebuilder = rebuilder }

func NewFetcher(s *store.Store) *Fetcher {
	return &Fetcher{
		store:           s,
		client:          &http.Client{Timeout: 30 * time.Second},
		intervalChanged: make(chan struct{}, 1),
	}
}

// FetchOne 拉取单个源,成功写入缓存,失败记录 last_error 并返回错误。
func (f *Fetcher) FetchOne(ctx context.Context, src store.Source) error {
	if src.Kind != "" && src.Kind != store.SourceKindSubscription {
		return fmt.Errorf("source %d is not a subscription source", src.ID)
	}
	raw, userinfoHeader, err := f.download(ctx, src)
	if err != nil {
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		return err
	}
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

func (f *Fetcher) download(ctx context.Context, src store.Source) (raw, userinfoHeader string, err error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		raw, userinfoHeader, err = f.downloadOnce(ctx, src)
		if err == nil {
			return raw, userinfoHeader, nil
		}
		last = err
		var upstream *upstreamError
		if ctx.Err() != nil || !errors.As(err, &upstream) || !upstream.retryable || attempt == 2 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", "", fmt.Errorf("upstream request canceled or timed out")
		case <-timer.C:
		}
	}
	return "", "", last
}

type upstreamError struct {
	message   string
	retryable bool
}

func (e *upstreamError) Error() string { return e.message }

func (f *Fetcher) downloadOnce(ctx context.Context, src store.Source) (raw, userinfoHeader string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return "", "", &upstreamError{message: "invalid upstream URL"}
	}
	req.Header.Set("User-Agent", defUA(src.UserAgent))
	resp, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", &upstreamError{message: "upstream request canceled or timed out"}
		}
		return "", "", &upstreamError{message: "upstream request failed", retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", &upstreamError{message: fmt.Sprintf("upstream status %d", resp.StatusCode), retryable: resp.StatusCode >= 500}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBytes+1))
	if err != nil {
		return "", "", &upstreamError{message: "upstream body read failed", retryable: true}
	}
	if len(body) > maxUpstreamBytes {
		return "", "", &upstreamError{message: fmt.Sprintf("upstream body exceeds %d bytes", maxUpstreamBytes)}
	}
	return string(body), resp.Header.Get("Subscription-Userinfo"), nil
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
