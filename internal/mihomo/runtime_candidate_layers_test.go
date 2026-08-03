package mihomo

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDetailedCandidateMergesOverrideAndExplainsEveryLayer(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		Port:            7890,
		ControlEndpoint: filepath.Join(t.TempDir(), "mihomo.sock"),
		Platform:        "linux",
	}
	result, err := builder.BuildDetailed(
		[]byte(`
ipv6: false
external-ui: /tmp/unsafe
tunnels:
  - tcp,0.0.0.0:7777,127.0.0.1:22,DIRECT
ntp:
  enable: true
  write-to-system: true
dns:
  enable: true
  listen: 0.0.0.0:53
  nameserver: [1.1.1.1]
proxies: []
rules: [MATCH,DIRECT]
`),
		[]byte(`
dns:
  nameserver: [9.9.9.9]
log-level: debug
`),
		nil,
	)
	if err != nil {
		t.Fatalf("build detailed candidate: %v", err)
	}
	var candidate map[string]any
	if err := yaml.Unmarshal(result.YAML, &candidate); err != nil {
		t.Fatalf("decode detailed candidate: %v", err)
	}
	dns := candidate["dns"].(map[string]any)
	if _, found := dns["listen"]; found {
		t.Fatal("Runtime-owned dns.listen survived candidate generation")
	}
	nameservers := dns["nameserver"].([]any)
	if len(nameservers) != 1 || nameservers[0] != "9.9.9.9" {
		t.Fatalf("override nameservers = %#v", nameservers)
	}
	if candidate["ipv6"] != true || candidate["log-level"] != "debug" {
		t.Fatalf("layered candidate = %#v", candidate)
	}
	if _, found := candidate["external-ui"]; found {
		t.Fatal("removed Runtime-owned external-ui survived candidate generation")
	}
	if _, found := candidate["tunnels"]; found {
		t.Fatal("removed Runtime-owned tunnels survived candidate generation")
	}
	if _, found := candidate["ntp"]; found {
		t.Fatal("removed Runtime-owned NTP survived candidate generation")
	}
	assertFieldOrigin(t, result.FieldOrigins, "dns.enable", CandidateOriginSource, CandidateStatusKept, "")
	assertFieldOrigin(t, result.FieldOrigins, "dns.nameserver", CandidateOriginOverride, CandidateStatusOverridden, CandidateOriginSource)
	assertFieldOrigin(t, result.FieldOrigins, "dns.listen", CandidateOriginRuntime, CandidateStatusRemoved, CandidateOriginSource)
	assertFieldOrigin(t, result.FieldOrigins, "external-ui", CandidateOriginRuntime, CandidateStatusRemoved, CandidateOriginSource)
	assertFieldOrigin(t, result.FieldOrigins, "tunnels", CandidateOriginRuntime, CandidateStatusRemoved, CandidateOriginSource)
	assertFieldOrigin(t, result.FieldOrigins, "ntp", CandidateOriginRuntime, CandidateStatusRemoved, CandidateOriginSource)
	assertFieldOrigin(t, result.FieldOrigins, "ipv6", CandidateOriginRuntime, CandidateStatusReplaced, CandidateOriginSource)
	assertFieldOrigin(t, result.FieldOrigins, "log-level", CandidateOriginOverride, CandidateStatusAdded, "")
	assertFieldOrigin(t, result.FieldOrigins, "listeners", CandidateOriginRuntime, CandidateStatusAdded, "")
	assertFieldOrigin(t, result.FieldOrigins, "rules", CandidateOriginRuntime, CandidateStatusReplaced, CandidateOriginSource)
}

