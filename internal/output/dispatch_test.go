package output

import "testing"

func TestSelectByUA(t *testing.T) {
	cases := []struct {
		ua, def, want string
	}{
		{"clash-verge/2.0", "clash", "clash"},
		{"mihomo/1.18", "clash", "clash"},
		{"ClashMetaForAndroid", "clash", "clash"},
		{"sing-box/1.8", "clash", "clash"}, // sing-box 回退 default
		{"v2rayN/7.0", "clash", "base64"},
		{"NekoBox/1.3", "clash", "base64"},
		{"Mozilla/5.0", "clash", "clash"}, // 未知 → default
		{"", "clash", "clash"},
		{"Mozilla/5.0", "base64", "base64"},
	}
	for _, c := range cases {
		got := SelectByUA(c.ua, c.def).Format()
		if got != c.want {
			t.Fatalf("UA %q def %q: got %q want %q", c.ua, c.def, got, c.want)
		}
	}
}
