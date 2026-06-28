package output

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
)

// Base64Adapter 把节点转成分享 URI 列表再 base64(通用订阅格式)。
type Base64Adapter struct{}

func (Base64Adapter) Format() string { return "base64" }

func (Base64Adapter) Render(cfg map[string]any) ([]byte, string, error) {
	proxies, _ := cfg["proxies"].([]any)
	var lines []string
	for _, p := range proxies {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if uri, ok := nodeToURI(m); ok {
			lines = append(lines, uri)
		}
	}
	enc := base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))
	return []byte(enc), "text/plain; charset=utf-8", nil
}

// nodeToURI 目前支持 vless;其它协议返回 false(MVP 跳过,后续补 vmess/trojan/ss)。
func nodeToURI(n map[string]any) (string, bool) {
	if s, _ := n["type"].(string); s == "vless" {
		return vlessURI(n), true
	}
	return "", false
}

func vlessURI(n map[string]any) string {
	q := url.Values{}
	netw := str(n["network"])
	if netw == "" {
		netw = "tcp"
	}
	q.Set("type", netw)
	if b, _ := n["tls"].(bool); b {
		if ro, ok := n["reality-opts"].(map[string]any); ok {
			q.Set("security", "reality")
			if v := str(ro["public-key"]); v != "" {
				q.Set("pbk", v)
			}
			if v := str(ro["short-id"]); v != "" {
				q.Set("sid", v)
			}
		} else {
			q.Set("security", "tls")
		}
		if v := str(n["servername"]); v != "" {
			q.Set("sni", v)
		}
		if v := str(n["client-fingerprint"]); v != "" {
			q.Set("fp", v)
		}
	}
	if v := str(n["flow"]); v != "" {
		q.Set("flow", v)
	}
	u := url.URL{
		Scheme:   "vless",
		User:     url.User(str(n["uuid"])),
		Host:     str(n["server"]) + ":" + str(n["port"]),
		RawQuery: q.Encode(),
		Fragment: str(n["name"]),
	}
	return u.String()
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return ""
	}
}
