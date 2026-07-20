package compiler

import (
	"fmt"
	"reflect"

	"submux/internal/store"
)

type builtinTemplate struct {
	name, engine, scenario, description, engineVersion, content string
	slots                                                       []store.TemplateSlot
}

const builtinTemplatesCatalogVersion = "builtin_templates_v11"

var retiredBuiltinTemplateKeys = map[string]bool{
	templateKey(EngineMihomo, "Mihomo 桌面"):              true,
	templateKey(EngineMihomo, "Mihomo 桌面 TUN（推荐）"):      true,
	templateKey(EngineMihomo, "Mihomo 网关"):              true,
	templateKey(EngineMihomo, "Mihomo 服务器 Sidecar（推荐）"): true,
	templateKey(EngineSingBox, "sing-box 桌面"):           true,
	templateKey(EngineSingBox, "sing-box 服务器"):          true,
}

var builtinTemplateAliases = map[string]string{
	templateKey(EngineMihomo, "Mihomo 桌面 TUN（推荐）"):      templateKey(EngineMihomo, "Mihomo 桌面 TUN"),
	templateKey(EngineMihomo, "Mihomo 服务器 Sidecar（推荐）"): templateKey(EngineMihomo, "Mihomo Linux 服务器"),
}

func templateKey(engine, name string) string {
	return engine + "\x00" + name
}

// EnsureBuiltinTemplates reconciles the pre-release starter catalog. Renamed
// built-ins keep their IDs and current version IDs, user-created templates are
// left untouched, and superseded built-ins are deleted unless an existing
// output subscription still references one of their versions. Referenced
// superseded templates remain hidden, retired compatibility records.
func (s *Service) EnsureBuiltinTemplates() error {
	seeded, _ := s.store.GetSetting(builtinTemplatesCatalogVersion)
	if seeded == "1" {
		return nil
	}
	items := builtinTemplates()
	for _, item := range items {
		if err := ValidateTemplate(item.engine, item.content, item.slots); err != nil {
			return fmt.Errorf("builtin template %q: %w", item.name, err)
		}
	}

	templates, err := s.store.ListTemplates()
	if err != nil {
		return err
	}
	subscriptions, err := s.store.ListOutputSubscriptions()
	if err != nil {
		return err
	}
	referencedVersions := make(map[int64]bool, len(subscriptions))
	for _, subscription := range subscriptions {
		referencedVersions[subscription.TemplateVersionID] = true
	}

	catalogKeys := make(map[string]bool, len(items))
	targets := make(map[string]store.Template, len(items))
	selectedIDs := make(map[int64]bool, len(items))
	for _, item := range items {
		catalogKeys[templateKey(item.engine, item.name)] = true
	}
	// Exact current names take precedence when a partially migrated database
	// contains both the old and new catalog entries.
	for _, template := range templates {
		key := templateKey(template.Engine, template.Name)
		if catalogKeys[key] && targets[key].ID == 0 {
			targets[key] = template
			selectedIDs[template.ID] = true
		}
	}
	for _, template := range templates {
		key := templateKey(template.Engine, template.Name)
		targetKey := builtinTemplateAliases[key]
		if targetKey == "" || targets[targetKey].ID != 0 {
			continue
		}
		targets[targetKey] = template
		selectedIDs[template.ID] = true
	}

	for _, template := range templates {
		if selectedIDs[template.ID] {
			continue
		}
		key := templateKey(template.Engine, template.Name)
		if !retiredBuiltinTemplateKeys[key] && !catalogKeys[key] {
			continue
		}
		versions, err := s.store.ListTemplateVersions(template.ID)
		if err != nil {
			return err
		}
		isReferenced := false
		for _, version := range versions {
			if referencedVersions[version.ID] {
				isReferenced = true
				break
			}
		}
		if isReferenced {
			template.Status = "retired"
			if _, err := s.store.SaveTemplate(template); err != nil {
				return err
			}
			continue
		}
		if err := s.store.DeleteTemplate(template.ID); err != nil {
			return err
		}
	}

	changed := false
	for _, item := range items {
		key := templateKey(item.engine, item.name)
		target := targets[key]
		if target.ID == 0 {
			target.ID, err = s.store.SaveTemplate(store.Template{
				Name: item.name, Engine: item.engine, Scenario: item.scenario,
				Description: item.description, Status: "draft",
			})
			if err != nil {
				return err
			}
		} else {
			target.Name = item.name
			target.Engine = item.engine
			target.Scenario = item.scenario
			target.Description = item.description
			if target.CurrentVersionID == 0 {
				target.Status = "draft"
			} else {
				target.Status = "published"
			}
			if _, err := s.store.SaveTemplate(target); err != nil {
				return err
			}
		}

		if target.CurrentVersionID == 0 {
			if _, err := s.store.PublishTemplateVersion(target.ID, item.engineVersion, item.content, item.slots); err != nil {
				return err
			}
			continue
		}
		current, err := s.store.GetTemplateVersion(target.CurrentVersionID)
		if err != nil {
			return err
		}
		if current.Content == item.content && current.EngineVersion == item.engineVersion && reflect.DeepEqual(current.Slots, item.slots) {
			continue
		}
		if _, err := s.store.OverwriteTemplateVersionForDevelopment(current.ID, item.engineVersion, item.content, item.slots); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		// Successful subscriptions receive refreshed artifacts. Failures retain
		// their last-good artifact under the normal rebuild policy.
		_ = s.RebuildAll()
	}
	return s.store.SetSetting(builtinTemplatesCatalogVersion, "1")
}

