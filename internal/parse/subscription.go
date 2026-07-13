package parse

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// ParseSubscription 接受 Clash YAML、明文分享链接列表或 Base64 分享链接订阅。
// 当前分享链接支持 vless、vmess、trojan、shadowsocks 和 hysteria2。
func ParseSubscription(raw string) ([]Node, error) {
	if nodes, err := ParseClash(raw); err == nil && len(nodes) > 0 {
		return nodes, nil
	}

	text := strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
	if text == "" {
		return nil, fmt.Errorf("empty subscription")
	}
	if !strings.Contains(text, "://") {
		decoded, err := decodeBase64(text)
		if err != nil {
			return nil, fmt.Errorf("unsupported subscription format: %w", err)
		}
		text = string(decoded)
	}

	nodes, firstErr := parseShareLinks(text)
	if firstErr != nil {
		return nil, firstErr
	}
	if len(nodes) > 0 {
		return nodes, nil
	}
	return nil, fmt.Errorf("subscription contains no supported nodes")
}

func parseShareLinks(text string) ([]Node, error) {
	var nodes []Node
	var firstErr error
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		node, supported, err := parseShareLink(line)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if supported {
			nodes = append(nodes, node)
		} else if strings.Contains(line, "://") && firstErr == nil {
			firstErr = fmt.Errorf("unsupported share-link protocol")
		}
	}
	return nodes, firstErr
}

func parseShareLink(link string) (Node, bool, error) {
	switch {
	case strings.HasPrefix(strings.ToLower(link), "vmess://"):
		n, err := parseVMess(link)
		return n, true, err
	case strings.HasPrefix(strings.ToLower(link), "ss://"):
		n, err := parseSS(link)
		return n, true, err
	case strings.HasPrefix(strings.ToLower(link), "vless://"):
		n, err := parseUserURL(link, "vless")
		return n, true, err
	case strings.HasPrefix(strings.ToLower(link), "trojan://"):
		n, err := parseUserURL(link, "trojan")
		return n, true, err
	case strings.HasPrefix(strings.ToLower(link), "hysteria2://"), strings.HasPrefix(strings.ToLower(link), "hy2://"):
		n, err := parseHysteria2(link)
		return n, true, err
	default:
		return nil, false, nil
	}
}

func parseUserURL(link, kind string) (Node, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("parse %s link: %w", kind, err)
	}
	port, err := parsePort(u.Port())
	if err != nil || u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("invalid %s endpoint", kind)
	}
	q := u.Query()
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	name := u.Fragment
	if name == "" {
		name = net.JoinHostPort(u.Hostname(), u.Port())
	}
	n := Node{
		"name": name, "type": kind, "server": u.Hostname(), "port": port, "network": network,
	}
	if kind == "vless" {
		n["uuid"] = u.User.Username()
		n["encryption"] = valueOrDefault(q.Get("encryption"), "none")
	} else {
		n["password"] = u.User.Username()
	}
	security := strings.ToLower(q.Get("security"))
	if kind == "trojan" && security == "" {
		security = "tls"
	}
	if (kind == "vless" && security != "" && security != "none" && security != "tls" && security != "reality") ||
		(kind == "trojan" && security != "tls" && security != "reality") {
		return nil, fmt.Errorf("unsupported %s security %q", kind, security)
	}
	if security == "tls" || security == "reality" {
		n["tls"] = true
	}
	if v := q.Get("sni"); v != "" {
		n["servername"] = v
	}
	if v := q.Get("fp"); v != "" {
		n["client-fingerprint"] = v
	}
	if v := q.Get("flow"); v != "" {
		n["flow"] = v
	}
	if queryBool(q, "insecure", "allowInsecure", "allow_insecure") {
		n["skip-cert-verify"] = true
	}
	if v := q.Get("alpn"); v != "" {
		parts := strings.Split(v, ",")
		values := make([]any, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				values = append(values, part)
			}
		}
		n["alpn"] = values
	}
	if security == "reality" {
		opts := map[string]any{}
		for queryKey, nodeKey := range map[string]string{"pbk": "public-key", "sid": "short-id", "spx": "spider-x", "pqv": "mldsa65-verify"} {
			if v := q.Get(queryKey); v != "" {
				opts[nodeKey] = v
			}
		}
		n["reality-opts"] = opts
	}
	if v := q.Get("vcn"); v != "" {
		n["verify-peer-cert-by-name"] = v
	}
	if v := q.Get("pcs"); v != "" {
		n["certificate-sha256"] = v
	}
	if err := setTransportOptions(n, q, network); err != nil {
		return nil, fmt.Errorf("parse %s transport: %w", kind, err)
	}
	allowed := []string{"security", "type", "sni", "fp", "flow", "insecure", "allowInsecure", "allow_insecure", "alpn", "pbk", "sid", "spx", "pqv", "vcn", "pcs", "path", "host", "serviceName", "authority", "mode", "headerType"}
	if kind == "vless" {
		allowed = append(allowed, "encryption")
	}
	if key := firstUnknownQueryKey(q, allowed...); key != "" {
		return nil, fmt.Errorf("unsupported %s query parameter %q", kind, key)
	}
	return n, nil
}

