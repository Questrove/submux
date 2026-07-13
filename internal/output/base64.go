package output

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Base64Adapter 把节点转成分享 URI 列表再 base64(通用订阅格式)。
type Base64Adapter struct{}

func (Base64Adapter) Format() string { return "base64" }

func (Base64Adapter) Render(cfg map[string]any) ([]byte, string, error) {
	proxies, _ := cfg["proxies"].([]any)
	var lines []string
	failures := map[string]int{}
	for _, p := range proxies {
		m, ok := p.(map[string]any)
		if !ok {
			failures["invalid"]++
			continue
		}
		uri, err := nodeToURI(m)
		if err != nil {
			kind := strings.ToLower(str(m["type"]))
			if kind == "" {
				kind = "unknown"
			}
			failures[kind]++
			continue
		}
		lines = append(lines, uri)
	}
	if len(failures) > 0 {
		return nil, "", fmt.Errorf("base64 output is not lossless: %s", formatFailures(failures))
	}
	if len(lines) == 0 {
		return nil, "", fmt.Errorf("base64 output contains no nodes")
	}
	enc := base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))
	return []byte(enc), "text/plain; charset=utf-8", nil
}

// nodeToURI 把 Clash 节点转换为通用分享链接。
func nodeToURI(n map[string]any) (string, error) {
	if !validEndpoint(n) {
		return "", fmt.Errorf("invalid endpoint")
	}
	switch strings.ToLower(str(n["type"])) {
	case "vless":
		if str(n["uuid"]) == "" {
			return "", fmt.Errorf("missing uuid")
		}
		return vlessURI(n)
	case "trojan":
		if str(n["password"]) == "" {
			return "", fmt.Errorf("missing password")
		}
		return trojanURI(n)
	case "ss":
		if str(n["cipher"]) == "" || str(n["password"]) == "" {
			return "", fmt.Errorf("missing cipher or password")
		}
		return ssURI(n)
	case "vmess":
		if str(n["uuid"]) == "" {
			return "", fmt.Errorf("missing uuid")
		}
		return vmessURI(n)
	case "hysteria2", "hy2":
		if str(n["password"]) == "" {
			return "", fmt.Errorf("missing password")
		}
		return hysteria2URI(n)
	}
	return "", fmt.Errorf("unsupported protocol")
}

func formatFailures(failures map[string]int) string {
	kinds := make([]string, 0, len(failures))
	for kind := range failures {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", kind, failures[kind]))
	}
	return strings.Join(parts, ",")
}

func vlessURI(n map[string]any) (string, error) {
	q, err := transportQuery(n)
	if err != nil {
		return "", err
	}
	q.Set("encryption", strDefault(n["encryption"], "none"))
	u := url.URL{
		Scheme:   "vless",
		User:     url.User(str(n["uuid"])),
		Host:     endpoint(n),
		RawQuery: q.Encode(),
		Fragment: str(n["name"]),
	}
	return u.String(), nil
}

func trojanURI(n map[string]any) (string, error) {
	q, err := transportQuery(n)
	if err != nil {
		return "", err
	}
	if q.Get("security") == "none" {
		q.Set("security", "tls")
	}
	u := url.URL{
		Scheme:   "trojan",
		User:     url.User(str(n["password"])),
		Host:     endpoint(n),
		RawQuery: q.Encode(),
		Fragment: str(n["name"]),
	}
	return u.String(), nil
}