// EnsureBuiltinTemplates reconciles the pre-release starter catalog to the
// single template that has been verified in a real deployment. User-created
// templates are left untouched. Superseded built-ins are deleted unless an
// existing output subscription still references one of their versions; those
// templates are retained as hidden, retired compatibility records.
func (s *Service) ensureBuiltinTemplatesV9() error {
	seeded, _ := s.store.GetSetting(builtinTemplatesCatalogVersion)
	if seeded == "1" {
		return nil
	}
	item := mihomoDesktopTUNTemplate()
	if err := ValidateTemplate(item.engine, item.content, item.slots); err != nil {
		return fmt.Errorf("builtin template %q: %w", item.name, err)
	}

	templates, err := s.store.ListTemplates()
	if err != nil {
		return err
	}
	subscriptions, err := s.store.ListOutputSubscriptions()
	if err != nil {
		return err
	}
	referencedVersions := make(map[int64]bool, len(subscriptions))
	for _, subscription := range subscriptions {
		referencedVersions[subscription.TemplateVersionID] = true
	}

	var target store.Template
	for _, template := range templates {
		key := templateKey(template.Engine, template.Name)
		if key == templateKey(item.engine, item.name) && target.ID == 0 {
			target = template
			continue
		}
		if !retiredBuiltinTemplateKeys[key] {
			continue
		}
		versions, listErr := s.store.ListTemplateVersions(template.ID)
		if listErr != nil {
			return listErr
		}
		isReferenced := false
		for _, version := range versions {
			if referencedVersions[version.ID] {
				isReferenced = true
				break
			}
		}
		if isReferenced {
			template.Status = "retired"
			if _, err := s.store.SaveTemplate(template); err != nil {
				return err
			}
			continue
		}
		if err := s.store.DeleteTemplate(template.ID); err != nil {
			return err
		}
	}

	if target.ID == 0 {
		target.ID, err = s.store.SaveTemplate(store.Template{
			Name: item.name, Engine: item.engine, Scenario: item.scenario,
			Description: item.description, Status: "draft",
		})
		if err != nil {
			return err
		}
	} else {
		target.Name = item.name
		target.Engine = item.engine
		target.Scenario = item.scenario
		target.Description = item.description
		if target.CurrentVersionID == 0 {
			target.Status = "draft"
		} else {
			target.Status = "published"
		}
		if _, err := s.store.SaveTemplate(target); err != nil {
			return err
		}
	}

	changed := false
	if target.CurrentVersionID == 0 {
		if _, err := s.store.PublishTemplateVersion(target.ID, item.engineVersion, item.content, item.slots); err != nil {
			return err
		}
	} else {
		current, err := s.store.GetTemplateVersion(target.CurrentVersionID)
		if err != nil {
			return err
		}
		if current.Content != item.content || current.EngineVersion != item.engineVersion || !reflect.DeepEqual(current.Slots, item.slots) {
			if _, err := s.store.OverwriteTemplateVersionForDevelopment(current.ID, item.engineVersion, item.content, item.slots); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		// Successful subscriptions receive refreshed artifacts. Failures retain
		// their last-good artifact under the normal rebuild policy.
		_ = s.RebuildAll()
	}
	return s.store.SetSetting(builtinTemplatesCatalogVersion, "1")
}

func builtinTemplates() []builtinTemplate {
	return []builtinTemplate{mihomoDesktopTUNTemplate(), mihomoLinuxServerTemplate()}
}

// legacyBuiltinTemplates documents the exact pre-v9 catalog. It is not seeded;
// its names are kept here for migration tests and historical readability.
func legacyBuiltinTemplates() []builtinTemplate {
	slot := []store.TemplateSlot{{Key: "primary", Target: "PROXY", Mode: "replace", Required: true}}
	return []builtinTemplate{
		{
			name: "Mihomo 桌面", engine: EngineMihomo, scenario: "desktop", engineVersion: "mihomo current",
			description: "本机 mixed-port、规则模式与手动节点选择，适合桌面客户端。",
			content: `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
ipv6: true
unified-delay: true
tcp-concurrent: true
dns:
  enable: true
  ipv6: true
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  default-nameserver:
    - 223.5.5.5
    - 1.1.1.1
  nameserver:
    - https://dns.alidns.com/dns-query
    - https://1.1.1.1/dns-query
proxy-groups:
  - name: PROXY
    type: select
    proxies: []
rules:
  - GEOIP,LAN,DIRECT,no-resolve
  - MATCH,PROXY
`,
			slots: slot,
		},
		mihomoDesktopTUNTemplate(),
		{
			name: "Mihomo 网关", engine: EngineMihomo, scenario: "gateway", engineVersion: "mihomo current",
			description: "开启 LAN 与 TUN 自动路由，适合作为 Linux 网关或旁路客户端。",
			content: `mixed-port: 7890
allow-lan: true
bind-address: "*"
mode: rule
log-level: info
ipv6: true
unified-delay: true
tcp-concurrent: true
dns:
  enable: true
  listen: 0.0.0.0:1053
  ipv6: true
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  default-nameserver:
    - 223.5.5.5
    - 1.1.1.1
  nameserver:
    - https://dns.alidns.com/dns-query
    - https://1.1.1.1/dns-query
tun:
  enable: true
  stack: system
  auto-route: true
  auto-redirect: true
  auto-detect-interface: true
  strict-route: true
  dns-hijack:
    - any:53
    - tcp://any:53
proxy-groups:
  - name: PROXY
    type: select
    proxies: []
rules:
  - GEOIP,LAN,DIRECT,no-resolve
  - MATCH,PROXY
`,
			slots: slot,
		},
		mihomoServerSidecarTemplate(),
		{
			name: "sing-box 桌面", engine: EngineSingBox, scenario: "desktop", engineVersion: "sing-box 1.14",
			description: "本机 mixed 入口并设置系统代理，使用当前 DNS server 与 route action 格式。",
			content: `{
  "log": {"level": "info", "timestamp": true},
  "dns": {
    "servers": [
      {"type": "local", "tag": "local"},
      {"type": "https", "tag": "remote", "server": "1.1.1.1"}
    ],
    "final": "remote"
  },
  "inbounds": [
    {"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": 2080, "set_system_proxy": true}
  ],
  "outbounds": [
    {"type": "selector", "tag": "PROXY", "outbounds": ["AUTO", "DIRECT"], "default": "AUTO"},
    {"type": "urltest", "tag": "AUTO", "outbounds": [], "url": "https://www.gstatic.com/generate_204", "interval": "3m", "tolerance": 50},
    {"type": "direct", "tag": "DIRECT"}
  ],
  "route": {
    "rules": [
      {"action": "sniff"},
      {"ip_is_private": true, "action": "route", "outbound": "DIRECT"}
    ],
    "final": "PROXY",
    "auto_detect_interface": true,
    "default_domain_resolver": "local"
  }
}`,
			slots: []store.TemplateSlot{{Key: "primary", Target: "AUTO", Mode: "replace", Required: true}},
		},
		{
			name: "sing-box 服务器", engine: EngineSingBox, scenario: "server", engineVersion: "sing-box 1.14",
			description: "仅监听回环地址的无界面 sidecar 配置，适合服务器上的应用通过 mixed 代理出站。",
			content: `{
  "log": {"level": "warn", "timestamp": true},
  "dns": {
    "servers": [
      {"type": "local", "tag": "local"},
      {"type": "tls", "tag": "remote", "server": "1.1.1.1"}
    ],
    "final": "remote"
  },
  "inbounds": [
    {"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": 2080}
  ],
  "outbounds": [
    {"type": "selector", "tag": "PROXY", "outbounds": ["AUTO", "DIRECT"], "default": "AUTO"},
    {"type": "urltest", "tag": "AUTO", "outbounds": [], "url": "https://www.gstatic.com/generate_204", "interval": "5m", "tolerance": 50},
    {"type": "direct", "tag": "DIRECT"}
  ],
  "route": {
    "rules": [
      {"action": "sniff"},
      {"ip_is_private": true, "action": "route", "outbound": "DIRECT"}
    ],
    "final": "PROXY",
    "auto_detect_interface": true,
    "default_domain_resolver": "local"
  },
  "experimental": {"cache_file": {"enabled": true, "store_dns": true}}
}`,
			slots: []store.TemplateSlot{{Key: "primary", Target: "AUTO", Mode: "replace", Required: true}},
		},
	}
}

func mihomoServerSidecarTemplate() builtinTemplate {
	return builtinTemplate{
		name: "Mihomo 服务器 Sidecar（推荐）", engine: EngineMihomo, scenario: "server", engineVersion: "mihomo 1.19+",
		description: "仅监听回环地址、由 submux-agent 管理控制 API 的服务器 sidecar 配置。",
		content: `mixed-port: 7890
allow-lan: false
bind-address: 127.0.0.1
mode: rule
log-level: warning
ipv6: false
unified-delay: true
tcp-concurrent: true
dns:
  enable: true
  ipv6: false
  enhanced-mode: redir-host
  default-nameserver:
    - 223.5.5.5
    - 1.1.1.1
  nameserver:
    - https://dns.alidns.com/dns-query
    - https://1.1.1.1/dns-query
proxy-groups:
  - name: PROXY
    type: select
    proxies: []
rules:
  - GEOIP,LAN,DIRECT,no-resolve
  - MATCH,PROXY
`,
		slots: []store.TemplateSlot{{Key: "primary", Target: "PROXY", Mode: "replace", Required: true}},
	}
}

func mihomoLinuxServerTemplate() builtinTemplate {
	return builtinTemplate{
		name:          "Mihomo Linux 服务器",
		engine:        EngineMihomo,
		scenario:      "server",
		engineVersion: "mihomo 1.19+",
		description:   "仅向本机应用提供 mixed 代理，不接管路由或系统配置；由 rootless submux-agent 管理 Mihomo。",
		content: `mixed-port: 7890
allow-lan: false
bind-address: 127.0.0.1
mode: rule
log-level: warning
ipv6: false
find-process-mode: off
unified-delay: true
tcp-concurrent: true
profile:
  store-selected: true
dns:
  enable: true
  cache-algorithm: arc
  ipv6: false
  enhanced-mode: redir-host
  default-nameserver:
    - tls://223.5.5.5
    - tls://223.6.6.6
  proxy-server-nameserver:
    - https://doh.pub/dns-query
    - https://dns.alidns.com/dns-query
  direct-nameserver:
    - https://doh.pub/dns-query
    - https://dns.alidns.com/dns-query
  nameserver:
    - "https://1.1.1.1/dns-query#PROXY"
    - "https://8.8.8.8/dns-query#PROXY"
proxy-groups:
  - name: PROXY
    type: select
    proxies: []
  - name: MEDIA
    type: select
    proxies:
      - PROXY
rules: []
`,
		slots: []store.TemplateSlot{
			{Key: "primary", Target: "PROXY", Mode: "replace", Required: true},
			{Key: "media", Target: "MEDIA", Mode: "append", Required: false},
		},
	}
}

func mihomoDesktopTUNTemplate() builtinTemplate {
	return builtinTemplate{
		name:          "Mihomo 桌面 TUN",
		engine:        EngineMihomo,
		scenario:      "desktop",
		engineVersion: "mihomo 1.19+",
		description:   "IPv4-only、mixed TUN、fake-ip DNS、MRS 分流与 WireGuard 网段绕行；适合由 Mihomo 独占默认路由的桌面客户端。",
		content: `mixed-port: 7890
allow-lan: false
bind-address: 127.0.0.1
mode: rule
log-level: warning
ipv6: false
find-process-mode: strict
unified-delay: true
tcp-concurrent: true
profile:
  store-selected: true
  store-fake-ip: true
tun:
  enable: true
  stack: mixed
  auto-route: true
  auto-detect-interface: true
  strict-route: true
  route-exclude-address:
    - 192.0.2.0/24
    - 198.51.100.0/24
  dns-hijack:
    - any:53
    - tcp://any:53
sniffer:
  enable: true
  parse-pure-ip: true
  override-destination: false
  sniff:
    HTTP:
      ports: [80, 8080-8880]
      override-destination: true
    TLS:
      ports: [443, 8443]
    QUIC:
      ports: [443, 8443]
  skip-domain:
    - Mijia Cloud
    - +.push.apple.com
dns:
  enable: true
  cache-algorithm: arc
  ipv6: false
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  fake-ip-filter:
    - +.lan
    - +.local
    - +.msftconnecttest.com
    - +.msftncsi.com
    - time.windows.com
    - time.apple.com
    - localhost.ptlogin2.qq.com
  default-nameserver:
    - tls://223.5.5.5
    - tls://223.6.6.6
  proxy-server-nameserver:
    - https://doh.pub/dns-query
    - https://dns.alidns.com/dns-query
  direct-nameserver:
    - https://doh.pub/dns-query
    - https://dns.alidns.com/dns-query
  nameserver-policy:
    "+.lan": system
    "+.local": system
  nameserver:
    - "https://1.1.1.1/dns-query#PROXY"
    - "https://8.8.8.8/dns-query#PROXY"
proxy-groups:
  - name: PROXY
    type: select
    proxies: []
  - name: MEDIA
    type: select
    proxies:
      - PROXY
rules: []
`,
		slots: []store.TemplateSlot{
			{Key: "primary", Target: "PROXY", Mode: "replace", Required: true},
			{Key: "media", Target: "MEDIA", Mode: "append", Required: false},
		},
	}
}