func parseVMess(link string) (Node, error) {
	payload := strings.TrimSpace(link[len("vmess://"):])
	b, err := decodeBase64(payload)
	if err != nil {
		return nil, fmt.Errorf("decode vmess link: %w", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("decode vmess json: %w", err)
	}
	if key := firstUnknownMapKey(v, "v", "ps", "add", "port", "id", "aid", "scy", "net", "type", "host", "path", "tls", "sni", "alpn", "fp", "insecure", "allowInsecure", "vcn", "pcs"); key != "" {
		return nil, fmt.Errorf("unsupported vmess json field %q", key)
	}
	server, uuid := valueString(v["add"]), valueString(v["id"])
	port, err := parsePort(valueString(v["port"]))
	if err != nil || server == "" || uuid == "" {
		return nil, fmt.Errorf("invalid vmess endpoint")
	}
	network := valueDefault(v["net"], "tcp")
	n := Node{
		"name": valueDefault(v["ps"], net.JoinHostPort(server, strconv.Itoa(port))),
		"type": "vmess", "server": server, "port": port, "uuid": uuid,
		"alterId": valueDefault(v["aid"], "0"), "cipher": valueDefault(v["scy"], "auto"), "network": network,
	}
	if tls := strings.ToLower(valueString(v["tls"])); tls == "tls" {
		n["tls"] = true
	} else if tls != "" && tls != "none" {
		return nil, fmt.Errorf("unsupported vmess security %q", tls)
	}
	if sni := valueString(v["sni"]); sni != "" {
		n["servername"] = sni
	}
	if fp := valueString(v["fp"]); fp != "" {
		n["client-fingerprint"] = fp
	}
	if alpn := valueString(v["alpn"]); alpn != "" {
		values := make([]any, 0)
		for _, item := range strings.Split(alpn, ",") {
			if item = strings.TrimSpace(item); item != "" {
				values = append(values, item)
			}
		}
		n["alpn"] = values
	}
	if isTrueValue(v["insecure"]) || isTrueValue(v["allowInsecure"]) {
		n["skip-cert-verify"] = true
	}
	if value := valueString(v["vcn"]); value != "" {
		n["verify-peer-cert-by-name"] = value
	}
	if value := valueString(v["pcs"]); value != "" {
		n["certificate-sha256"] = value
	}
	q := url.Values{}
	q.Set("path", valueString(v["path"]))
	q.Set("host", valueString(v["host"]))
	q.Set("serviceName", valueString(v["path"]))
	if err := setTransportOptions(n, q, network); err != nil {
		return nil, fmt.Errorf("parse vmess transport: %w", err)
	}
	return n, nil
}

func parseSS(link string) (Node, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("parse ss link: %w", err)
	}
	name := u.Fragment
	if key := firstUnknownQueryKey(u.Query(), "plugin"); key != "" {
		return nil, fmt.Errorf("unsupported ss query parameter %q", key)
	}
	var method, password, host, portText string
	if u.User != nil && u.User.Username() != "" {
		if pass, hasPassword := u.User.Password(); hasPassword {
			method, password = u.User.Username(), pass
		} else {
			credential, err := decodeBase64(u.User.Username())
			if err != nil {
				return nil, fmt.Errorf("decode ss credential: %w", err)
			}
			method, password, err = splitCredential(string(credential))
			if err != nil {
				return nil, err
			}
		}
		host, portText = u.Hostname(), u.Port()
	} else {
		legacy, err := decodeBase64(u.Host)
		if err != nil {
			return nil, fmt.Errorf("decode legacy ss link: %w", err)
		}
		at := strings.LastIndexByte(string(legacy), '@')
		if at <= 0 {
			return nil, fmt.Errorf("invalid legacy ss credential")
		}
		method, password, err = splitCredential(string(legacy[:at]))
		if err != nil {
			return nil, err
		}
		host, portText, err = net.SplitHostPort(string(legacy[at+1:]))
		if err != nil {
			return nil, fmt.Errorf("invalid legacy ss endpoint: %w", err)
		}
	}
	port, err := parsePort(portText)
	if err != nil || host == "" || method == "" || password == "" {
		return nil, fmt.Errorf("invalid ss endpoint")
	}
	if name == "" {
		name = net.JoinHostPort(host, portText)
	}
	n := Node{"name": name, "type": "ss", "server": host, "port": port, "cipher": method, "password": password}
	if plugin := u.Query().Get("plugin"); plugin != "" {
		name, opts, err := parseSSPlugin(plugin)
		if err != nil {
			return nil, err
		}
		n["plugin"] = name
		if len(opts) > 0 {
			n["plugin-opts"] = opts
		}
	}
	return n, nil
}