func TestDetailedCandidateAppliesRuntimeTrafficPolicyAfterAdvancedOverride(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		ControlEndpoint: filepath.Join(t.TempDir(), "mihomo.sock"),
		Platform:        "linux",
		TrafficPolicy:   TrafficPolicyRule,
	}
	result, err := builder.BuildDetailed(
		[]byte("mode: global\nproxy-groups:\n  - name: PROXY\n    type: select\n    proxies: [DIRECT]\nrules: [MATCH,PROXY]\n"),
		[]byte("mode: direct\n"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	if err := yaml.Unmarshal(result.YAML, &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate["mode"] != "rule" || result.TrafficPolicy != TrafficPolicyRule {
		t.Fatalf("traffic policy candidate=%#v detail=%#v", candidate["mode"], result)
	}
	assertFieldOrigin(t, result.FieldOrigins, "mode", CandidateOriginRuntime, CandidateStatusReplaced, CandidateOriginOverride)
}

func TestDetailedCandidateRejectsGlobalTrafficPolicyWithoutProxyGroup(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		ControlEndpoint: filepath.Join(t.TempDir(), "mihomo.sock"),
		Platform:        "linux",
		TrafficPolicy:   TrafficPolicyGlobal,
	}
	if _, err := builder.BuildDetailed([]byte("proxies: []\nrules: [MATCH,DIRECT]\n"), nil, nil); err == nil || !strings.Contains(err.Error(), "proxy group") {
		t.Fatalf("global traffic policy error=%v", err)
	}

	builder.TrafficPolicy = TrafficPolicyFollowSource
	if _, err := builder.BuildDetailed([]byte("mode: global\nproxies: []\nrules: [MATCH,DIRECT]\n"), nil, nil); err == nil || !strings.Contains(err.Error(), "proxy group") {
		t.Fatalf("follow-source global traffic policy error=%v", err)
	}
}

func TestTrafficPolicyFromConfigurationReportsAppliedGlobalWithoutRevalidatingCandidate(t *testing.T) {
	policy, err := TrafficPolicyFromConfiguration([]byte("mode: global\nproxies: []\nrules: [MATCH,DIRECT]\n"))
	if err != nil || policy != TrafficPolicyGlobal {
		t.Fatalf("applied traffic policy=%q error=%v", policy, err)
	}
}

func TestDetailedCandidateRejectsRuntimeOwnedOverrideAndUnknownSensitiveFields(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		ControlEndpoint: filepath.Join(t.TempDir(), "mihomo.sock"),
		Platform:        "linux",
	}
	if _, err := builder.BuildDetailed(
		[]byte("proxies: []\n"),
		[]byte("listeners: []\n"),
		nil,
	); err == nil || !strings.Contains(err.Error(), "Runtime-owned") {
		t.Fatalf("Runtime-owned override error = %v", err)
	}
	if _, err := builder.BuildDetailed(
		[]byte("proxies: []\n"),
		[]byte("ntp:\n  enable: true\n  write-to-system: true\n"),
		nil,
	); err == nil || !strings.Contains(err.Error(), "Runtime-owned") {
		t.Fatalf("Runtime-owned NTP override error = %v", err)
	}
	if _, err := builder.BuildDetailed(
		[]byte("proxies: []\nunsafe-path: /etc/passwd\n"),
		nil,
		nil,
	); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("unknown path error = %v", err)
	}
	if _, err := builder.BuildDetailed(
		[]byte(`
proxies:
  - name: ws
    type: vmess
    server: 127.0.0.1
    port: 443
    uuid: 00000000-0000-0000-0000-000000000000
    network: ws
    ws-opts:
      path: /allowed-request-path
`),
		nil,
		nil,
	); err != nil {
		t.Fatalf("ordinary WebSocket request path was rejected: %v", err)
	}
}