func ssURI(n map[string]any) (string, error) {
	cipher, password := str(n["cipher"]), str(n["password"])
	plugin, err := ssPlugin(n)
	if err != nil {
		return "", err
	}
	u := url.URL{
		Scheme:   "ss",
		Host:     endpoint(n),
		Fragment: str(n["name"]),
	}
	if strings.HasPrefix(strings.ToLower(cipher), "2022-") {
		// SIP002/SIP022: AEAD-2022 userinfo must remain percent-encoded plaintext.
		u.User = url.UserPassword(cipher, password)
	} else {
		credential := base64.RawURLEncoding.EncodeToString([]byte(cipher + ":" + password))
		u.User = url.User(credential)
	}
	if plugin != "" {
		u.Path = "/"
		q := url.Values{}
		q.Set("plugin", plugin)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func vmessURI(n map[string]any) (string, error) {
	if v := str(n["packet-encoding"]); v != "" {
		return "", fmt.Errorf("vmess packet-encoding is not representable in vmess QR JSON")
	}
	network, host, path, headerType, err := vmessTransport(n)
	if err != nil {
		return "", err
	}
	if _, reality := n["reality-opts"]; reality {
		return "", fmt.Errorf("vmess reality fields are not representable in vmess QR JSON")
	}
	tls := ""
	if enabled, _ := n["tls"].(bool); enabled {
		tls = "tls"
	}
	payload := map[string]string{
		"v": "2", "ps": str(n["name"]), "add": str(n["server"]), "port": str(n["port"]),
		"id": str(n["uuid"]), "aid": strDefault(n["alterId"], "0"), "scy": strDefault(n["cipher"], "auto"),
		"net": network, "type": headerType, "host": host, "path": path, "tls": tls,
		"sni": str(n["servername"]), "alpn": stringList(n["alpn"]), "fp": str(n["client-fingerprint"]),
	}
	if insecure, _ := n["skip-cert-verify"].(bool); insecure {
		payload["insecure"] = "1"
	}
	if v := str(n["verify-peer-cert-by-name"]); v != "" {
		payload["vcn"] = v
	}
	if v := str(n["certificate-sha256"]); v != "" {
		payload["pcs"] = v
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(b), nil
}

func hysteria2URI(n map[string]any) (string, error) {
	q := url.Values{}
	if v := str(n["obfs"]); v != "" {
		q.Set("obfs", v)
	}
	if v := str(n["obfs-password"]); v != "" {
		if q.Get("obfs") == "" {
			return "", fmt.Errorf("hysteria2 obfs-password requires obfs")
		}
		q.Set("obfs-password", v)
	}
	if v := str(n["sni"]); v != "" {
		q.Set("sni", v)
	} else if v := str(n["servername"]); v != "" {
		q.Set("sni", v)
	}
	if insecure, _ := n["skip-cert-verify"].(bool); insecure {
		q.Set("insecure", "1")
	}
	if v := str(n["fingerprint"]); v != "" {
		q.Set("pinSHA256", v)
	}
	if v := str(n["obfs-min-packet-size"]); v != "" {
		q.Set("minPacketSize", v)
	}
	if v := str(n["obfs-max-packet-size"]); v != "" {
		q.Set("maxPacketSize", v)
	}
	ports := str(n["ports"])
	if ports == "" {
		ports = str(n["mport"])
	}
	if ports != "" {
		// v2rayN uses mport as a compatibility extension to the official URI.
		q.Set("mport", strings.ReplaceAll(ports, ":", "-"))
	}
	if v := str(n["hop-interval"]); v != "" && ports != "" {
		return "", fmt.Errorf("hysteria2 hop-interval is not representable in the share URI")
	}
	if v := stringList(n["alpn"]); v != "" {
		return "", fmt.Errorf("hysteria2 alpn is not representable in the share URI")
	}
	u := url.URL{
		Scheme:   "hysteria2",
		User:     url.User(str(n["password"])),
		Host:     endpoint(n),
		Path:     "/",
		RawQuery: q.Encode(),
		Fragment: str(n["name"]),
	}
	return u.String(), nil
}

func ssPlugin(n map[string]any) (string, error) {
	plugin := strings.TrimSpace(str(n["plugin"]))
	if plugin == "" {
		return "", nil
	}
	opts, _ := n["plugin-opts"].(map[string]any)
	parts := []string{}
	switch plugin {
	case "obfs":
		if key := firstUnknownKey(opts, "mode", "host"); key != "" {
			return "", fmt.Errorf("unsupported shadowsocks obfs option %q", key)
		}
		plugin = "obfs-local"
		if mode := str(opts["mode"]); mode != "" {
			parts = append(parts, "obfs="+escapeSSPluginValue(mode))
		}
		if host := str(opts["host"]); host != "" {
			parts = append(parts, "obfs-host="+escapeSSPluginValue(host))
		}
	case "v2ray-plugin":
		if key := firstUnknownKey(opts, "mode", "host", "path", "tls", "mux"); key != "" {
			return "", fmt.Errorf("unsupported v2ray-plugin option %q", key)
		}
		for _, key := range []string{"mode", "host", "path"} {
			if v := str(opts[key]); v != "" {
				parts = append(parts, key+"="+escapeSSPluginValue(v))
			}
		}
		for _, key := range []string{"tls", "mux"} {
			if enabled, ok := opts[key].(bool); ok && enabled {
				parts = append(parts, key)
			}
		}
	default:
		return "", fmt.Errorf("unsupported shadowsocks plugin %q", plugin)
	}
	if len(parts) == 0 {
		return plugin, nil
	}
	return plugin + ";" + strings.Join(parts, ";"), nil
}

func escapeSSPluginValue(v string) string {
	r := strings.NewReplacer("\\", "\\\\", ";", "\\;", "=", "\\=", ":", "\\:")
	return r.Replace(v)
}

func vmessTransport(n map[string]any) (network, host, path, headerType string, err error) {
	network = strings.ToLower(strDefault(n["network"], "tcp"))
	headerType = "none"
	switch network {
	case "tcp", "raw":
		network = "tcp"
		if _, exists := n["http-opts"]; exists {
			return "", "", "", "", fmt.Errorf("vmess tcp http header options are not representable")
		}
	case "ws":
		opts, _ := n["ws-opts"].(map[string]any)
		if key := firstUnknownKey(opts, "path", "headers"); key != "" {
			return "", "", "", "", fmt.Errorf("unsupported vmess websocket option %q", key)
		}
		path = str(opts["path"])
		if headers, ok := opts["headers"].(map[string]any); ok {
			if key := firstUnknownKeyFold(headers, "host"); key != "" {
				return "", "", "", "", fmt.Errorf("unsupported vmess websocket header %q", key)
			}
			host = str(headers["Host"])
			if host == "" {
				host = str(headers["host"])
			}
		}
	case "grpc":
		opts, _ := n["grpc-opts"].(map[string]any)
		if key := firstUnknownKey(opts, "grpc-service-name", "grpc-authority", "grpc-mode"); key != "" {
			return "", "", "", "", fmt.Errorf("unsupported vmess grpc option %q", key)
		}
		path = str(opts["grpc-service-name"])
		host = str(opts["grpc-authority"])
		if mode := str(opts["grpc-mode"]); mode != "" {
			headerType = mode
		}
	default:
		return "", "", "", "", fmt.Errorf("unsupported vmess transport %q", network)
	}
	return network, host, path, headerType, nil
}

func transportQuery(n map[string]any) (url.Values, error) {
	if v := str(n["packet-encoding"]); v != "" {
		return nil, fmt.Errorf("packet-encoding is not representable in the share URI")
	}
	q := url.Values{}
	network := strings.ToLower(strDefault(n["network"], "tcp"))
	if network == "raw" {
		network = "tcp"
	}
	if network != "tcp" && network != "ws" && network != "grpc" {
		return nil, fmt.Errorf("unsupported transport %q", network)
	}
	q.Set("type", network)
	security := "none"
	if ro, ok := n["reality-opts"].(map[string]any); ok {
		security = "reality"
		if key := firstUnknownKey(ro, "public-key", "short-id", "spider-x", "mldsa65-verify"); key != "" {
			return nil, fmt.Errorf("unsupported reality option %q", key)
		}
		if v := str(ro["public-key"]); v != "" {
			q.Set("pbk", v)
		}
		if v := str(ro["short-id"]); v != "" {
			q.Set("sid", v)
		}
		if v := str(ro["spider-x"]); v != "" {
			q.Set("spx", v)
		}
		if v := str(ro["mldsa65-verify"]); v != "" {
			q.Set("pqv", v)
		}
	} else if enabled, _ := n["tls"].(bool); enabled {
		security = "tls"
	}
	q.Set("security", security)
	if v := str(n["servername"]); v != "" {
		q.Set("sni", v)
	}
	if v := str(n["client-fingerprint"]); v != "" {
		q.Set("fp", v)
	}
	if v := str(n["flow"]); v != "" {
		q.Set("flow", v)
	}
	if insecure, _ := n["skip-cert-verify"].(bool); insecure {
		q.Set("insecure", "1")
		q.Set("allowInsecure", "1")
	}
	if v := stringList(n["alpn"]); v != "" {
		q.Set("alpn", v)
	}
	if v := str(n["verify-peer-cert-by-name"]); v != "" {
		q.Set("vcn", v)
	}
	if v := str(n["certificate-sha256"]); v != "" {
		q.Set("pcs", v)
	}
	if network == "ws" {
		if opts, ok := n["ws-opts"].(map[string]any); ok {
			if key := firstUnknownKey(opts, "path", "headers"); key != "" {
				return nil, fmt.Errorf("unsupported websocket option %q", key)
			}
			if v := str(opts["path"]); v != "" {
				q.Set("path", v)
			}
			if headers, ok := opts["headers"].(map[string]any); ok {
				if key := firstUnknownKeyFold(headers, "host"); key != "" {
					return nil, fmt.Errorf("unsupported websocket header %q", key)
				}
				if v := str(headers["Host"]); v != "" {
					q.Set("host", v)
				} else if v := str(headers["host"]); v != "" {
					q.Set("host", v)
				}
			}
		}
	}
	if network == "grpc" {
		if opts, ok := n["grpc-opts"].(map[string]any); ok {
			if key := firstUnknownKey(opts, "grpc-service-name", "grpc-authority", "grpc-mode"); key != "" {
				return nil, fmt.Errorf("unsupported grpc option %q", key)
			}
			if v := str(opts["grpc-service-name"]); v != "" {
				q.Set("serviceName", v)
			}
			if v := str(opts["grpc-authority"]); v != "" {
				q.Set("authority", v)
			}
			if v := str(opts["grpc-mode"]); v != "" {
				q.Set("mode", v)
			}
		}
	}
	if network == "tcp" {
		if _, exists := n["http-opts"]; exists {
			return nil, fmt.Errorf("tcp http header options are not representable")
		}
	}
	return q, nil
}

func firstUnknownKey(values map[string]any, allowed ...string) string {
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

func firstUnknownKeyFold(values map[string]any, allowed ...string) string {
	known := map[string]bool{}
	for _, key := range allowed {
		known[strings.ToLower(key)] = true
	}
	for key := range values {
		if !known[strings.ToLower(key)] {
			return key
		}
	}
	return ""
}

func validEndpoint(n map[string]any) bool {
	port, err := strconv.Atoi(str(n["port"]))
	return str(n["server"]) != "" && err == nil && port > 0 && port <= 65535
}

func endpoint(n map[string]any) string {
	return netJoinHostPort(str(n["server"]), str(n["port"]))
}

func netJoinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func strDefault(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
}

func stringList(v any) string {
	switch values := v.(type) {
	case []any:
		parts := make([]string, 0, len(values))
		for _, item := range values {
			if s := str(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(values, ",")
	default:
		return str(v)
	}
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return ""
	}
}
