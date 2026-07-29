package runtimesource

import (
	"context"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (function resolverFunc) LookupNetIP(
	ctx context.Context,
	network string,
	host string,
) ([]netip.Addr, error) {
	return function(ctx, network, host)
}

func TestNormalizeDraftBindsHighRiskSettingsToCanonicalTarget(t *testing.T) {
	interval := int64((6 * time.Hour) / time.Second)
	config, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:                   "office",
		URL:                    "https://BÜCHER.example/config.yaml?token=secret",
		Route:                  runtimeapi.SourceRouteDirect,
		AuthorizedTarget:       "https://xn--bcher-kva.example:443",
		AllowPrivate:           true,
		RefreshIntervalSeconds: &interval,
	})
	if err != nil {
		t.Fatalf("normalize source draft: %v", err)
	}
	if config.Target != "https://xn--bcher-kva.example:443" ||
		config.RedactedTarget != "https://xn--bcher-kva.example:443/…" ||
		config.URL.RawQuery != "token=secret" {
		t.Fatalf("normalized source = %#v", config)
	}

	_, err = NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:             "office",
		URL:              "https://private.example/config.yaml",
		AuthorizedTarget: "https://other.example:443",
		AllowPrivate:     true,
	})
	if err == nil {
		t.Fatal("NormalizeDraft accepted a mismatched authorization target")
	}
}

func TestSourcePolicyRequiresExplicitPrivateAndPublicHTTPAuthorization(t *testing.T) {
	publicResolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	privateResolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.20.30.40")}, nil
	})
	ctx := context.Background()

	publicHTTP, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name: "public-http",
		URL:  "http://download.example/config.yaml",
	})
	if err != nil {
		t.Fatalf("normalize public HTTP source: %v", err)
	}
	if _, _, err := publicHTTP.validateRequestTarget(ctx, publicResolver, publicHTTP.URL, "http"); err == nil {
		t.Fatal("public HTTP source was accepted without authorization")
	}
	authorizedHTTP, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:             "public-http",
		URL:              "http://download.example/config.yaml",
		AuthorizedTarget: "http://download.example:80",
		AllowHTTP:        true,
	})
	if err != nil {
		t.Fatalf("normalize authorized HTTP source: %v", err)
	}
	if _, _, err := authorizedHTTP.validateRequestTarget(ctx, publicResolver, authorizedHTTP.URL, "http"); err != nil {
		t.Fatalf("authorized public HTTP source was rejected: %v", err)
	}

	privateHTTPS, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name: "private-https",
		URL:  "https://private.example/config.yaml",
	})
	if err != nil {
		t.Fatalf("normalize private HTTPS source: %v", err)
	}
	if _, _, err := privateHTTPS.validateRequestTarget(ctx, privateResolver, privateHTTPS.URL, "https"); err == nil {
		t.Fatal("private HTTPS source was accepted without authorization")
	}
	authorizedPrivate, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:             "private-https",
		URL:              "https://private.example/config.yaml",
		AuthorizedTarget: "https://private.example:443",
		AllowPrivate:     true,
	})
	if err != nil {
		t.Fatalf("normalize authorized private source: %v", err)
	}
	if _, _, err := authorizedPrivate.validateRequestTarget(ctx, privateResolver, authorizedPrivate.URL, "https"); err != nil {
		t.Fatalf("authorized private source was rejected: %v", err)
	}
}

func TestSourcePolicyAllowsLoopbackHTTPButAlwaysRejectsMetadataAndLinkLocal(t *testing.T) {
	ctx := context.Background()
	loopback, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name: "loopback",
		URL:  "http://127.0.0.1:8080/config.yaml",
	})
	if err != nil {
		t.Fatalf("normalize loopback source: %v", err)
	}
	if _, _, err := loopback.validateRequestTarget(ctx, nil, loopback.URL, "http"); err != nil {
		t.Fatalf("default loopback HTTP source was rejected: %v", err)
	}

	linkLocal, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:             "link-local",
		URL:              "http://169.254.169.254/latest/meta-data",
		AuthorizedTarget: "http://169.254.169.254:80",
		AllowHTTP:        true,
		AllowPrivate:     true,
	})
	if err != nil {
		t.Fatalf("normalize link-local source: %v", err)
	}
	if _, _, err := linkLocal.validateRequestTarget(ctx, nil, linkLocal.URL, "http"); err == nil {
		t.Fatal("link-local metadata target was accepted with every compatibility flag")
	}

	_, err = NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name: "metadata-host",
		URL:  "https://metadata.google.internal/computeMetadata/v1/",
	})
	if err == nil {
		t.Fatal("known metadata hostname was accepted")
	}

	aliyun, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:             "aliyun-metadata",
		URL:              "https://100.100.100.200/latest/meta-data",
		AuthorizedTarget: "https://100.100.100.200:443",
		AllowPrivate:     true,
	})
	if err != nil {
		t.Fatalf("normalize Aliyun metadata source: %v", err)
	}
	if _, _, err := aliyun.validateRequestTarget(ctx, nil, aliyun.URL, "https"); err == nil {
		t.Fatal("Aliyun metadata address was accepted")
	}

	azure, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:             "azure-platform",
		URL:              "https://168.63.129.16/config.yaml",
		AuthorizedTarget: "https://168.63.129.16:443",
		AllowPrivate:     true,
	})
	if err != nil {
		t.Fatalf("normalize Azure platform source: %v", err)
	}
	if _, _, err := azure.validateRequestTarget(ctx, nil, azure.URL, "https"); err == nil {
		t.Fatal("Azure WireServer address was accepted")
	}

	awsIPv6Resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("fd00:ec2::254")}, nil
	})
	awsIPv6, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:             "aws-ipv6-metadata",
		URL:              "https://private.example/config.yaml",
		AuthorizedTarget: "https://private.example:443",
		AllowPrivate:     true,
	})
	if err != nil {
		t.Fatalf("normalize AWS IPv6 metadata source: %v", err)
	}
	if _, _, err := awsIPv6.validateRequestTarget(ctx, awsIPv6Resolver, awsIPv6.URL, "https"); err == nil {
		t.Fatal("AWS IPv6 metadata address returned by DNS was accepted")
	}
}

func TestSourcePolicyRejectsHTTPSDowngrade(t *testing.T) {
	config, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name: "secure",
		URL:  "https://example.com/config.yaml",
	})
	if err != nil {
		t.Fatalf("normalize HTTPS source: %v", err)
	}
	redirect, err := url.Parse("http://127.0.0.1/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.validateRequestTarget(context.Background(), nil, redirect, "https"); err == nil {
		t.Fatal("HTTPS downgrade redirect was accepted")
	}
}

func TestNormalizeDraftRejectsUnboundTLSCompatibilityAndInvalidLimits(t *testing.T) {
	tests := []runtimeapi.RemoteSourceDraft{
		{
			Name:          "skip-without-target",
			URL:           "https://example.com/config.yaml",
			SkipTLSVerify: true,
		},
		{
			Name:             "custom-ca-over-http",
			URL:              "http://127.0.0.1/config.yaml",
			AuthorizedTarget: "http://127.0.0.1:80",
			CustomCAPEM:      "not a certificate",
		},
		{
			Name:             "short-refresh",
			URL:              "https://example.com/config.yaml",
			TimeoutSeconds:   1,
			MaxResponseBytes: 512,
		},
	}
	for _, draft := range tests {
		if _, err := NormalizeDraft(draft); err == nil {
			t.Fatalf("NormalizeDraft accepted invalid draft %q", draft.Name)
		}
	}
}
