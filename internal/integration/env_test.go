package integration

import (
	"strings"
	"testing"
)

func TestProxyEnvironmentUsesOnlyLoopbackAndPrivateBypass(t *testing.T) {
	bash, err := RenderProxyEnvironment("bash", 7890)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"http://127.0.0.1:7890", "socks5h://127.0.0.1:7890", "10.0.0.0/8", "192.168.0.0/16"} {
		if !strings.Contains(bash, required) {
			t.Fatalf("missing %q in %s", required, bash)
		}
	}
	if _, err := RenderProxyEnvironment("bash", 0); err == nil {
		t.Fatal("invalid port was accepted")
	}
	httpOnly, err := ProxyEnvironmentFor(8080, "http")
	if err != nil || httpOnly["ALL_PROXY"] != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected HTTP-only environment: %#v, %v", httpOnly, err)
	}
	socksOnly, err := ProxyEnvironmentFor(1080, "socks5")
	if err != nil || !strings.HasPrefix(socksOnly["HTTP_PROXY"], "socks5h://") {
		t.Fatalf("unexpected SOCKS-only environment: %#v, %v", socksOnly, err)
	}
	if _, err := ProxyEnvironmentFor(7890, "unknown"); err == nil {
		t.Fatal("unknown proxy listener kind was accepted")
	}
}
