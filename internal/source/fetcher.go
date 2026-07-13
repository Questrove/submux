package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"submux/internal/parse"
	"submux/internal/store"
)

const maxUpstreamBytes = 10 << 20

type Fetcher struct {
	store           *store.Store
	client          *http.Client
	intervalChanged chan struct{}
}

func NewFetcher(s *store.Store) *Fetcher {
	return &Fetcher{
		store:           s,
		client:          &http.Client{Timeout: 30 * time.Second},
		intervalChanged: make(chan struct{}, 1),
	}
}

// FetchOne 拉取单个源,成功写入缓存,失败记录 last_error 并返回错误。
func (f *Fetcher) FetchOne(ctx context.Context, src store.Source) error {
	raw, userinfoJSON, err := f.download(ctx, src)
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
	nodesJSON, err := json.Marshal(nodes)
	if err != nil {
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		return err
	}
	if err := f.store.UpsertCacheSuccess(src.ID, raw, string(nodesJSON), userinfoJSON); err != nil {
		return err
	}
	return nil
}

func (f *Fetcher) download(ctx context.Context, src store.Source) (raw, userinfoJSON string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", defUA(src.UserAgent))
	resp, err := f.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(body) > maxUpstreamBytes {
		return "", "", fmt.Errorf("upstream body exceeds %d bytes", maxUpstreamBytes)
	}
	uiJSON := ""
	if u, ok := ParseUserinfo(resp.Header.Get("Subscription-Userinfo")); ok {
		b, _ := json.Marshal(u)
		uiJSON = string(b)
	}
	return string(body), uiJSON, nil
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
	timer := time.NewTimer(f.currentInterval(fallback))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = f.RunOnce(ctx)
			resetTimer(timer, f.currentInterval(fallback))
		case <-f.intervalChanged:
			resetTimer(timer, f.currentInterval(fallback))
		}
	}
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
