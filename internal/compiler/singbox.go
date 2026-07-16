package compiler

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"submux/internal/node"
	"submux/internal/parse"
	"submux/internal/store"
)

func validateSingBoxTemplate(content string, slots []store.TemplateSlot) error {
	root, err := parseJSONMap(content)
	if err != nil {
		return fmt.Errorf("parse sing-box template: %w", err)
	}
	outbounds, err := objectList(root["outbounds"], "outbounds")
	if err != nil {
		return err
	}
	byTag := map[string]map[string]any{}
	for _, outbound := range outbounds {
		tag := stringValue(outbound["tag"])
		if tag == "" {
			return fmt.Errorf("sing-box outbound is missing tag")
		}
		if byTag[tag] != nil {
			return fmt.Errorf("duplicate sing-box outbound tag %q", tag)
		}
		byTag[tag] = outbound
	}
	for _, slot := range slots {
		target := byTag[slot.Target]
		if target == nil {
			return fmt.Errorf("sing-box slot %q targets missing outbound %q", slot.Key, slot.Target)
		}
		kind := stringValue(target["type"])
		if kind != "selector" && kind != "urltest" {
			return fmt.Errorf("sing-box slot %q target must be selector or urltest", slot.Key)
		}
	}
	return nil
}

func compileSingBox(value resolvedSubscription) ([]byte, error) {
	root, err := parseJSONMap(value.Template.Content)
	if err != nil {
		return nil, err
	}
	outbounds, _ := objectList(root["outbounds"], "outbounds")
	byTag, tags := map[string]map[string]any{}, map[string]bool{}
	for _, outbound := range outbounds {
		tag := stringValue(outbound["tag"])
		byTag[tag], tags[tag] = outbound, true
	}
	for _, record := range value.Records {
		tag := value.Names[record.Fingerprint]
		if tags[tag] {
			return nil, fmt.Errorf("generated outbound tag conflicts with template tag %q", tag)
		}
		parsed, err := node.Decode(record)
		if err != nil {
			return nil, err
		}
		outbound, err := toSingBoxOutbound(parsed, tag)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", tag, err)
		}
		outbounds, tags[tag] = append(outbounds, outbound), true
	}
	for _, slot := range value.Template.Slots {
		target := byTag[slot.Target]
		current, err := optionalStringList(target["outbounds"], "outbounds")
		if err != nil {
			return nil, fmt.Errorf("outbound %q: %w", slot.Target, err)
		}
		names := value.Slots[slot.Key]
		if slot.Required && len(names) == 0 {
			return nil, fmt.Errorf("required slot %q is empty", slot.Key)
		}
		if slot.Mode == "replace" {
			current = nil
		}
		target["outbounds"] = uniqueStrings(append(current, names...))
	}
	root["outbounds"] = mapsToAny(outbounds)
	if err := validateSingBoxReferences(root, tags); err != nil {
		return nil, err
	}
	return json.MarshalIndent(root, "", "  ")
}

