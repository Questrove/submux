package compiler

import (
	"fmt"
	"reflect"

	"submux/internal/store"
)

type builtinTemplate struct {
	name, engine, scenario, description, engineVersion, runtimeContract, content string
	slots                                                                        []store.TemplateSlot
}

// EnsureBuiltinTemplates installs and upgrades the versioned starter catalog.
// Templates remain ordinary records so administrators can publish newer
// immutable versions without changing existing output subscriptions.
func (s *Service) EnsureBuiltinTemplates() error {
	seededV8, _ := s.store.GetSetting("builtin_templates_v8")
	if seededV8 == "1" {
		return nil
	}
	seededV7, _ := s.store.GetSetting("builtin_templates_v7")
	if seededV7 == "1" {
		return s.upgradeBuiltinTemplatesV8()
	}
	seededV6, _ := s.store.GetSetting("builtin_templates_v6")
	if seededV6 == "1" {
		return s.upgradeBuiltinTemplatesV7()
	}
	seededV5, _ := s.store.GetSetting("builtin_templates_v5")
	if seededV5 == "1" {
		return s.upgradeBuiltinTemplatesV6()
	}
	seededV4, _ := s.store.GetSetting("builtin_templates_v4")
	if seededV4 == "1" {
		if err := s.upgradeBuiltinTemplatesV5(); err != nil {
			return err
		}
		return s.upgradeBuiltinTemplatesV6()
	}
	seededV3, _ := s.store.GetSetting("builtin_templates_v3")
	if seededV3 == "1" {
		if err := s.upgradeBuiltinTemplatesV5(); err != nil {
			return err
		}
		return s.upgradeBuiltinTemplatesV6()
	}
	seededV2, _ := s.store.GetSetting("builtin_templates_v2")
	items := builtinTemplates()
	if seededV2 == "1" {
		items = builtinTemplateUpgradesV3()
	}
	for _, item := range items {
		if err := ValidateTemplate(item.engine, item.content, item.slots); err != nil {
			return fmt.Errorf("builtin template %q: %w", item.name, err)
		}
	}
	existing, err := s.store.ListTemplates()
	if err != nil {
		return err
	}
	byKey := make(map[string]store.Template, len(existing))
	for _, item := range existing {
		byKey[item.Engine+"\x00"+item.Name] = item
	}
	for _, item := range items {
		key := item.engine + "\x00" + item.name
		if template, ok := byKey[key]; ok {
			if template.CurrentVersionID == 0 {
				if _, err := s.store.PublishTemplateVersionWithContract(template.ID, item.engineVersion, item.runtimeContract, item.content, item.slots); err != nil {
					return err
				}
			}
			continue
		}
		id, err := s.store.SaveTemplate(store.Template{
			Name: item.name, Engine: item.engine, Scenario: item.scenario,
			Description: item.description, Status: "draft",
		})
		if err != nil {
			return err
		}
		if _, err := s.store.PublishTemplateVersionWithContract(id, item.engineVersion, item.runtimeContract, item.content, item.slots); err != nil {
			return err
		}
		byKey[key] = store.Template{ID: id, Name: item.name, Engine: item.engine}
	}
	if err := s.store.SetSetting("builtin_templates_v2", "1"); err != nil {
		return err
	}
	if err := s.store.SetSetting("builtin_templates_v3", "1"); err != nil {
		return err
	}
	if err := s.store.SetSetting("builtin_templates_v4", "1"); err != nil {
		return err
	}
	if err := s.store.SetSetting("builtin_templates_v5", "1"); err != nil {
		return err
	}
	return s.upgradeBuiltinTemplatesV6()
}

func builtinTemplateUpgradesV3() []builtinTemplate {
	return []builtinTemplate{mihomoDesktopTUNTemplate()}
}

