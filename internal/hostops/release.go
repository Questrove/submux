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
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	officialAPIBase        = "https://api.github.com/repos/MetaCubeX/mihomo"
	officialReleaseTimeout = 10 * time.Minute
	maxReleaseMetadataSize = 8 << 20
	maxAssetSize           = 100 << 20
	maxBinarySize          = 250 << 20
)

var (
	stableVersionPattern     = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$`)
	alphaVersionPattern      = regexp.MustCompile(`^alpha-[0-9a-f]{7,40}$`)
	alphaAssetVersionPattern = regexp.MustCompile(`alpha-[0-9a-f]{7,40}`)
	unstableStableTagPattern = regexp.MustCompile(`(?i)(alpha|beta|rc|pre)`)
)

type releaseAssetMetadata struct {
	Name               string `json:"name"`
	State              string `json:"state"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseMetadata struct {
	TagName    string                 `json:"tag_name"`
	Draft      bool                   `json:"draft"`
	Prerelease bool                   `json:"prerelease"`
	Assets     []releaseAssetMetadata `json:"assets"`
}

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
	progress   CoreProgressReporter
}

func OfficialReleaseSource(httpClient *http.Client) *releaseSource {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: officialReleaseTimeout}
	}
	clientCopy := *httpClient
	if clientCopy.Timeout == 0 {
		clientCopy.Timeout = officialReleaseTimeout
	}
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 5 || !officialDownloadURL(request.URL) {
			return errors.New("Mihomo asset redirected outside GitHub's official download hosts")
		}
		return nil
	}
	return &releaseSource{apiBase: officialAPIBase, httpClient: &clientCopy, allowURL: officialDownloadURL}
}

func officialReleaseHTTPClient(proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, errors.New("invalid Mihomo release proxy URL")
		}
		switch parsed.Scheme {
		case "http":
			transport.Proxy = http.ProxyURL(parsed)
		case "socks5":
			dialer, err := xproxy.SOCKS5("tcp", parsed.Host, nil, xproxy.Direct)
			if err != nil {
				return nil, errors.New("initialize Mihomo release SOCKS5 proxy")
			}
			if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
				transport.DialContext = contextDialer.DialContext
			} else {
				transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					type result struct {
						connection net.Conn
						err        error
					}
					ready := make(chan result, 1)
					go func() {
						connection, err := dialer.Dial(network, address)
						ready <- result{connection: connection, err: err}
					}()
					select {
					case value := <-ready:
						return value.connection, value.err
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
		default:
			return nil, errors.New("unsupported Mihomo release proxy scheme")
		}
	}
	return &http.Client{Transport: transport, Timeout: officialReleaseTimeout}, nil
}

func (s *releaseSource) SetProgressReporter(reporter CoreProgressReporter) {
	s.progress = reporter
}

func (s *releaseSource) report(value CoreProgress) {
	if s.progress != nil {
		s.progress(value)
	}
}

