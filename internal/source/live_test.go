package source_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"submux/internal/compiler"
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
	for index, rawURL := range urls {
		id, err := st.CreateSource(store.Source{Name: "live-" + string(rune('A'+index)), URL: strings.TrimSpace(rawURL)})
		if err != nil {
			t.Fatal(err)
		}
		src, _ := st.GetSource(id)
		if err := fetcher.FetchOne(context.Background(), src); err != nil {
			t.Fatalf("source %d refresh failed: %v", index+1, err)
		}
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) == 0 {
		t.Fatalf("normalized node library is empty: %v", err)
	}
	protocols := map[string]int{}
	for _, value := range nodes {
		protocols[value.Protocol]++
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
	allSetID, _ := st.SaveNodeSet(store.NodeSet{Name: "all live nodes", Enabled: true})
	mihomo, err := service.Preview(store.Profile{
		Engine: compiler.EngineMihomo, TemplateVersionID: versions[compiler.EngineMihomo],
		Bindings: []store.ProfileBinding{{Slot: "primary", NodeSetID: allSetID}},
	})
	if err != nil {
		t.Fatalf("mihomo live compile failed: %v", err)
	}

	singBoxCompatible := 0
	rejected := map[string]int{}
	for _, value := range nodes {
		nodeSetID, _ := st.SaveNodeSet(store.NodeSet{Name: "node", NodeIDs: []int64{value.ID}, Enabled: true})
		if _, err := service.Preview(store.Profile{
			Engine: compiler.EngineSingBox, TemplateVersionID: versions[compiler.EngineSingBox],
			Bindings: []store.ProfileBinding{{Slot: "primary", NodeSetID: nodeSetID}},
		}); err == nil {
			singBoxCompatible++
		} else {
			rejected[classifyCompileError(err)]++
		}
	}
	if singBoxCompatible == 0 {
		t.Fatal("no live node can be converted losslessly to sing-box")
	}
	t.Logf("live v2 result: sources=%d nodes=%d protocols=%s mihomo=%d sing-box-compatible=%d rejected=%s", len(urls), len(nodes), summarizeCounts(protocols), mihomo.NodeCount, singBoxCompatible, summarizeCounts(rejected))
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