func toSingBoxOutbound(n parse.Node, tag string) (map[string]any, error) {
	kind := strings.ToLower(stringValue(n["type"]))
	allowed := []string{
		"name", "type", "server", "port", "udp",
		"interface-name", "routing-mark", "tfo", "mptcp", "ip-version",
	}
	switch kind {
	case "vless":
		allowed = append(allowed, "uuid", "flow", "encryption", "packet-encoding", "network", "tls", "servername", "sni", "skip-cert-verify", "alpn", "client-fingerprint", "reality-opts", "ws-opts", "grpc-opts")
	case "vmess":
		allowed = append(allowed, "uuid", "cipher", "alterId", "packet-encoding", "network", "tls", "servername", "sni", "skip-cert-verify", "alpn", "client-fingerprint", "ws-opts", "grpc-opts")
	case "trojan":
		allowed = append(allowed, "password", "network", "tls", "servername", "sni", "skip-cert-verify", "alpn", "client-fingerprint", "reality-opts", "ws-opts", "grpc-opts")
	case "ss":
		allowed = append(allowed, "cipher", "password", "plugin", "plugin-opts")
	case "hysteria2", "hy2":
		allowed = append(allowed, "password", "ports", "mport", "hop-interval", "up", "down", "obfs", "obfs-password", "obfs-min-packet-size", "obfs-max-packet-size", "network", "tls", "servername", "sni", "skip-cert-verify", "alpn")
	default:
		return nil, fmt.Errorf("protocol %q is not supported by sing-box compiler", kind)
	}
	if err := rejectUnknownNodeFields(n, allowed); err != nil {
		return nil, err
	}
	if err := validateTLSOptions(n); err != nil {
		return nil, err
	}
	server, port := stringValue(n["server"]), intValue(n["port"])
	if server == "" || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid endpoint")
	}
	out := map[string]any{"tag": tag, "server": server, "server_port": port}
	if raw, exists := n["udp"]; exists {
		if _, ok := raw.(bool); !ok {
			return nil, fmt.Errorf("udp must be a boolean")
		}
	}
	if strategy := stringValue(n["ip-version"]); strategy != "" && strategy != "dual" {
		return nil, fmt.Errorf("ip-version %q requires template-level DNS resolver policy", strategy)
	}
	copyStringSnake(out, "bind_interface", n["interface-name"])
	if mark, exists := n["routing-mark"]; exists {
		out["routing_mark"] = mark
	}
	copyBoolSnake(out, "tcp_fast_open", n["tfo"])
	copyBoolSnake(out, "tcp_multi_path", n["mptcp"])
	// Mihomo TCP proxy nodes default to UDP disabled, while sing-box enables
	// both networks when omitted. Preserve the source policy explicitly.
	if kind != "hysteria2" && kind != "hy2" {
		udp, _ := n["udp"].(bool)
		if !udp {
			out["network"] = "tcp"
		}
	}
	switch kind {
	case "vless":
		if encryption := stringValue(n["encryption"]); encryption != "" && encryption != "none" {
			return nil, fmt.Errorf("VLESS encryption %q is not supported by sing-box outbound schema", encryption)
		}
		out["type"], out["uuid"] = "vless", stringValue(n["uuid"])
		if out["uuid"] == "" {
			return nil, fmt.Errorf("missing VLESS uuid")
		}
		if flow := stringValue(n["flow"]); flow != "" {
			if flow != "xtls-rprx-vision" {
				return nil, fmt.Errorf("unsupported VLESS flow %q", flow)
			}
			out["flow"] = flow
		}
		if err := copyPacketEncoding(out, n["packet-encoding"]); err != nil {
			return nil, err
		}
	case "vmess":
		out["type"], out["uuid"] = "vmess", stringValue(n["uuid"])
		if out["uuid"] == "" {
			return nil, fmt.Errorf("missing VMess uuid")
		}
		security := defaultString(n["cipher"], "auto")
		if !stringIn(security, "auto", "none", "zero", "aes-128-gcm", "chacha20-poly1305", "aes-128-ctr") {
			return nil, fmt.Errorf("unsupported VMess security %q", security)
		}
		out["security"], out["alter_id"] = security, intValue(n["alterId"])
		if err := copyPacketEncoding(out, n["packet-encoding"]); err != nil {
			return nil, err
		}
	case "trojan":
		out["type"], out["password"] = "trojan", stringValue(n["password"])
		if out["password"] == "" {
			return nil, fmt.Errorf("missing Trojan password")
		}
	case "ss":
		out["type"], out["method"], out["password"] = "shadowsocks", stringValue(n["cipher"]), stringValue(n["password"])
		if out["method"] == "" || out["password"] == "" {
			return nil, fmt.Errorf("missing Shadowsocks method or password")
		}
		if !stringIn(stringValue(out["method"]), "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305", "none", "aes-128-gcm", "aes-192-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "xchacha20-ietf-poly1305", "aes-128-ctr", "aes-192-ctr", "aes-256-ctr", "aes-128-cfb", "aes-192-cfb", "aes-256-cfb", "rc4-md5", "chacha20-ietf", "xchacha20") {
			return nil, fmt.Errorf("unsupported Shadowsocks method %q", out["method"])
		}
		plugin, opts, err := shadowsocksPlugin(n)
		if err != nil {
			return nil, err
		}
		if plugin != "" {
			out["plugin"], out["plugin_opts"] = plugin, opts
		}
		return out, nil
	case "hysteria2", "hy2":
		out["type"], out["password"] = "hysteria2", stringValue(n["password"])
		if out["password"] == "" {
			return nil, fmt.Errorf("missing Hysteria2 password")
		}
		if ports := stringValue(n["ports"]); ports != "" {
			out["server_ports"] = splitPorts(ports)
			delete(out, "server_port")
		} else if ports := stringValue(n["mport"]); ports != "" {
			out["server_ports"] = splitPorts(ports)
			delete(out, "server_port")
		}
		copyStringSnake(out, "hop_interval", n["hop-interval"])
		if up := bandwidthMbps(n["up"]); up > 0 {
			out["up_mbps"] = up
		}
		if down := bandwidthMbps(n["down"]); down > 0 {
			out["down_mbps"] = down
		}
		if obfsType := stringValue(n["obfs"]); obfsType != "" {
			if !stringIn(obfsType, "salamander", "gecko") {
				return nil, fmt.Errorf("unsupported Hysteria2 obfs %q", obfsType)
			}
			obfs := map[string]any{"type": obfsType}
			copyStringSnake(obfs, "password", n["obfs-password"])
			if min := intValue(n["obfs-min-packet-size"]); min > 0 {
				if obfsType != "gecko" {
					return nil, fmt.Errorf("Hysteria2 obfs packet size is only valid for gecko")
				}
				obfs["min_packet_size"] = min
			}
			if max := intValue(n["obfs-max-packet-size"]); max > 0 {
				if obfsType != "gecko" {
					return nil, fmt.Errorf("Hysteria2 obfs packet size is only valid for gecko")
				}
				obfs["max_packet_size"] = max
			}
			out["obfs"] = obfs
		}
		if network := stringValue(n["network"]); network != "" {
			if network != "tcp" && network != "udp" {
				return nil, fmt.Errorf("unsupported Hysteria2 network %q", network)
			}
			out["network"] = network
		}
		out["tls"] = singBoxTLS(n, true)
		return out, nil
	}
	tlsRequired := kind == "trojan"
	if tls := singBoxTLS(n, tlsRequired); tls != nil {
		out["tls"] = tls
	}
	if transport, err := singBoxTransport(n); err != nil {
		return nil, err
	} else if transport != nil {
		out["transport"] = transport
	}
	return out, nil
}