func (s *releaseSource) ListVersions(ctx context.Context, channel, osName, arch string, limit int) (versions []string, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Mihomo release list: %w", err)
		}
	}()
	if channel != "stable" && channel != "alpha" {
		return nil, errors.New("channel must be stable or alpha")
	}
	if limit < 1 || limit > 50 {
		return nil, errors.New("release version limit must be between 1 and 50")
	}
	probeVersion := "v0.0.0"
	if channel == "alpha" {
		probeVersion = "alpha-0000000"
	}
	if _, _, err := releaseCoordinates(channel, probeVersion, osName, arch); err != nil {
		return nil, err
	}
	s.report(CoreProgress{Phase: "listing_releases"})
	pageSize := limit + 1
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/releases?per_page=%d&page=1", strings.TrimRight(s.apiBase, "/"), pageSize), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "submux-agent")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official Mihomo release API returned %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseMetadataSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxReleaseMetadataSize {
		return nil, errors.New("official Mihomo release metadata exceeds its limit")
	}
	var releases []releaseMetadata
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, errors.New("official Mihomo release metadata is invalid")
	}
	seen := make(map[string]bool)
	for _, release := range releases {
		if release.Draft {
			continue
		}
		if channel == "stable" {
			version := release.TagName
			if release.Prerelease || !stableVersionPattern.MatchString(version) || unstableStableTagPattern.MatchString(version) {
				continue
			}
			_, expectedAsset, coordinateErr := releaseCoordinates(channel, version, osName, arch)
			if coordinateErr != nil || !hasInstallableReleaseAsset(release.Assets, expectedAsset) || seen[version] {
				continue
			}
			seen[version] = true
			versions = append(versions, version)
		} else {
			if !release.Prerelease || release.TagName != "Prerelease-Alpha" {
				continue
			}
			for _, asset := range release.Assets {
				version := alphaAssetVersionPattern.FindString(asset.Name)
				if version == "" || seen[version] {
					continue
				}
				_, expectedAsset, coordinateErr := releaseCoordinates(channel, version, osName, arch)
				if coordinateErr != nil || expectedAsset != asset.Name || !installableReleaseAsset(asset) {
					continue
				}
				seen[version] = true
				versions = append(versions, version)
			}
		}
		if len(versions) >= limit {
			break
		}
	}
	return versions, nil
}

func hasInstallableReleaseAsset(assets []releaseAssetMetadata, expectedName string) bool {
	for _, asset := range assets {
		if asset.Name == expectedName && installableReleaseAsset(asset) {
			return true
		}
	}
	return false
}

func installableReleaseAsset(asset releaseAssetMetadata) bool {
	digest := strings.TrimPrefix(asset.Digest, "sha256:")
	if asset.State != "uploaded" || asset.Size <= 0 || asset.Size > maxAssetSize || len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (s *releaseSource) Fetch(ctx context.Context, channel, version, osName, arch string) (ReleaseBinary, error) {
	s.report(CoreProgress{Phase: "resolving_release"})
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
	var release releaseMetadata
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
	s.report(CoreProgress{Phase: "downloading", BytesTotal: selected.Size})
	reader := &progressReader{reader: io.LimitReader(download.Body, maxAssetSize+1), total: selected.Size, report: s.progress}
	compressed, err := io.ReadAll(reader)
	if err != nil {
		return ReleaseBinary{}, fmt.Errorf("read official asset download: %w", err)
	}
	if len(compressed) > maxAssetSize || int64(len(compressed)) != selected.Size {
		return ReleaseBinary{}, errors.New("official asset size does not match release metadata")
	}
	s.report(CoreProgress{Phase: "verifying_download", BytesCompleted: int64(len(compressed)), BytesTotal: selected.Size})
	sum := sha256.Sum256(compressed)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), digest) {
		return ReleaseBinary{}, errors.New("official asset SHA-256 verification failed")
	}
	s.report(CoreProgress{Phase: "unpacking", BytesCompleted: int64(len(compressed)), BytesTotal: selected.Size})
	binary, err := unpackReleaseBinary(asset, compressed)
	if err != nil {
		return ReleaseBinary{}, err
	}
	return ReleaseBinary{Version: version, AssetName: asset, AssetDigest: digest, Binary: binary}, nil
}

type progressReader struct {
	reader     io.Reader
	total      int64
	completed  int64
	lastReport time.Time
	report     CoreProgressReporter
}

func (r *progressReader) Read(value []byte) (int, error) {
	n, err := r.reader.Read(value)
	r.completed += int64(n)
	now := time.Now()
	if r.report != nil && (r.lastReport.IsZero() || now.Sub(r.lastReport) >= 500*time.Millisecond || err == io.EOF) {
		r.lastReport = now
		completed := r.completed
		if r.total > 0 && completed > r.total {
			completed = r.total
		}
		r.report(CoreProgress{Phase: "downloading", BytesCompleted: completed, BytesTotal: r.total})
	}
	return n, err
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
		if !stableVersionPattern.MatchString(version) || unstableStableTagPattern.MatchString(version) {
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
