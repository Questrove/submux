package source_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"submux/internal/compiler"
	"submux/internal/lifecycle"
	"submux/internal/source"
	"submux/internal/store"
)

// TestLiveSubscriptions exercises the complete v2 ingestion and compilation
// path without committing private subscription URLs. It is skipped unless the
// caller supplies SUBMUX_LIVE_URLS separated by |||.
func TestLiveSubscriptions(t *testing.T) {
	rawURLs := strings.TrimSpace(os.Getenv("SUBMUX_LIVE_URLS"))
	if rawURLs == "" {
		t.Skip("SUBMUX_LIVE_URLS is not set")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fetcher := source.NewFetcher(st)
	urls := strings.Split(rawURLs, "|||")
	successfulSources := 0
	failures := map[string]int{}
	for index, rawURL := range urls {
		id, err := st.CreateSource(store.Source{Name: "live-" + string(rune('A'+index)), URL: strings.TrimSpace(rawURL)})
		if err != nil {
			t.Fatal(err)
		}
		src, _ := st.GetSource(id)
		if err := fetcher.FetchOne(context.Background(), src); err != nil {
			failures[classifyFetchError(err)]++
			t.Logf("source %d refresh failed: %s", index+1, classifyFetchError(err))
			continue
		}
		successfulSources++
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) == 0 || successfulSources == 0 {
		t.Fatalf("normalized node library is empty: %v", err)
	}
	protocols, noticeTypes, states := map[string]int{}, map[string]int{}, map[string]int{}
	proxyNodes := make([]store.NodeRecord, 0, len(nodes))
	for _, value := range nodes {
		if value.Role == "notice" {
			if value.Notice != nil {
				noticeTypes[value.Notice.Type]++
			}
			continue
		}
		protocols[value.Protocol]++
		proxyNodes = append(proxyNodes, value)
	}
	sources, _ := st.ListSources()
	for _, value := range sources {
		cache, _ := st.GetCache(value.ID)
		states[lifecycle.Evaluate(value, cache, time.Now()).Entitlement]++
	}

	service := compiler.New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	templates, _ := st.ListTemplates()
	versions := map[string]int64{}
	for _, template := range templates {
		if versions[template.Engine] == 0 {
			versions[template.Engine] = template.CurrentVersionID
		}
	}
	allNodeIDs := make([]int64, 0, len(proxyNodes))
	for _, value := range proxyNodes {
		allNodeIDs = append(allNodeIDs, value.ID)
	}
	mihomo, err := service.Preview(store.OutputSubscription{
		Engine: compiler.EngineMihomo, TemplateVersionID: versions[compiler.EngineMihomo],
		Bindings: []store.SubscriptionBinding{{Slot: "primary", NodeIDs: allNodeIDs}},
	})
	if err != nil {
		t.Fatalf("mihomo live compile failed: %v", err)
	}

	singBoxCompatible := 0
	rejected := map[string]int{}
	for _, value := range proxyNodes {
		if _, err := service.Preview(store.OutputSubscription{
			Engine: compiler.EngineSingBox, TemplateVersionID: versions[compiler.EngineSingBox],
			Bindings: []store.SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{value.ID}}},
		}); err == nil {
			singBoxCompatible++
		} else {
			rejected[classifyCompileError(err)]++
		}
	}
	if singBoxCompatible == 0 {
		t.Fatal("no live node can be converted losslessly to sing-box")
	}
	t.Logf("live v3 result: requested=%d refreshed=%d fetch-failures=%s proxies=%d notices=%d notice-types=%s lifecycle=%s protocols=%s mihomo=%d sing-box-compatible=%d rejected=%s", len(urls), successfulSources, summarizeCounts(failures), len(proxyNodes), len(nodes)-len(proxyNodes), summarizeCounts(noticeTypes), summarizeCounts(states), summarizeCounts(protocols), mihomo.NodeCount, singBoxCompatible, summarizeCounts(rejected))
}

func classifyFetchError(err error) string {
	message := err.Error()
	for _, item := range []struct{ contains, category string }{
		{"request failed", "network"},
		{"timed out", "timeout"},
		{"upstream status", "http-status"},
		{"contains no proxy", "no-proxy-nodes"},
		{"contains no nodes", "no-nodes"},
		{"exceeds", "oversized"},
	} {
		if strings.Contains(message, item.contains) {
			return item.category
		}
	}
	return "parse-or-schema"
}

func classifyCompileError(err error) string {
	message := err.Error()
	for _, item := range []struct{ contains, category string }{
		{"fields not representable", "unsupported-fields"},
		{"protocol", "unsupported-protocol"},
		{"transport", "unsupported-transport"},
		{"Shadowsocks", "shadowsocks-schema"},
		{"Hysteria2", "hysteria2-schema"},
		{"VLESS", "vless-schema"},
		{"VMess", "vmess-schema"},
	} {
		if strings.Contains(message, item.contains) {
			return item.category
		}
	}
	return "other"
}

func summarizeCounts(values map[string]int) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", key, values[key]))
	}
	return strings.Join(parts, ",")
}
