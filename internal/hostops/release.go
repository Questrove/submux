package hostops

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	officialAPIBase = "https://api.github.com/repos/MetaCubeX/mihomo"
	maxAssetSize    = 100 << 20
	maxBinarySize   = 250 << 20
)

var (
	stableVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$`)
	alphaVersionPattern  = regexp.MustCompile(`^alpha-[0-9a-f]{7,40}$`)
)

type ReleaseBinary struct {
	Version     string
	AssetName   string
	AssetDigest string
	Binary      []byte
}

type releaseSource struct {
	apiBase    string
	httpClient *http.Client
	allowURL   func(*url.URL) bool
}

func OfficialReleaseSource(httpClient *http.Client) *releaseSource {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	clientCopy := *httpClient
	if clientCopy.Timeout == 0 {
		clientCopy.Timeout = 2 * time.Minute
	}
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 5 || !officialDownloadURL(request.URL) {
			return errors.New("Mihomo asset redirected outside GitHub's official download hosts")
		}
		return nil
	}
	return &releaseSource{apiBase: officialAPIBase, httpClient: &clientCopy, allowURL: officialDownloadURL}
}

func (s *releaseSource) Fetch(ctx context.Context, channel, version, osName, arch string) (ReleaseBinary, error) {
	tag, asset, err := releaseCoordinates(channel, version, osName, arch)
	if err != nil {
		return ReleaseBinary{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.apiBase, "/")+"/releases/tags/"+url.PathEscape(tag), nil)
	if err != nil {
		return ReleaseBinary{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "submux-agent")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return ReleaseBinary{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ReleaseBinary{}, fmt.Errorf("official Mihomo release API returned %d", response.StatusCode)
	}
	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name               string `json:"name"`
			State              string `json:"state"`
			Size               int64  `json:"size"`
			Digest             string `json:"digest"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&release); err != nil {
		return ReleaseBinary{}, fmt.Errorf("decode official release metadata: %w", err)
	}
	if release.TagName != tag || release.Draft || (channel == "stable" && release.Prerelease) || (channel == "alpha" && !release.Prerelease) {
		return ReleaseBinary{}, errors.New("official release metadata does not match the requested channel and tag")
	}
	var selected struct {
		Name, State, Digest, BrowserDownloadURL string
		Size                                    int64
	}
	for _, candidate := range release.Assets {
		if candidate.Name == asset {
			selected.Name, selected.State, selected.Size = candidate.Name, candidate.State, candidate.Size
			selected.Digest, selected.BrowserDownloadURL = candidate.Digest, candidate.BrowserDownloadURL
			break
		}
	}
	if selected.Name == "" || selected.State != "uploaded" || selected.Size <= 0 || selected.Size > maxAssetSize {
		return ReleaseBinary{}, errors.New("official release does not contain the expected bounded asset")
	}
	digest := strings.TrimPrefix(selected.Digest, "sha256:")
	if len(digest) != 64 {
		return ReleaseBinary{}, errors.New("official release asset has no SHA-256 digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ReleaseBinary{}, errors.New("official release asset has an invalid SHA-256 digest")
	}
	downloadURL, err := url.Parse(selected.BrowserDownloadURL)
	if err != nil || !s.allowURL(downloadURL) {
		return ReleaseBinary{}, errors.New("official release returned an unexpected download URL")
	}
	if s.apiBase == officialAPIBase {
		expectedPath := "/MetaCubeX/mihomo/releases/download/" + tag + "/" + asset
		if downloadURL.Host != "github.com" || downloadURL.Path != expectedPath {
			return ReleaseBinary{}, errors.New("official release asset URL does not match the built-in repository and version")
		}
	}
	downloadRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	downloadRequest.Header.Set("User-Agent", "submux-agent")
	download, err := s.httpClient.Do(downloadRequest)
	if err != nil {
		return ReleaseBinary{}, err
	}
	defer download.Body.Close()
	if download.StatusCode != http.StatusOK {
		return ReleaseBinary{}, fmt.Errorf("official asset download returned %d", download.StatusCode)
	}
	compressed, err := io.ReadAll(io.LimitReader(download.Body, maxAssetSize+1))
	if err != nil || len(compressed) > maxAssetSize || int64(len(compressed)) != selected.Size {
		return ReleaseBinary{}, errors.New("official asset size does not match release metadata")
	}
	sum := sha256.Sum256(compressed)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), digest) {
		return ReleaseBinary{}, errors.New("official asset SHA-256 verification failed")
	}
	binary, err := unpackReleaseBinary(asset, compressed)
	if err != nil {
		return ReleaseBinary{}, err
	}
	return ReleaseBinary{Version: version, AssetName: asset, AssetDigest: digest, Binary: binary}, nil
}

