package compiler

import (
	"fmt"

	"submux/internal/store"
)

type builtinTemplate struct {
	name, engine, scenario, description, engineVersion, content string
	slots                                                       []store.TemplateSlot
}

// EnsureBuiltinTemplates installs a versioned starter catalog once. The
// templates remain ordinary records so an administrator can publish a newer
// immutable version without changing existing profiles.
func (s *Service) EnsureBuiltinTemplates() error {
	seeded, _ := s.store.GetSetting("builtin_templates_v2")
	if seeded == "1" {
		return nil
	}
	for _, item := range builtinTemplates() {
		if err := ValidateTemplate(item.engine, item.content, item.slots); err != nil {
			return fmt.Errorf("builtin template %q: %w", item.name, err)
		}
		id, err := s.store.SaveTemplate(store.Template{
			Name: item.name, Engine: item.engine, Scenario: item.scenario,
			Description: item.description, Status: "draft",
		})
		if err != nil {
			return err
		}
		if _, err := s.store.PublishTemplateVersion(id, item.engineVersion, item.content, item.slots); err != nil {
			return err
		}
	}
	return s.store.SetSetting("builtin_templates_v2", "1")
}

func builtinTemplates() []builtinTemplate {
	slot := []store.TemplateSlot{{Key: "primary", Target: "AUTO", Mode: "replace", Required: true}}
	return []builtinTemplate{
		{
			name: "Mihomo 桌面", engine: EngineMihomo, scenario: "desktop", engineVersion: "mihomo current",
			description: "本机 mixed-port、规则模式与自动测速，适合桌面客户端。",
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
  - name: AUTO
    type: url-test
    proxies: []
    url: https://www.gstatic.com/generate_204
    interval: 300
    tolerance: 50
  - name: PROXY
    type: select
    proxies:
      - AUTO
      - DIRECT
rules:
  - GEOIP,LAN,DIRECT,no-resolve
  - MATCH,PROXY
`,
			slots: slot,
		},
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
  - name: AUTO
    type: url-test
    proxies: []
    url: https://www.gstatic.com/generate_204
    interval: 300
    tolerance: 50
  - name: PROXY
    type: select
    proxies:
      - AUTO
      - DIRECT
rules:
  - GEOIP,LAN,DIRECT,no-resolve
  - MATCH,PROXY
`,
			slots: slot,
		},
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