// upgradeBuiltinTemplatesV5 is a pre-release catalog migration. During the
// current development phase it intentionally rewrites the existing current
// version in place so subscriptions pinned to that version receive the fix.
func (s *Service) upgradeBuiltinTemplatesV5() error {
	item := mihomoDesktopTUNTemplate()
	if err := ValidateTemplate(item.engine, item.content, item.slots); err != nil {
		return fmt.Errorf("builtin template %q: %w", item.name, err)
	}
	templates, err := s.store.ListTemplates()
	if err != nil {
		return err
	}
	var template store.Template
	for _, candidate := range templates {
		if candidate.Engine == item.engine && candidate.Name == item.name {
			template = candidate
			break
		}
	}
	if template.ID == 0 {
		template.ID, err = s.store.SaveTemplate(store.Template{
			Name: item.name, Engine: item.engine, Scenario: item.scenario,
			Description: item.description, Status: "draft",
		})
		if err != nil {
			return err
		}
	}
	if template.CurrentVersionID == 0 {
		if _, err := s.store.PublishTemplateVersionWithContract(template.ID, item.engineVersion, item.runtimeContract, item.content, item.slots); err != nil {
			return err
		}
	} else {
		current, getErr := s.store.GetTemplateVersion(template.CurrentVersionID)
		if getErr != nil {
			return getErr
		}
		if current.Content != item.content || current.EngineVersion != item.engineVersion {
			if _, err := s.store.OverwriteTemplateVersionForDevelopmentWithContract(current.ID, item.engineVersion, item.runtimeContract, item.content, item.slots); err != nil {
				return err
			}
			// Successful subscriptions receive refreshed artifacts. Failures keep
			// their existing last-good artifact under the normal rebuild policy.
			_ = s.RebuildAll()
		}
	}
	if err := s.store.SetSetting("builtin_templates_v4", "1"); err != nil {
		return err
	}
	return s.store.SetSetting("builtin_templates_v5", "1")
}

// upgradeBuiltinTemplatesV6 removes the redundant PROXY -> AUTO -> node
// hierarchy from Mihomo templates. Selected nodes are injected directly into
// the single PROXY group; full direct mode remains a core mode switch.
func (s *Service) upgradeBuiltinTemplatesV6() error {
	if err := s.upgradeMihomoBuiltinTemplates(); err != nil {
		return err
	}
	if err := s.store.SetSetting("builtin_templates_v6", "1"); err != nil {
		return err
	}
	return s.upgradeBuiltinTemplatesV7()
}

// upgradeBuiltinTemplatesV7 changes the single PROXY group from url-test to
// select so latency tests never change the user's selected node.
func (s *Service) upgradeBuiltinTemplatesV7() error {
	if err := s.upgradeMihomoBuiltinTemplates(); err != nil {
		return err
	}
	if err := s.store.SetSetting("builtin_templates_v7", "1"); err != nil {
		return err
	}
	return s.upgradeBuiltinTemplatesV8()
}

// upgradeBuiltinTemplatesV8 adds the first Agent-compatible server template.
// Existing template versions remain without a runtime contract until an owner
// explicitly publishes one, so enabling the Agent never changes old behavior.
func (s *Service) upgradeBuiltinTemplatesV8() error {
	item := mihomoServerSidecarTemplate()
	if err := ValidateTemplate(item.engine, item.content, item.slots); err != nil {
		return fmt.Errorf("builtin template %q: %w", item.name, err)
	}
	templates, err := s.store.ListTemplates()
	if err != nil {
		return err
	}
	for _, template := range templates {
		if template.Engine != item.engine || template.Name != item.name {
			continue
		}
		if template.CurrentVersionID == 0 {
			_, err = s.store.PublishTemplateVersionWithContract(template.ID, item.engineVersion, item.runtimeContract, item.content, item.slots)
		} else {
			current, getErr := s.store.GetTemplateVersion(template.CurrentVersionID)
			if getErr != nil {
				return getErr
			}
			if current.Content != item.content || current.EngineVersion != item.engineVersion || current.RuntimeContract != item.runtimeContract || !reflect.DeepEqual(current.Slots, item.slots) {
				_, err = s.store.OverwriteTemplateVersionForDevelopmentWithContract(current.ID, item.engineVersion, item.runtimeContract, item.content, item.slots)
			}
		}
		if err != nil {
			return err
		}
		return s.store.SetSetting("builtin_templates_v8", "1")
	}
	templateID, err := s.store.SaveTemplate(store.Template{
		Name: item.name, Engine: item.engine, Scenario: item.scenario,
		Description: item.description, Status: "draft",
	})
	if err != nil {
		return err
	}
	if _, err := s.store.PublishTemplateVersionWithContract(templateID, item.engineVersion, item.runtimeContract, item.content, item.slots); err != nil {
		return err
	}
	return s.store.SetSetting("builtin_templates_v8", "1")
}