func singBoxTLS(n parse.Node, required bool) map[string]any {
	enabled, _ := n["tls"].(bool)
	_, reality := n["reality-opts"]
	if !enabled && !required && !reality {
		return nil
	}
	tls := map[string]any{"enabled": true}
	if name := firstString(n, "servername", "sni"); name != "" {
		tls["server_name"] = name
	}
	if insecure, _ := n["skip-cert-verify"].(bool); insecure {
		tls["insecure"] = true
	}
	if alpn := stringSlice(n["alpn"]); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if fp := stringValue(n["client-fingerprint"]); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if opts, ok := n["reality-opts"].(map[string]any); ok {
		realityValue := map[string]any{"enabled": true}
		copyStringSnake(realityValue, "public_key", opts["public-key"])
		copyStringSnake(realityValue, "short_id", opts["short-id"])
		tls["reality"] = realityValue
	}
	return tls
}

func validateTLSOptions(n parse.Node) error {
	raw, exists := n["reality-opts"]
	if !exists {
		return nil
	}
	opts, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("reality-opts must be an object")
	}
	if err := rejectUnknownMapFields(opts, "public-key", "short-id"); err != nil {
		return fmt.Errorf("reality-opts: %w", err)
	}
	if stringValue(opts["public-key"]) == "" {
		return fmt.Errorf("reality-opts.public-key is required")
	}
	return nil
}

func singBoxTransport(n parse.Node) (map[string]any, error) {
	network := strings.ToLower(defaultString(n["network"], "tcp"))
	switch network {
	case "tcp", "raw", "":
		return nil, nil
	case "ws":
		opts, _ := n["ws-opts"].(map[string]any)
		if err := rejectUnknownMapFields(opts, "path", "headers", "max-early-data", "early-data-header-name"); err != nil {
			return nil, fmt.Errorf("ws-opts: %w", err)
		}
		out := map[string]any{"type": "ws"}
		copyStringSnake(out, "path", opts["path"])
		if headers, ok := opts["headers"].(map[string]any); ok && len(headers) > 0 {
			out["headers"] = headers
		}
		if value := intValue(opts["max-early-data"]); value > 0 {
			out["max_early_data"] = value
		}
		copyStringSnake(out, "early_data_header_name", opts["early-data-header-name"])
		return out, nil
	case "grpc":
		opts, _ := n["grpc-opts"].(map[string]any)
		if err := rejectUnknownMapFields(opts, "grpc-service-name"); err != nil {
			return nil, fmt.Errorf("grpc-opts: %w", err)
		}
		out := map[string]any{"type": "grpc"}
		copyStringSnake(out, "service_name", opts["grpc-service-name"])
		return out, nil
	default:
		return nil, fmt.Errorf("transport %q is not supported by sing-box compiler", network)
	}
}