func TestDetailedCandidateRewritesOnlyMatchingManagedResources(t *testing.T) {
	resourceID := "res_0123456789abcdef0123456789abcdef"
	resourcePath := filepath.Join(t.TempDir(), "provider.yaml")
	builder := ExplicitCandidateBuilder{
		ControlEndpoint: filepath.Join(t.TempDir(), "mihomo.sock"),
		Platform:        "linux",
	}
	source := []byte(`
proxy-providers:
  office:
    type: file
    path: resource://res_0123456789abcdef0123456789abcdef
proxies: []
`)
	result, err := builder.BuildDetailed(source, nil, []ManagedResource{{
		ID:   resourceID,
		Kind: ManagedResourceKindProxyProvider,
		Path: resourcePath,
	}})
	if err != nil {
		t.Fatalf("rewrite managed provider: %v", err)
	}
	if !strings.Contains(string(result.YAML), resourcePath) ||
		len(result.ReferencedResources) != 1 ||
		result.ReferencedResources[0] != resourceID {
		t.Fatalf("managed provider result = %#v\n%s", result, result.YAML)
	}
	assertFieldOrigin(
		t,
		result.FieldOrigins,
		"proxy-providers.office.path",
		CandidateOriginRuntime,
		CandidateStatusReplaced,
		CandidateOriginSource,
	)
	_, err = builder.BuildDetailed(source, nil, []ManagedResource{{
		ID:   resourceID,
		Kind: ManagedResourceKindRuleProvider,
		Path: resourcePath,
	}})
	if err == nil || !strings.Contains(err.Error(), "wrong type") {
		t.Fatalf("mismatched managed provider error = %v", err)
	}
}

func TestDetailedCandidateUsesPrecreatedLinuxTUNWithoutAutomaticRouting(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		Port:            7890,
		ControlEndpoint: filepath.Join(t.TempDir(), "mihomo.sock"),
		Platform:        "linux",
		TUN: &TUNCandidateSettings{
			Device:      "smxtun0",
			RoutingMark: 41234,
			IPv6Policy:  "proxy",
			HijackDNS:   true,
		},
	}
	result, err := builder.BuildDetailed([]byte(`
routing-mark: 1
tun:
  enable: true
  auto-route: true
  auto-redirect: true
  device: unsafe0
proxies: []
rules: [MATCH,DIRECT]
`), nil, nil)
	if err != nil {
		t.Fatalf("build Linux TUN candidate: %v", err)
	}
	var candidate map[string]any
	if err := yaml.Unmarshal(result.YAML, &candidate); err != nil {
		t.Fatalf("parse Linux TUN candidate: %v", err)
	}
	if candidate["routing-mark"] != 41234 {
		t.Fatalf("Linux TUN routing-mark=%#v", candidate["routing-mark"])
	}
	tun, ok := candidate["tun"].(map[string]any)
	if !ok {
		t.Fatalf("Linux TUN candidate tun=%#v", candidate["tun"])
	}
	if tun["enable"] != true ||
		tun["device"] != "smxtun0" ||
		tun["auto-route"] != false ||
		tun["auto-redirect"] != false {
		t.Fatalf("Linux TUN candidate tun=%#v", tun)
	}
	dnsHijack, ok := tun["dns-hijack"].([]any)
	if !ok || len(dnsHijack) != 2 ||
		dnsHijack[0] != "any:53" ||
		dnsHijack[1] != "tcp://any:53" {
		t.Fatalf("Linux TUN dns-hijack=%#v", tun["dns-hijack"])
	}
}

func TestDetailedCandidateUsesPrecreatedWindowsWintunWithoutLinuxMark(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		Port:            7890,
		ControlEndpoint: `\\.\pipe\submux-runtime-mihomo`,
		Platform:        "windows",
		TUN: &TUNCandidateSettings{
			Device:     "SubmuxRuntime",
			IPv6Policy: "proxy",
			HijackDNS:  true,
		},
	}
	result, err := builder.BuildDetailed([]byte(`
routing-mark: 1
tun:
  enable: true
  auto-route: true
  device: unsafe0
proxies: []
rules: [MATCH,DIRECT]
`), nil, nil)
	if err != nil {
		t.Fatalf("build Windows TUN candidate: %v", err)
	}
	var candidate map[string]any
	if err := yaml.Unmarshal(result.YAML, &candidate); err != nil {
		t.Fatalf("parse Windows TUN candidate: %v", err)
	}
	if _, exists := candidate["routing-mark"]; exists {
		t.Fatalf("Windows TUN candidate retained Linux routing-mark=%#v", candidate["routing-mark"])
	}
	if candidate["external-controller-pipe"] != `\\.\pipe\submux-runtime-mihomo` {
		t.Fatalf("Windows controller Pipe=%#v", candidate["external-controller-pipe"])
	}
	tun, ok := candidate["tun"].(map[string]any)
	if !ok ||
		tun["enable"] != true ||
		tun["device"] != "SubmuxRuntime" ||
		tun["auto-route"] != false ||
		tun["auto-redirect"] != false {
		t.Fatalf("Windows TUN candidate tun=%#v", candidate["tun"])
	}
}

