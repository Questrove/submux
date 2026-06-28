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
		{"Mozilla/5.0", "clash", "base64"}, // 未知 → base64
		{"", "clash", "base64"},
	}
	for _, c := range cases {
		got := SelectByUA(c.ua, c.def).Format()
		if got != c.want {
			t.Fatalf("UA %q def %q: got %q want %q", c.ua, c.def, got, c.want)
		}
	}
}