func parseHysteria2(link string) (Node, error) {
	normalized, authorityPortSpec, err := normalizeHysteria2URL(link)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("parse hysteria2 link: %w", err)
	}
	if u.Hostname() == "" || u.User == nil {
		return nil, fmt.Errorf("invalid hysteria2 endpoint")
	}
	auth, err := url.PathUnescape(u.User.String())
	if err != nil || auth == "" {
		return nil, fmt.Errorf("invalid hysteria2 auth")
	}
	portSpec := authorityPortSpec
	if portSpec == "" {
		portSpec = u.Port()
	}
	if portSpec == "" {
		portSpec = "443"
	}
	port, err := firstHysteriaPort(portSpec)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	name := u.Fragment
	if name == "" {
		name = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	}
	n := Node{"name": name, "type": "hysteria2", "server": u.Hostname(), "port": port, "password": auth, "udp": true}
	if strings.ContainsAny(portSpec, ",-:") {
		n["ports"] = strings.ReplaceAll(portSpec, "-", ":")
	}
	if v := q.Get("mport"); v != "" {
		n["ports"] = strings.ReplaceAll(v, "-", ":")
	}
	for _, key := range []string{"obfs", "obfs-password", "sni"} {
		if v := q.Get(key); v != "" {
			n[key] = v
		}
	}
	if queryBool(q, "insecure", "allowInsecure", "allow_insecure") {
		n["skip-cert-verify"] = true
	}
	if v := q.Get("pinSHA256"); v != "" {
		n["fingerprint"] = v
	}
	if v := q.Get("minPacketSize"); v != "" {
		n["obfs-min-packet-size"] = v
	}
	if v := q.Get("maxPacketSize"); v != "" {
		n["obfs-max-packet-size"] = v
	}
	if key := firstUnknownQueryKey(q, "mport", "obfs", "obfs-password", "sni", "insecure", "allowInsecure", "allow_insecure", "pinSHA256", "minPacketSize", "maxPacketSize"); key != "" {
		return nil, fmt.Errorf("unsupported hysteria2 query parameter %q", key)
	}
	return n, nil
}

// Hysteria2 官方允许 authority 中使用 80,443,2000-3000 形式的多端口，
// 这超出了 net/url 对端口的语法限制；先用首端口规范化，再保留完整端口集。
func normalizeHysteria2URL(link string) (string, string, error) {
	schemeEnd := strings.Index(link, "://")
	if schemeEnd < 0 {
		return "", "", fmt.Errorf("invalid hysteria2 URI")
	}
	authorityStart := schemeEnd + 3
	rest := link[authorityStart:]
	authorityLen := len(rest)
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authorityLen = i
	}
	authority := rest[:authorityLen]
	hostPort := authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		hostPort = authority[at+1:]
	}
	colon := strings.LastIndexByte(hostPort, ':')
	if colon < 0 || (strings.HasPrefix(hostPort, "[") && colon < strings.LastIndexByte(hostPort, ']')) {
		return link, "", nil
	}
	portSpec := hostPort[colon+1:]
	if !strings.ContainsAny(portSpec, ",-:") {
		return link, "", nil
	}
	port, err := firstHysteriaPort(portSpec)
	if err != nil {
		return "", "", err
	}
	hostPortOffset := len(authority) - len(hostPort)
	portStart := authorityStart + hostPortOffset + colon + 1
	portEnd := authorityStart + authorityLen
	normalized := link[:portStart] + strconv.Itoa(port) + link[portEnd:]
	return normalized, portSpec, nil
}

