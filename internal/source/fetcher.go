package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"submux/internal/store"
)

type Fetcher struct {
	store  *store.Store
	client *http.Client
}

func NewFetcher(s *store.Store) *Fetcher {
	return &Fetcher{
		store:  s,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// FetchOne 拉取单个源,成功写入缓存,失败记录 last_error 并返回错误。
func (f *Fetcher) FetchOne(ctx context.Context, src store.Source) error {
	raw, userinfoJSON, err := f.download(ctx, src)
	if err != nil {
		_ = f.store.UpsertCacheError(src.ID, err.Error())
		return err
	}
	// M1:节点解析在 M2 引入,这里 nodes_json 先存空数组。
	if err := f.store.UpsertCacheSuccess(src.ID, raw, "[]", userinfoJSON); err != nil {
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
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

// Loop 立即跑一次,然后每 interval 跑一次,直到 ctx 取消。
func (f *Fetcher) Loop(ctx context.Context, interval time.Duration) {
	_ = f.RunOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = f.RunOnce(ctx)
		}
	}
}
