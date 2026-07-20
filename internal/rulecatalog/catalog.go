package rulecatalog

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"submux/internal/store"
)

//go:generate go run ./cmd/cataloggen -output catalog.tsv

// catalogData is a release-time snapshot of MetaCubeX/meta-rules-dat. Keeping
// the index in the binary makes the editor usable before the target machine has
// working access to GitHub. Selected .mrs files are still downloaded by
// Mihomo, through the PROXY group configured by the compiler.
//
//go:embed catalog.tsv
var catalogData string

const (
	ActionDirect = "direct"
	ActionProxy  = "proxy"
	ActionMedia  = "media"
	ActionReject = "reject"
)

type Entry struct {
	Key               string `json:"key"`
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	Kind              string `json:"kind"`
	Behavior          string `json:"behavior"`
	Group             string `json:"group"`
	Description       string `json:"description,omitempty"`
	RecommendedAction string `json:"recommended_action"`
	DefaultEnabled    bool   `json:"default_enabled"`
}

type Snapshot struct {
	Source    string  `json:"source"`
	Commit    string  `json:"commit"`
	Entries   []Entry `json:"entries"`
	UpdatedAt string  `json:"updated_at,omitempty"`
	Origin    string  `json:"origin"`
}

type entryMetadata struct {
	displayName, group, description, action string
	defaultEnabled                          bool
}

var curated = map[string]entryMetadata{
	"geosite/private":                          {"私有域名", "基础规则", "局域网和保留域名", ActionDirect, true},
	"geoip/private":                            {"私有地址", "基础规则", "局域网和保留地址", ActionDirect, true},
	"geosite/category-ads-all":                 {"广告域名", "广告过滤", "MetaCubeX 汇总的广告域名", ActionReject, false},
	"geosite/microsoft@cn":                     {"微软国内服务", "微软", "微软在中国大陆可直连的服务", ActionDirect, true},
	"geosite/onedrive":                         {"OneDrive", "微软", "OneDrive 服务", ActionDirect, true},
	"geosite/microsoft":                        {"微软其他服务", "微软", "微软服务的宽泛集合", ActionProxy, true},
	"geosite/apple-cn":                         {"Apple 国内服务", "Apple", "Apple 在中国大陆可直连的服务", ActionDirect, true},
	"geosite/apple-music":                      {"Apple Music", "Apple", "Apple Music 服务", ActionMedia, true},
	"geosite/apple-tvplus":                     {"Apple TV+", "Apple", "Apple TV+ 服务", ActionMedia, true},
	"geosite/apple":                            {"Apple 其他服务", "Apple", "Apple 服务的宽泛集合", ActionDirect, true},
	"geosite/steam@cn":                         {"Steam 国内服务", "游戏", "Steam 在中国大陆可直连的服务", ActionDirect, true},
	"geosite/category-game-platforms-download": {"游戏平台下载", "游戏", "常见游戏平台的下载流量", ActionDirect, true},
	"geosite/steam":                            {"Steam 其他服务", "游戏", "Steam 商店和社区等服务", ActionProxy, true},
	"geosite/github":                           {"GitHub", "开发服务", "GitHub 及相关服务", ActionProxy, true},
	"geosite/google":                           {"Google", "开发服务", "Google 服务", ActionProxy, true},
	"geosite/openai":                           {"OpenAI", "开发服务", "OpenAI 服务", ActionProxy, true},
	"geosite/category-media-cn":                {"国内流媒体", "流媒体", "中国大陆流媒体服务", ActionDirect, true},
	"geosite/bilibili":                         {"哔哩哔哩", "流媒体", "哔哩哔哩国内服务", ActionDirect, true},
	"geosite/youtube":                          {"YouTube", "流媒体", "YouTube 视频服务", ActionMedia, true},
	"geosite/netflix":                          {"Netflix", "流媒体", "Netflix 流媒体服务", ActionMedia, true},
	"geoip/netflix":                            {"Netflix 地址", "流媒体", "Netflix IP 地址", ActionMedia, true},
	"geosite/disney":                           {"Disney+", "流媒体", "Disney 流媒体服务", ActionMedia, true},
	"geosite/hbo":                              {"HBO / Max", "流媒体", "HBO 和 Max 流媒体服务", ActionMedia, true},
	"geosite/primevideo":                       {"Prime Video", "流媒体", "Amazon Prime Video", ActionMedia, true},
	"geosite/spotify":                          {"Spotify", "流媒体", "Spotify 音乐服务", ActionMedia, true},
	"geosite/tiktok":                           {"TikTok", "流媒体", "TikTok 服务", ActionMedia, true},
	"geosite/bahamut":                          {"巴哈姆特", "流媒体", "巴哈姆特动画疯", ActionMedia, true},
	"geosite/biliintl":                         {"哔哩哔哩国际版", "流媒体", "BiliIntl 服务", ActionMedia, true},
	"geosite/hulu":                             {"Hulu", "流媒体", "Hulu 流媒体服务", ActionMedia, true},
	"geosite/twitch":                           {"Twitch", "流媒体", "Twitch 直播服务", ActionMedia, true},
	"geosite/cn":                               {"国内域名", "地区分流", "中国大陆域名集合", ActionDirect, true},
	"geosite/geolocation-!cn":                  {"非国内域名", "地区分流", "中国大陆以外的域名集合", ActionProxy, true},
	"geoip/cn":                                 {"国内地址", "地区分流", "中国大陆 IP 地址", ActionDirect, true},
}