func firstHysteriaPort(spec string) (int, error) {
	first := strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == '-' || r == ':' })
	if len(first) == 0 {
		return 0, fmt.Errorf("invalid hysteria2 port %q", spec)
	}
	port, err := parsePort(first[0])
	if err != nil {
		return 0, fmt.Errorf("invalid hysteria2 port %q", spec)
	}
	return port, nil
}

func setTransportOptions(n Node, q url.Values, network string) error {
	switch network {
	case "tcp", "raw":
		if header := q.Get("headerType"); header != "" && header != "none" {
			return fmt.Errorf("unsupported tcp header type %q", header)
		}
		if q.Get("host") != "" || q.Get("path") != "" {
			return fmt.Errorf("tcp host/path header options are not representable")
		}
		return nil
	case "ws":
		opts := map[string]any{}
		if path := q.Get("path"); path != "" {
			opts["path"] = path
		}
		if host := q.Get("host"); host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		if len(opts) > 0 {
			n["ws-opts"] = opts
		}
		return nil
	case "grpc":
		opts := map[string]any{}
		if service := q.Get("serviceName"); service != "" {
			opts["grpc-service-name"] = service
		}
		if authority := q.Get("authority"); authority != "" {
			opts["grpc-authority"] = authority
		}
		if mode := q.Get("mode"); mode != "" {
			opts["grpc-mode"] = mode
		}
		if len(opts) > 0 {
			n["grpc-opts"] = opts
		}
		return nil
	default:
		return fmt.Errorf("unsupported transport %q", network)
	}
}

func parseSSPlugin(raw string) (string, map[string]any, error) {
	parts, err := splitEscaped(raw, ';')
	if err != nil || len(parts) == 0 || parts[0] == "" {
		return "", nil, fmt.Errorf("invalid shadowsocks plugin")
	}
	plugin := parts[0]
	opts := map[string]any{}
	switch plugin {
	case "obfs-local":
		plugin = "obfs"
		for _, part := range parts[1:] {
			key, value, ok := strings.Cut(part, "=")
			if !ok {
				return "", nil, fmt.Errorf("invalid shadowsocks obfs option")
			}
			switch key {
			case "obfs":
				opts["mode"] = value
			case "obfs-host":
				opts["host"] = value
			default:
				return "", nil, fmt.Errorf("unsupported shadowsocks obfs option %q", key)
			}
		}
	case "v2ray-plugin":
		for _, part := range parts[1:] {
			if part == "tls" || part == "mux" {
				opts[part] = true
				continue
			}
			key, value, ok := strings.Cut(part, "=")
			if !ok || (key != "mode" && key != "host" && key != "path") {
				return "", nil, fmt.Errorf("unsupported v2ray-plugin option %q", part)
			}
			opts[key] = value
		}
	default:
		return "", nil, fmt.Errorf("unsupported shadowsocks plugin %q", plugin)
	}
	return plugin, opts, nil
}

func splitEscaped(raw string, separator rune) ([]string, error) {
	var parts []string
	var b strings.Builder
	escaped := false
	for _, r := range raw {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == separator {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		return nil, fmt.Errorf("unterminated escape")
	}
	return append(parts, b.String()), nil
}

func queryBool(q url.Values, keys ...string) bool {
	for _, key := range keys {
		switch strings.ToLower(q.Get(key)) {
		case "1", "true", "yes":
			return true
		}
	}
	return false
}

func isTrueValue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	case float64:
		return t == 1
	default:
		return false
	}
}

func valueOrDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func firstUnknownQueryKey(values url.Values, allowed ...string) string {
	known := map[string]bool{}
	for _, key := range allowed {
		known[key] = true
	}
	for key := range values {
		if !known[key] {
			return key
		}
	}
	return ""
}

func firstUnknownMapKey(values map[string]any, allowed ...string) string {
	known := map[string]bool{}
	for _, key := range allowed {
		known[key] = true
	}
	for key := range values {
		if !known[key] {
			return key
		}
	}
	return ""
}

func splitCredential(v string) (string, string, error) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid ss credential")
	}
	return parts[0], parts[1], nil
}

func parsePort(v string) (int, error) {
	port, err := strconv.Atoi(v)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q", v)
	}
	return port, nil
}

func decodeBase64(v string) ([]byte, error) {
	v = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, v)
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}
	var lastErr error
	for _, enc := range encodings {
		if b, err := enc.DecodeString(v); err == nil {
			return b, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

func valueDefault(v any, def string) string {
	if s := valueString(v); s != "" {
		return s
	}
	return def
}

func valueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}