func TestDetailedCandidateLetsMihomoAllocateDarwinUtun(t *testing.T) {
	controlEndpoint := filepath.Join(t.TempDir(), "mihomo.sock")
	builder := ExplicitCandidateBuilder{
		Port:            7890,
		ControlEndpoint: controlEndpoint,
		Platform:        "darwin",
		TUN: &TUNCandidateSettings{
			Device:     "utun",
			IPv6Policy: "proxy",
			HijackDNS:  true,
		},
	}
	result, err := builder.BuildDetailed([]byte(`
routing-mark: 1
tun:
  enable: true
  auto-route: true
  device: unsafe0
proxies: []
rules: [MATCH,DIRECT]
`), nil, nil)
	if err != nil {
		t.Fatalf("build macOS TUN candidate: %v", err)
	}
	var candidate map[string]any
	if err := yaml.Unmarshal(result.YAML, &candidate); err != nil {
		t.Fatalf("parse macOS TUN candidate: %v", err)
	}
	if _, exists := candidate["routing-mark"]; exists {
		t.Fatalf("macOS TUN candidate retained Linux routing-mark=%#v", candidate["routing-mark"])
	}
	if candidate["external-controller-unix"] != controlEndpoint {
		t.Fatalf("macOS controller Socket=%#v", candidate["external-controller-unix"])
	}
	tun, ok := candidate["tun"].(map[string]any)
	if !ok || tun["enable"] != true ||
		tun["auto-route"] != false ||
		tun["auto-redirect"] != false {
		t.Fatalf("macOS TUN candidate tun=%#v", candidate["tun"])
	}
	if _, exists := tun["device"]; exists {
		t.Fatalf("macOS TUN candidate fixed a utun device=%#v", tun["device"])
	}
}

func TestValidateManagedResourceRejectsTypeMismatchAndParsesPrivateKeys(t *testing.T) {
	if err := ValidateManagedResource(
		ManagedResourceKindProxyProvider,
		[]byte("payload:\n  - DOMAIN,example.com\n"),
	); err == nil {
		t.Fatal("proxy provider accepted a rule-provider payload")
	}
	if err := ValidateManagedResource(
		ManagedResourceKindRuleProvider,
		[]byte("payload:\n  - DOMAIN,example.com\n"),
	); err != nil {
		t.Fatalf("valid rule provider rejected: %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := ValidateManagedResource(ManagedResourceKindPrivateKey, keyPEM); err != nil {
		t.Fatalf("valid private key rejected: %v", err)
	}
	if err := ValidateManagedResource(
		ManagedResourceKindCertificate,
		keyPEM,
	); err == nil {
		t.Fatal("certificate resource accepted a private key")
	}
}

func assertFieldOrigin(
	t *testing.T,
	fields []CandidateFieldOrigin,
	path string,
	origin string,
	status string,
	replaced string,
) {
	t.Helper()
	for _, field := range fields {
		if field.Path != path {
			continue
		}
		if field.Origin != origin ||
			field.Status != status ||
			field.ReplacedOrigin != replaced {
			t.Fatalf("field %s = %#v", path, field)
		}
		return
	}
	t.Fatalf("field origin %s not found in %#v", path, fields)
}
