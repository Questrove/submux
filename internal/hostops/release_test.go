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
	"testing"
)

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
	source := &releaseSource{apiBase: server.URL, httpClient: server.Client(), allowURL: func(value *url.URL) bool { return value.Host == parsed.Host }}
	result, err := source.Fetch(context.Background(), "stable", "v1.19.28", "linux", "amd64")
	if err != nil || string(result.Binary) != "mihomo-binary" {
		t.Fatalf("fetch result=%#v err=%v", result, err)
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
