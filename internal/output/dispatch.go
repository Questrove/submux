package output

import "strings"

// SelectByUA 按 User-Agent 选输出适配器。
func SelectByUA(ua, defaultFormat string) Adapter {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "clash"), strings.Contains(l, "mihomo"), strings.Contains(l, "meta"):
		return ClashAdapter{}
	case strings.Contains(l, "sing-box"), strings.Contains(l, "sing_box"):
		return byName(defaultFormat) // sing-box 适配器属二期,暂回退默认
	default:
		return Base64Adapter{}
	}
}

func byName(format string) Adapter {
	if format == "base64" {
		return Base64Adapter{}
	}
	return ClashAdapter{}
}
