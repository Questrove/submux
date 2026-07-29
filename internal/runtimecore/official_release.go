package runtimecore

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
	"strings"
	"time"
)

const (
	officialAPIBase        = "https://api.github.com/repos/MetaCubeX/mihomo"
	officialReleaseTimeout = 10 * time.Minute
	maxReleaseMetadataSize = 8 << 20
	maxAssetSize           = 100 << 20
	maxBinarySize          = 250 << 20
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
	Version      string
	AssetName    string
	AssetDigest  string
	BinaryDigest string
	Data         []byte
}

type ReleaseProgress struct {
	Phase          string
	BytesCompleted int64
	BytesTotal     int64
}

type ReleaseProgressReporter func(ReleaseProgress)

// OfficialReleaseSource validates the fixed MetaCubeX/mihomo repository,
// exact release tag, exact platform asset, bounded sizes and GitHub-provided
// digest. This is supplemental upstream verification; it does not replace the
// TUF trust required by the Runtime distribution design.
type OfficialReleaseSource struct {
	apiBase  string
	client   *http.Client
	allowURL func(*url.URL) bool
	progress ReleaseProgressReporter
}

func NewOfficialReleaseSource(client *http.Client) *OfficialReleaseSource {
	if client == nil {
		client = &http.Client{Timeout: officialReleaseTimeout}
	}
	clientCopy := *client
	if clientCopy.Timeout == 0 {
		clientCopy.Timeout = officialReleaseTimeout
	}
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 5 || !officialDownloadURL(request.URL) {
			return errors.New("Mihomo asset redirected outside GitHub official download hosts")
		}
		return nil
	}
	return &OfficialReleaseSource{
		apiBase:  officialAPIBase,
		client:   &clientCopy,
		allowURL: officialDownloadURL,
	}
}

func (s *OfficialReleaseSource) SetProgressReporter(reporter ReleaseProgressReporter) {
	s.progress = reporter
}

func (s *OfficialReleaseSource) FetchStable(ctx context.Context, version, osName, arch string) (ReleaseBinary, error) {
	tag, asset, err := releaseCoordinates(version, osName, arch)
	if err != nil {
		return ReleaseBinary{}, err
	}
	if s == nil || s.client == nil || s.apiBase == "" || s.allowURL == nil {
		return ReleaseBinary{}, errors.New("official Mihomo release source is incomplete")
	}
	s.report(ReleaseProgress{Phase: "resolving_release"})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.apiBase, "/")+"/releases/tags/"+url.PathEscape(tag), nil)
	if err != nil {
		return ReleaseBinary{}, err
	}
	setReleaseHeaders(request)
	response, err := s.client.Do(request)
	if err != nil {
		return ReleaseBinary{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ReleaseBinary{}, fmt.Errorf("official Mihomo release API returned %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseMetadataSize+1))
	if err != nil {
		return ReleaseBinary{}, fmt.Errorf("read official release metadata: %w", err)
	}
	if len(raw) > maxReleaseMetadataSize {
		return ReleaseBinary{}, errors.New("official Mihomo release metadata exceeds its limit")
	}
	var release releaseMetadata
	if err := json.Unmarshal(raw, &release); err != nil {
		return ReleaseBinary{}, errors.New("official Mihomo release metadata is invalid")
	}
	if release.TagName != tag || release.Draft || release.Prerelease {
		return ReleaseBinary{}, errors.New("official release metadata does not match the requested stable tag")
	}
	var selected releaseAssetMetadata
	for _, candidate := range release.Assets {
		if candidate.Name == asset {
			selected = candidate
			break
		}
	}
	if selected.Name == "" || selected.State != "uploaded" || selected.Size <= 0 || selected.Size > maxAssetSize {
		return ReleaseBinary{}, errors.New("official release does not contain the expected bounded asset")
	}
	assetDigest := strings.TrimPrefix(selected.Digest, "sha256:")
	if len(assetDigest) != 64 {
		return ReleaseBinary{}, errors.New("official release asset has no SHA-256 digest")
	}
	if _, err := hex.DecodeString(assetDigest); err != nil {
		return ReleaseBinary{}, errors.New("official release asset has an invalid SHA-256 digest")
	}
	downloadURL, err := url.Parse(selected.BrowserDownloadURL)
	if err != nil || !s.allowURL(downloadURL) {
		return ReleaseBinary{}, errors.New("official release returned an unexpected download URL")
	}
	if s.apiBase == officialAPIBase {
		expectedPath := "/MetaCubeX/mihomo/releases/download/" + tag + "/" + asset
		if downloadURL.Host != "github.com" || downloadURL.Path != expectedPath {
			return ReleaseBinary{}, errors.New("official release asset URL does not match the fixed repository and version")
		}
	}

	downloadRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		return ReleaseBinary{}, err
	}
	downloadRequest.Header.Set("User-Agent", "submux-runtime")
	download, err := s.client.Do(downloadRequest)
	if err != nil {
		return ReleaseBinary{}, err
	}
	defer download.Body.Close()
	if download.StatusCode != http.StatusOK {
		return ReleaseBinary{}, fmt.Errorf("official Mihomo asset download returned %d", download.StatusCode)
	}
	s.report(ReleaseProgress{Phase: "downloading", BytesTotal: selected.Size})
	reader := &progressReader{
		reader: io.LimitReader(download.Body, maxAssetSize+1),
		total:  selected.Size,
		report: s.progress,
	}
	compressed, err := io.ReadAll(reader)
	if err != nil {
		return ReleaseBinary{}, fmt.Errorf("read official Mihomo asset: %w", err)
	}
	if len(compressed) > maxAssetSize || int64(len(compressed)) != selected.Size {
		return ReleaseBinary{}, errors.New("official Mihomo asset size does not match release metadata")
	}
	s.report(ReleaseProgress{Phase: "verifying_download", BytesCompleted: int64(len(compressed)), BytesTotal: selected.Size})
	sum := sha256.Sum256(compressed)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), assetDigest) {
		return ReleaseBinary{}, errors.New("official Mihomo asset SHA-256 verification failed")
	}
	s.report(ReleaseProgress{Phase: "unpacking", BytesCompleted: int64(len(compressed)), BytesTotal: selected.Size})
	binary, err := unpackReleaseBinary(asset, compressed)
	if err != nil {
		return ReleaseBinary{}, err
	}
	binarySum := sha256.Sum256(binary)
	return ReleaseBinary{
		Version:      version,
		AssetName:    asset,
		AssetDigest:  assetDigest,
		BinaryDigest: hex.EncodeToString(binarySum[:]),
		Data:         binary,
	}, nil
}