func releaseCoordinates(channel, version, osName, arch string) (string, string, error) {
	if (osName != "linux" && osName != "windows") || (arch != "amd64" && arch != "arm64") {
		return "", "", errors.New("Mihomo Agent supports only Linux/Windows amd64 and arm64")
	}
	assetName := func(version string) string {
		if osName == "linux" {
			return fmt.Sprintf("mihomo-linux-%s-%s.gz", arch, version)
		}
		variant := ""
		if arch == "amd64" {
			variant = "-compatible"
		}
		return fmt.Sprintf("mihomo-windows-%s%s-%s.zip", arch, variant, version)
	}
	switch channel {
	case "stable":
		if !stableVersionPattern.MatchString(version) || regexp.MustCompile(`(?i)(alpha|beta|rc|pre)`).MatchString(version) {
			return "", "", errors.New("stable channel requires an exact stable vX.Y.Z version")
		}
		return version, assetName(version), nil
	case "alpha":
		if !alphaVersionPattern.MatchString(version) {
			return "", "", errors.New("alpha channel requires an exact alpha-<commit> version")
		}
		return "Prerelease-Alpha", assetName(version), nil
	default:
		return "", "", errors.New("channel must be stable or alpha")
	}
}

func unpackReleaseBinary(asset string, compressed []byte) ([]byte, error) {
	if strings.HasSuffix(asset, ".gz") {
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, errors.New("official asset is not a valid gzip stream")
		}
		binary, readErr := io.ReadAll(io.LimitReader(reader, maxBinarySize+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || len(binary) == 0 || len(binary) > maxBinarySize {
			return nil, errors.New("decompressed Mihomo binary is invalid or too large")
		}
		return binary, nil
	}
	if strings.HasSuffix(asset, ".zip") {
		archive, err := zip.NewReader(bytes.NewReader(compressed), int64(len(compressed)))
		if err != nil || len(archive.File) != 1 {
			return nil, errors.New("official Windows asset must contain exactly one file")
		}
		entry := archive.File[0]
		if entry.FileInfo().IsDir() || strings.ContainsAny(entry.Name, `/\\`) || !strings.HasSuffix(strings.ToLower(entry.Name), ".exe") || entry.UncompressedSize64 > maxBinarySize {
			return nil, errors.New("official Windows asset contains an unexpected entry")
		}
		reader, err := entry.Open()
		if err != nil {
			return nil, errors.New("official Windows asset could not be opened")
		}
		binary, readErr := io.ReadAll(io.LimitReader(reader, maxBinarySize+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || len(binary) == 0 || len(binary) > maxBinarySize {
			return nil, errors.New("decompressed Mihomo binary is invalid or too large")
		}
		return binary, nil
	}
	return nil, errors.New("official release asset uses an unsupported archive format")
}

func officialDownloadURL(value *url.URL) bool {
	if value == nil || value.Scheme != "https" || value.User != nil {
		return false
	}
	host := strings.ToLower(value.Hostname())
	return host == "github.com" || host == "release-assets.githubusercontent.com" || host == "objects.githubusercontent.com"
}
