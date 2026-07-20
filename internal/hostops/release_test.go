package hostops

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOfficialReleaseSourceAllowsSlowBoundedDownloads(t *testing.T) {
	if got := OfficialReleaseSource(nil).httpClient.Timeout; got != 10*time.Minute {
		t.Fatalf("default release timeout = %s", got)
	}
}

func TestOfficialReleaseHTTPClientSupportsExplicitHTTPAndSOCKS5Proxy(t *testing.T) {
	httpClient, err := officialReleaseHTTPClient("http://127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.github.com/", nil)
	proxyURL, err := httpClient.Transport.(*http.Transport).Proxy(request)
	if err != nil || proxyURL == nil || proxyURL.String() != "http://127.0.0.1:18080" {
		t.Fatalf("HTTP proxy = %v, %v", proxyURL, err)
	}
	socksClient, err := officialReleaseHTTPClient("socks5://127.0.0.1:11080")
	if err != nil {
		t.Fatal(err)
	}
	transport := socksClient.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.DialContext == nil {
		t.Fatal("SOCKS5 proxy did not replace the transport dialer")
	}
}

func TestReleaseSourceUsesExactAssetAndDigest(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte("mihomo-binary"))
	_ = writer.Close()
	sum := sha256.Sum256(compressed.Bytes())
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/releases/tags/v1.19.28", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.19.28", "draft": false, "prerelease": false,
			"assets": []map[string]any{{"name": "mihomo-linux-amd64-v1.19.28.gz", "state": "uploaded", "size": compressed.Len(), "digest": "sha256:" + hex.EncodeToString(sum[:]), "browser_download_url": server.URL + "/asset"}},
		})
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(compressed.Bytes()) })
	server = httptest.NewServer(mux)
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	var phases []string
	source := &releaseSource{apiBase: server.URL, httpClient: server.Client(), allowURL: func(value *url.URL) bool { return value.Host == parsed.Host }, progress: func(value CoreProgress) { phases = append(phases, value.Phase) }}
	result, err := source.Fetch(context.Background(), "stable", "v1.19.28", "linux", "amd64")
	if err != nil || string(result.Binary) != "mihomo-binary" {
		t.Fatalf("fetch result=%#v err=%v", result, err)
	}
	for _, expected := range []string{"resolving_release", "downloading", "verifying_download", "unpacking"} {
		found := false
		for _, phase := range phases {
			found = found || phase == expected
		}
		if !found {
			t.Fatalf("release progress is missing %q: %v", expected, phases)
		}
	}
}

func TestReleaseSourceListsOnlyInstallableVersionsForAgentPlatform(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	var requestQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("User-Agent") != "submux-agent" {
			t.Errorf("unexpected release list request: %s %#v", r.URL.String(), r.Header)
		}
		requestQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "Prerelease-Alpha", "draft": false, "prerelease": true, "assets": []map[string]any{
				{"name": "mihomo-linux-amd64-alpha-e911985.gz", "state": "uploaded", "size": 123, "digest": digest},
				{"name": "mihomo-windows-amd64-compatible-alpha-e911985.zip", "state": "uploaded", "size": 123, "digest": digest},
			}},
			{"tag_name": "v1.19.28", "draft": false, "prerelease": false, "assets": []map[string]any{
				{"name": "mihomo-linux-amd64-v1.19.28.gz", "state": "uploaded", "size": 456, "digest": digest},
			}},
			{"tag_name": "v1.19.27", "draft": false, "prerelease": false, "assets": []map[string]any{
				{"name": "mihomo-linux-amd64-v1.19.27.gz", "state": "uploaded", "size": 456},
			}},
		})
	}))
	defer server.Close()
	var phases []string
	source := &releaseSource{apiBase: server.URL, httpClient: server.Client(), progress: func(value CoreProgress) { phases = append(phases, value.Phase) }}
	stable, err := source.ListVersions(context.Background(), "stable", "linux", "amd64", 30)
	if err != nil || len(stable) != 1 || stable[0] != "v1.19.28" {
		t.Fatalf("stable versions = %#v, %v", stable, err)
	}
	alpha, err := source.ListVersions(context.Background(), "alpha", "linux", "amd64", 30)
	if err != nil || len(alpha) != 1 || alpha[0] != "alpha-e911985" {
		t.Fatalf("alpha versions = %#v, %v", alpha, err)
	}
	if requestQuery != "per_page=31&page=1" {
		t.Fatalf("release list query = %q", requestQuery)
	}
	if len(phases) != 2 || phases[0] != "listing_releases" || phases[1] != "listing_releases" {
		t.Fatalf("release list phases = %#v", phases)
	}
}

func TestReleaseCoordinatesRejectControlPlaneURLsAndInexactVersions(t *testing.T) {
	if _, _, err := releaseCoordinates("stable", "latest", "linux", "amd64"); err == nil {
		t.Fatal("latest was accepted instead of an exact version")
	}
	if _, _, err := releaseCoordinates("alpha", "alpha", "linux", "amd64"); err == nil {
		t.Fatal("inexact alpha was accepted")
	}
	if officialDownloadURL(&url.URL{Scheme: "https", Host: "example.com"}) {
		t.Fatal("non-GitHub download host was accepted")
	}
}

func TestAlphaReleaseCoordinatesUsePinnedCommitAssets(t *testing.T) {
	for _, test := range []struct {
		osName string
		arch   string
		asset  string
	}{
		{"linux", "amd64", "mihomo-linux-amd64-alpha-e911985.gz"},
		{"linux", "arm64", "mihomo-linux-arm64-alpha-e911985.gz"},
		{"windows", "amd64", "mihomo-windows-amd64-compatible-alpha-e911985.zip"},
		{"windows", "arm64", "mihomo-windows-arm64-alpha-e911985.zip"},
	} {
		tag, asset, err := releaseCoordinates("alpha", "alpha-e911985", test.osName, test.arch)
		if err != nil || tag != "Prerelease-Alpha" || asset != test.asset {
			t.Fatalf("%s/%s coordinates: tag=%q asset=%q err=%v", test.osName, test.arch, tag, asset, err)
		}
	}
}

func TestWindowsReleaseUsesCompatibleAssetAndSafeZip(t *testing.T) {
	tag, asset, err := releaseCoordinates("stable", "v1.19.28", "windows", "amd64")
	if err != nil || tag != "v1.19.28" || asset != "mihomo-windows-amd64-compatible-v1.19.28.zip" {
		t.Fatalf("unexpected Windows release coordinates: %q %q %v", tag, asset, err)
	}
	var compressed bytes.Buffer
	writer := zip.NewWriter(&compressed)
	entry, err := writer.Create("mihomo.exe")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("windows-mihomo"))
	_ = writer.Close()
	binary, err := unpackReleaseBinary(asset, compressed.Bytes())
	if err != nil || string(binary) != "windows-mihomo" {
		t.Fatalf("Windows archive result=%q err=%v", binary, err)
	}

	compressed.Reset()
	writer = zip.NewWriter(&compressed)
	entry, _ = writer.Create("../mihomo.exe")
	_, _ = entry.Write([]byte("unsafe"))
	_ = writer.Close()
	if _, err := unpackReleaseBinary(asset, compressed.Bytes()); err == nil {
		t.Fatal("path-traversing Windows archive was accepted")
	}
}