func (s *OfficialReleaseSource) report(value ReleaseProgress) {
	if s.progress != nil {
		s.progress(value)
	}
}

func releaseCoordinates(version, osName, arch string) (string, string, error) {
	lowerVersion := strings.ToLower(version)
	if !stableVersionPattern.MatchString(version) ||
		strings.Contains(lowerVersion, "alpha") ||
		strings.Contains(lowerVersion, "beta") ||
		strings.Contains(lowerVersion, "rc") ||
		strings.Contains(lowerVersion, "pre") {
		return "", "", errors.New("stable release requires an exact stable vX.Y.Z version")
	}
	if arch != "amd64" && arch != "arm64" {
		return "", "", errors.New("Mihomo Runtime supports only amd64 and arm64")
	}
	switch osName {
	case "linux":
		return version, fmt.Sprintf("mihomo-linux-%s-%s.gz", arch, version), nil
	case "windows":
		variant := ""
		if arch == "amd64" {
			variant = "-compatible"
		}
		return version, fmt.Sprintf("mihomo-windows-%s%s-%s.zip", arch, variant, version), nil
	case "darwin":
		variant := ""
		if arch == "amd64" {
			variant = "-compatible"
		}
		return version, fmt.Sprintf("mihomo-darwin-%s%s-%s.gz", arch, variant, version), nil
	default:
		return "", "", errors.New("Mihomo Runtime supports only Linux, Windows and macOS")
	}
}

func unpackReleaseBinary(asset string, compressed []byte) ([]byte, error) {
	if strings.HasSuffix(asset, ".gz") {
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, errors.New("official Mihomo asset is not a valid gzip stream")
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
			return nil, errors.New("official Windows Mihomo asset must contain exactly one file")
		}
		entry := archive.File[0]
		if entry.FileInfo().IsDir() || strings.ContainsAny(entry.Name, `/\`) || !strings.HasSuffix(strings.ToLower(entry.Name), ".exe") || entry.UncompressedSize64 > maxBinarySize {
			return nil, errors.New("official Windows Mihomo asset contains an unexpected entry")
		}
		reader, err := entry.Open()
		if err != nil {
			return nil, errors.New("official Windows Mihomo asset could not be opened")
		}
		binary, readErr := io.ReadAll(io.LimitReader(reader, maxBinarySize+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || len(binary) == 0 || len(binary) > maxBinarySize {
			return nil, errors.New("decompressed Mihomo binary is invalid or too large")
		}
		return binary, nil
	}
	return nil, errors.New("official Mihomo release asset uses an unsupported archive format")
}

func officialDownloadURL(value *url.URL) bool {
	if value == nil || value.Scheme != "https" || value.User != nil {
		return false
	}
	host := strings.ToLower(value.Hostname())
	return host == "github.com" || host == "release-assets.githubusercontent.com" || host == "objects.githubusercontent.com"
}

func setReleaseHeaders(request *http.Request) {
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "submux-runtime")
}

type progressReader struct {
	reader     io.Reader
	total      int64
	completed  int64
	lastReport time.Time
	report     ReleaseProgressReporter
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
		r.report(ReleaseProgress{Phase: "downloading", BytesCompleted: completed, BytesTotal: r.total})
	}
	return n, err
}