func (s *Service) upgradeMihomoBuiltinTemplates() error {
	items := make([]builtinTemplate, 0, 3)
	for _, item := range builtinTemplates() {
		if item.engine == EngineMihomo {
			items = append(items, item)
		}
	}
	for _, item := range items {
		if err := ValidateTemplate(item.engine, item.content, item.slots); err != nil {
			return fmt.Errorf("builtin template %q: %w", item.name, err)
		}
	}
	templates, err := s.store.ListTemplates()
	if err != nil {
		return err
	}
	byKey := make(map[string]store.Template, len(templates))
	for _, template := range templates {
		byKey[template.Engine+"\x00"+template.Name] = template
	}
	changed := false
	for _, item := range items {
		template, ok := byKey[item.engine+"\x00"+item.name]
		if !ok || template.CurrentVersionID == 0 {
			continue
		}
		current, err := s.store.GetTemplateVersion(template.CurrentVersionID)
		if err != nil {
			return err
		}
		if current.Content == item.content && current.EngineVersion == item.engineVersion && current.RuntimeContract == item.runtimeContract && reflect.DeepEqual(current.Slots, item.slots) {
			continue
		}
		if _, err := s.store.OverwriteTemplateVersionForDevelopmentWithContract(current.ID, item.engineVersion, item.runtimeContract, item.content, item.slots); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		// Successful subscriptions receive refreshed artifacts. Failures retain
		// their last-good artifact under the normal rebuild policy.
		_ = s.RebuildAll()
	}
	return nil
}

func builtinTemplates() []builtinTemplate {
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
		runtimeContract: RuntimeContractMihomoAgentV1,
		description:     "仅监听回环地址、由 submux-agent 管理控制 API 的服务器 sidecar 配置。",
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

func mihomoDesktopTUNTemplate() builtinTemplate {
	return builtinTemplate{
		name:          "Mihomo 桌面 TUN（推荐）",
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
    "rule-set:cn_domain":
      - https://doh.pub/dns-query
      - https://dns.alidns.com/dns-query
  nameserver:
    - "https://1.1.1.1/dns-query#PROXY"
    - "https://8.8.8.8/dns-query#PROXY"
proxy-groups:
  - name: PROXY
    type: select
    proxies: []
rule-providers:
  private_domain:
    type: http
    behavior: domain
    format: mrs
    interval: 86400
    proxy: PROXY
    url: "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/private.mrs"
  cn_domain:
    type: http
    behavior: domain
    format: mrs
    interval: 86400
    proxy: PROXY
    url: "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/cn.mrs"
  geolocation_non_cn:
    type: http
    behavior: domain
    format: mrs
    interval: 86400
    proxy: PROXY
    url: "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/geolocation-!cn.mrs"
  private_ip:
    type: http
    behavior: ipcidr
    format: mrs
    interval: 86400
    proxy: PROXY
    url: "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/private.mrs"
  cn_ip:
    type: http
    behavior: ipcidr
    format: mrs
    interval: 86400
    proxy: PROXY
    url: "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/cn.mrs"
rules:
  - RULE-SET,private_domain,DIRECT
  - RULE-SET,private_ip,DIRECT,no-resolve
  - RULE-SET,cn_domain,DIRECT
  - RULE-SET,geolocation_non_cn,PROXY
  - RULE-SET,cn_ip,DIRECT,no-resolve
  - MATCH,PROXY
`,
		slots: []store.TemplateSlot{{Key: "primary", Target: "PROXY", Mode: "replace", Required: true}},
	}
}
