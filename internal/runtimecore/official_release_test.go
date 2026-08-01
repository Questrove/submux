package runtimecore

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
)

func TestReleaseCoordinatesUseExactSupportedAssets(t *testing.T) {
	tests := []struct {
		osName string
		arch   string
		asset  string
	}{
		{osName: "linux", arch: "amd64", asset: "mihomo-linux-amd64-compatible-v1.19.29.gz"},
		{osName: "linux", arch: "arm64", asset: "mihomo-linux-arm64-v1.19.29.gz"},
		{osName: "windows", arch: "amd64", asset: "mihomo-windows-amd64-compatible-v1.19.29.zip"},
		{osName: "windows", arch: "arm64", asset: "mihomo-windows-arm64-v1.19.29.zip"},
		{osName: "darwin", arch: "amd64", asset: "mihomo-darwin-amd64-compatible-v1.19.29.gz"},
		{osName: "darwin", arch: "arm64", asset: "mihomo-darwin-arm64-v1.19.29.gz"},
	}
	for _, test := range tests {
		tag, asset, err := releaseCoordinates("v1.19.29", test.osName, test.arch)
		if err != nil || tag != "v1.19.29" || asset != test.asset {
			t.Fatalf("releaseCoordinates(%q, %q) = %q/%q, %v", test.osName, test.arch, tag, asset, err)
		}
	}
	for _, test := range []struct {
		version string
		osName  string
		arch    string
	}{
		{version: "latest", osName: "linux", arch: "amd64"},
		{version: "v1.19.29-rc1", osName: "linux", arch: "amd64"},
		{version: "v1.19.29", osName: "freebsd", arch: "amd64"},
		{version: "v1.19.29", osName: "linux", arch: "386"},
	} {
		if _, _, err := releaseCoordinates(test.version, test.osName, test.arch); err == nil {
			t.Fatalf("unsupported release coordinates were accepted: %#v", test)
		}
	}
}

func TestOfficialReleaseSourceUsesExactAssetAndDigest(t *testing.T) {
	version := "v1.19.29"
	binary := []byte("mihomo executable")
	compressed := gzipValue(t, binary)
	assetDigest := sha256.Sum256(compressed)
	assetName := "mihomo-linux-amd64-compatible-" + version + ".gz"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/releases/tags/" + version:
			_ = json.NewEncoder(w).Encode(releaseMetadata{
				TagName: version,
				Assets: []releaseAssetMetadata{{
					Name:               assetName,
					State:              "uploaded",
					Size:               int64(len(compressed)),
					Digest:             "sha256:" + hex.EncodeToString(assetDigest[:]),
					BrowserDownloadURL: server.URL + "/asset",
				}},
			})
		case "/asset":
			_, _ = w.Write(compressed)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	source := &OfficialReleaseSource{
		apiBase: server.URL,
		client:  server.Client(),
		allowURL: func(value *url.URL) bool {
			return value.Scheme == serverURL.Scheme && value.Host == serverURL.Host
		},
	}
	release, err := source.FetchStable(context.Background(), version, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != version || release.AssetName != assetName || !bytes.Equal(release.Data, binary) {
		t.Fatalf("release = %#v", release)
	}
	binaryDigest := sha256.Sum256(binary)
	if release.AssetDigest != hex.EncodeToString(assetDigest[:]) || release.BinaryDigest != hex.EncodeToString(binaryDigest[:]) {
		t.Fatalf("release digests = asset %q binary %q", release.AssetDigest, release.BinaryDigest)
	}
}

func TestOfficialReleaseSourceRejectsDigestAndDownloadURLMismatch(t *testing.T) {
	for _, test := range []struct {
		name        string
		digest      string
		downloadURL string
	}{
		{
			name:        "digest",
			digest:      strings.Repeat("0", 64),
			downloadURL: "",
		},
		{
			name:        "download-url",
			digest:      "",
			downloadURL: "https://example.com/mihomo.gz",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			version := "v1.19.29"
			compressed := gzipValue(t, []byte("mihomo executable"))
			actualDigest := sha256.Sum256(compressed)
			digest := test.digest
			if digest == "" {
				digest = hex.EncodeToString(actualDigest[:])
			}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/releases/tags/" + version:
					downloadURL := test.downloadURL
					if downloadURL == "" {
						downloadURL = server.URL + "/asset"
					}
					_ = json.NewEncoder(w).Encode(releaseMetadata{
						TagName: version,
						Assets: []releaseAssetMetadata{{
							Name:               "mihomo-linux-amd64-" + version + ".gz",
							State:              "uploaded",
							Size:               int64(len(compressed)),
							Digest:             "sha256:" + digest,
							BrowserDownloadURL: downloadURL,
						}},
					})
				case "/asset":
					_, _ = w.Write(compressed)
				default:
					http.NotFound(w, request)
				}
			}))
			defer server.Close()
			serverURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			source := &OfficialReleaseSource{
				apiBase: server.URL,
				client:  server.Client(),
				allowURL: func(value *url.URL) bool {
					return value.Scheme == serverURL.Scheme && value.Host == serverURL.Host
				},
			}
			if _, err := source.FetchStable(context.Background(), version, "linux", "amd64"); err == nil {
				t.Fatal("invalid official release metadata was accepted")
			}
		})
	}
}

func TestWindowsReleaseArchiveRejectsPathsAndMultipleFiles(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string]string
	}{
		{name: "path", files: map[string]string{"nested/mihomo.exe": "binary"}},
		{name: "multiple", files: map[string]string{"mihomo.exe": "binary", "extra.txt": "extra"}},
		{name: "wrong-extension", files: map[string]string{"mihomo": "binary"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := zipValue(t, test.files)
			if _, err := unpackReleaseBinary("mihomo-windows-amd64-compatible-v1.19.29.zip", archive); err == nil {
				t.Fatal("unsafe Windows release archive was accepted")
			}
		})
	}
	archive := zipValue(t, map[string]string{"mihomo.exe": "binary"})
	value, err := unpackReleaseBinary("mihomo-windows-amd64-compatible-v1.19.29.zip", archive)
	if err != nil || string(value) != "binary" {
		t.Fatalf("valid Windows archive = %q, %v", value, err)
	}
}

func TestOfficialDownloadURLIsFixedToGitHubHosts(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/MetaCubeX/mihomo/releases/download/v1.19.29/asset.gz",
		"https://release-assets.githubusercontent.com/object",
		"https://objects.githubusercontent.com/object",
	} {
		value, err := url.Parse(raw)
		if err != nil || !officialDownloadURL(value) {
			t.Fatalf("official URL %q was rejected", raw)
		}
	}
	for _, raw := range []string{
		"http://github.com/MetaCubeX/mihomo/releases/download/v1.19.29/asset.gz",
		"https://user@github.com/asset",
		"https://example.com/asset",
	} {
		value, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if officialDownloadURL(value) {
			t.Fatalf("unofficial URL %q was accepted", raw)
		}
	}
}

func gzipValue(t *testing.T, value []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(value); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func zipValue(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, value := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