var defaultOrder = []string{
	"geosite/private", "geoip/private",
	"geosite/microsoft@cn", "geosite/onedrive", "geosite/microsoft",
	"geosite/apple-cn", "geosite/apple-music", "geosite/apple-tvplus", "geosite/apple",
	"geosite/steam@cn", "geosite/category-game-platforms-download", "geosite/steam",
	"geosite/github", "geosite/google", "geosite/openai",
	"geosite/category-media-cn", "geosite/bilibili",
	"geosite/youtube", "geosite/netflix", "geoip/netflix", "geosite/disney", "geosite/hbo",
	"geosite/primevideo", "geosite/spotify", "geosite/tiktok", "geosite/bahamut",
	"geosite/biliintl", "geosite/hulu", "geosite/twitch",
	"geosite/geolocation-!cn", "geosite/cn", "geoip/cn",
}

var snapshot = parseCatalog(catalogData)

func Catalog() Snapshot {
	out := snapshot
	out.Entries = append([]Entry(nil), snapshot.Entries...)
	return out
}

func ActiveCatalog(st *store.Store) (Snapshot, error) {
	commit, err := st.GetSetting("rule_catalog_active_commit")
	if err != nil {
		return Snapshot{}, err
	}
	if commit == "" {
		return Catalog(), nil
	}
	return CatalogAt(st, commit)
}

func CatalogAt(st *store.Store, commit string) (Snapshot, error) {
	if commit == "" || commit == snapshot.Commit {
		return Catalog(), nil
	}
	raw, err := st.GetRuleCatalogSnapshot(commit)
	if err != nil {
		return Snapshot{}, err
	}
	var result Snapshot
	if err := json.Unmarshal(raw, &result); err != nil {
		return Snapshot{}, fmt.Errorf("decode rule catalog snapshot: %w", err)
	}
	if result.Commit != commit || len(result.Entries) == 0 {
		return Snapshot{}, fmt.Errorf("stored rule catalog snapshot %q is invalid", commit)
	}
	return result, nil
}

func SaveActiveCatalog(st *store.Store, value Snapshot) error {
	if value.Commit == "" || len(value.Entries) == 0 {
		return fmt.Errorf("rule catalog snapshot is empty")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := st.PutRuleCatalogSnapshot(value.Commit, raw); err != nil {
		return err
	}
	return st.SetSetting("rule_catalog_active_commit", value.Commit)
}

func Commit() string { return snapshot.Commit }

func Lookup(key string) (Entry, bool) {
	return LookupIn(snapshot, key)
}

func LookupIn(value Snapshot, key string) (Entry, bool) {
	index := sort.Search(len(value.Entries), func(i int) bool { return value.Entries[i].Key >= key })
	if index >= len(value.Entries) || value.Entries[index].Key != key {
		return Entry{}, false
	}
	return value.Entries[index], true
}

func ValidAction(action string) bool {
	switch action {
	case ActionDirect, ActionProxy, ActionMedia, ActionReject:
		return true
	default:
		return false
	}
}

func DefaultProfile() store.RuleProfile {
	return DefaultProfileFor(snapshot)
}

func DefaultProfileFor(value Snapshot) store.RuleProfile {
	rules := make([]store.RuleSelection, 0, len(defaultOrder))
	for _, key := range defaultOrder {
		entry, ok := LookupIn(value, key)
		if !ok {
			panic("rule catalog is missing default entry " + key)
		}
		rules = append(rules, store.RuleSelection{Key: key, Action: entry.RecommendedAction})
	}
	return store.RuleProfile{
		Key:            "default",
		Name:           "常用规则",
		Description:    "国内服务直连，国际服务使用主代理，流媒体使用流媒体代理。",
		Builtin:        true,
		Rules:          rules,
		FallbackAction: ActionProxy,
		CatalogCommit:  value.Commit,
	}
}

func parseCatalog(data string) Snapshot {
	result := Snapshot{Source: "MetaCubeX/meta-rules-dat", Origin: "embedded"}
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# commit=") {
			result.Commit = strings.TrimPrefix(line, "# commit=")
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 || (parts[0] != "geosite" && parts[0] != "geoip") {
			panic(fmt.Sprintf("invalid embedded rule catalog row %q", line))
		}
		result.Entries = append(result.Entries, makeEntry(parts[0], parts[1]))
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
	if result.Commit == "" || len(result.Entries) == 0 {
		panic("embedded rule catalog is empty")
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Key < result.Entries[j].Key })
	return result
}

func NewSnapshot(commit string, names map[string][]string, updatedAt string) Snapshot {
	result := Snapshot{Source: "MetaCubeX/meta-rules-dat", Commit: commit, UpdatedAt: updatedAt, Origin: "github"}
	for _, kind := range []string{"geosite", "geoip"} {
		for _, name := range names[kind] {
			result.Entries = append(result.Entries, makeEntry(kind, name))
		}
	}
	sort.Slice(result.Entries, func(i int, j int) bool { return result.Entries[i].Key < result.Entries[j].Key })
	return result
}

func makeEntry(kind, name string) Entry {
	key := kind + "/" + name
	behavior, group, action := "domain", "域名规则", ActionProxy
	if kind == "geoip" {
		behavior, group = "ipcidr", "IP 规则"
	}
	entry := Entry{Key: key, Name: name, DisplayName: name, Kind: kind, Behavior: behavior, Group: group, RecommendedAction: action}
	if meta, ok := curated[key]; ok {
		entry.DisplayName, entry.Group, entry.Description = meta.displayName, meta.group, meta.description
		entry.RecommendedAction, entry.DefaultEnabled = meta.action, meta.defaultEnabled
	} else if strings.Contains(name, "ads") || strings.Contains(name, "adblock") || strings.Contains(name, "easylist") {
		entry.Group, entry.RecommendedAction = "广告过滤", ActionReject
	}
	return entry
}