func shadowsocksPlugin(n parse.Node) (string, string, error) {
	plugin := stringValue(n["plugin"])
	opts, _ := n["plugin-opts"].(map[string]any)
	switch plugin {
	case "":
		return "", "", nil
	case "obfs":
		parts := []string{}
		if mode := stringValue(opts["mode"]); mode != "" {
			parts = append(parts, "obfs="+mode)
		}
		if host := stringValue(opts["host"]); host != "" {
			parts = append(parts, "obfs-host="+host)
		}
		return "obfs-local", strings.Join(parts, ";"), nil
	case "v2ray-plugin":
		parts := []string{}
		for _, key := range []string{"mode", "host", "path"} {
			if value := stringValue(opts[key]); value != "" {
				parts = append(parts, key+"="+value)
			}
		}
		for _, key := range []string{"tls", "mux"} {
			if value, _ := opts[key].(bool); value {
				parts = append(parts, key)
			}
		}
		return plugin, strings.Join(parts, ";"), nil
	default:
		return "", "", fmt.Errorf("Shadowsocks plugin %q is not supported by sing-box", plugin)
	}
}

func validateSingBoxReferences(root map[string]any, tags map[string]bool) error {
	outbounds, _ := objectList(root["outbounds"], "outbounds")
	for _, outbound := range outbounds {
		kind, tag := stringValue(outbound["type"]), stringValue(outbound["tag"])
		if kind == "selector" || kind == "urltest" {
			refs, err := optionalStringList(outbound["outbounds"], "outbounds")
			if err != nil {
				return fmt.Errorf("outbound %q: %w", tag, err)
			}
			for _, ref := range refs {
				if !tags[ref] {
					return fmt.Errorf("outbound %q references unknown outbound %q", tag, ref)
				}
			}
		}
	}
	if route, ok := root["route"].(map[string]any); ok {
		if final := stringValue(route["final"]); final != "" && !tags[final] {
			return fmt.Errorf("route final references unknown outbound %q", final)
		}
		if rules, ok := route["rules"].([]any); ok {
			for _, raw := range rules {
				if rule, ok := raw.(map[string]any); ok {
					if ref := stringValue(rule["outbound"]); ref != "" && !tags[ref] {
						return fmt.Errorf("route rule references unknown outbound %q", ref)
					}
				}
			}
		}
	}
	return nil
}

func parseJSONMap(content string) (map[string]any, error) {
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("template root must be an object")
	}
	return root, nil
}
func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}
func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		result, _ := strconv.Atoi(typed.String())
		return result
	case string:
		result, _ := strconv.Atoi(typed)
		return result
	default:
		return 0
	}
}
func defaultString(value any, fallback string) string {
	if result := stringValue(value); result != "" {
		return result
	}
	return fallback
}
func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result := stringValue(value[key]); result != "" {
			return result
		}
	}
	return ""
}
func copyString(target map[string]any, key string, value any) {
	if result := stringValue(value); result != "" {
		target[key] = result
	}
}
func copyStringSnake(target map[string]any, key string, value any) { copyString(target, key, value) }
func copyBoolSnake(target map[string]any, key string, value any) {
	if result, ok := value.(bool); ok && result {
		target[key] = true
	}
}
func copyPacketEncoding(target map[string]any, value any) error {
	encoding := stringValue(value)
	if encoding == "" {
		return nil
	}
	if encoding != "packetaddr" && encoding != "xudp" {
		return fmt.Errorf("unsupported packet encoding %q", encoding)
	}
	target["packet_encoding"] = encoding
	return nil
}
func stringIn(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if result := stringValue(item); result != "" {
				out = append(out, result)
			}
		}
		return out
	case string:
		if typed == "" {
			return nil
		}
		return strings.Split(typed, ",")
	default:
		return nil
	}
}
func splitPorts(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == ',' })
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(strings.ReplaceAll(field, "-", ":"))
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}
func bandwidthMbps(value any) int {
	text := stringValue(value)
	if text == "" {
		return intValue(value)
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0
	}
	parsed, _ := strconv.ParseFloat(fields[0], 64)
	return int(parsed)
}

func rejectUnknownNodeFields(node parse.Node, allowed []string) error {
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	var unknown []string
	for key := range node {
		if !set[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("fields not representable in sing-box schema: %s", strings.Join(unknown, ", "))
}

func rejectUnknownMapFields(value map[string]any, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	var unknown []string
	for key := range value {
		if !set[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unsupported fields: %s", strings.Join(unknown, ", "))
}
