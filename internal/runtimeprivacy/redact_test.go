package runtimeprivacy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"submux/internal/runtimeapi"
)

func TestRedactURLPreservesAddressShapeAndRepeatedKeys(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		host string
	}{
		{
			name: "IPv4",
			raw:  "https://user:pass@192.0.2.10:8443/path?token=one&token=two#access_token=three",
			host: "192.0.2.10:8443",
		},
		{
			name: "IPv6",
			raw:  "https://user:p%40ss@[2001:db8::10]:8443/path?sig=a%2Fb&sig=c%20d",
			host: "[2001:db8::10]:8443",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := RedactURL(test.raw)
			for _, secret := range []string{"user", "pass", "p%40ss", "one", "two", "three", "a%2Fb", "c%20d"} {
				if strings.Contains(got, secret) {
					t.Fatalf("redacted URL leaked %q: %s", secret, got)
				}
			}
			if !strings.Contains(got, test.host) || strings.Count(got, "%5BREDACTED%5D") < 2 {
				t.Fatalf("redacted URL lost host or repeated keys: %s", got)
			}
		})
	}
}

func TestRedactTextHandlesHeadersPrivateKeysPathsAndNestedErrors(t *testing.T) {
	nested := errors.Join(
		errors.New(`GET https://alice:pw@[2001:db8::1]/config?token=a%2Fb&token=two`),
		errors.New("mihomo request Authorization: Bearer abc\nruntime response Cookie: sid=secret\nC:\\Users\\Alice\\private\\config.yaml"),
		errors.New("runtime cache C:/Users/Alice/private/cache.db"),
		errors.New("generated Mihomo secret: controller-secret"),
		errors.New(`mihomo payload {"authorization":"Bearer json-secret"}`),
		errors.New("-----BEGIN PRIVATE KEY-----\nprivate material\n-----END PRIVATE KEY-----"),
	)
	got := RedactError(nested)
	for _, secret := range []string{"alice", "pw", "a%2Fb", "two", "Bearer abc", "sid=secret", "controller-secret", "json-secret", `C:\Users\Alice`, "C:/Users/Alice", "private material"} {
		if strings.Contains(got, secret) {
			t.Fatalf("nested error leaked %q: %s", secret, got)
		}
	}
}

func TestRedactJSONUsesFieldNamesAndNestedText(t *testing.T) {
	raw := json.RawMessage(`{"url":"https://u:p@127.0.0.1/x?token=one&token=two","authorization":"Bearer value","nested":{"message":"Cookie: sid=value","private_key":"material"}}`)
	got := string(RedactJSON(raw))
	for _, secret := range []string{"u:p", "one", "two", "Bearer value", "sid=value", "material"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted JSON leaked %q: %s", secret, got)
		}
	}
}

func TestCandidatePreviewRedactsCredentialsAndLocalPaths(t *testing.T) {
	preview := SanitizeCandidatePreview(runtimeapi.CandidatePreview{
		CandidateYAML: "proxies:\n  - uuid: 01234567-89ab-cdef-0123-456789abcdef\n    password: proxy-password\nsecret: controller-secret\npath: C:\\Users\\Test\\provider.yaml\n",
		FieldOrigins: []runtimeapi.CandidateFieldOrigin{{
			Path:           "proxy-providers.main.path",
			Origin:         `C:\Users\Test\override.yaml`,
			ReplacedOrigin: "/etc/submux/config.yaml",
		}},
	})
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatalf("encode candidate preview: %v", err)
	}
	for _, secret := range []string{
		"01234567-89ab-cdef-0123-456789abcdef",
		"proxy-password",
		"controller-secret",
		`C:\Users\Test`,
		"/etc/submux/config.yaml",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("candidate preview leaked %q: %s", secret, encoded)
		}
	}
}
