package runtimeupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/config"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
	"github.com/theupdateframework/go-tuf/v2/metadata/trustedmetadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/updater"

	"submux/internal/safepath"
)

const (
	OfficialRepository     = "MetaCubeX/mihomo"
	TargetKindMihomo       = "mihomo"
	DefaultMetadataURL     = "https://raw.githubusercontent.com/Questrove/submux/tuf/metadata/"
	DefaultMetadataTimeout = 2 * time.Minute

	maxAcceptedRootBytes = 512 << 10
)

var stableVersion = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

type Target struct {
	Path           string
	Version        string
	Platform       string
	Arch           string
	Repository     string
	AssetName      string
	UpstreamSHA256 string
	Length         int64
	SHA256         string
}

type targetCustom struct {
	Kind           string `json:"kind"`
	Version        string `json:"version"`
	Platform       string `json:"platform"`
	Arch           string `json:"arch"`
	Repository     string `json:"repository"`
	AssetName      string `json:"asset_name"`
	UpstreamSHA256 string `json:"upstream_sha256"`
}

type acceptedVersions struct {
	Root      int64 `json:"root"`
	Timestamp int64 `json:"timestamp"`
	Snapshot  int64 `json:"snapshot"`
	Targets   int64 `json:"targets"`
}

type Verifier struct {
	InitialRoot []byte
	StateRoot   string
	MetadataURL string
	HTTPClient  *http.Client
	Now         func() time.Time

	mu sync.Mutex
}

func (v *Verifier) RefreshOnline(
	ctx context.Context,
	platform string,
	arch string,
	version string,
) (Target, error) {
	if ctx == nil {
		return Target{}, errors.New("TUF update context is required")
	}
	metadataURL := v.MetadataURL
	if metadataURL == "" {
		metadataURL = DefaultMetadataURL
	}
	download, err := newOnlineFetcher(ctx, metadataURL, v.HTTPClient)
	if err != nil {
		return Target{}, err
	}
	target, _, err := v.refresh(ctx, download, metadataURL, "", platform, arch, version)
	return target, err
}

func (v *Verifier) RefreshOffline(
	ctx context.Context,
	bundlePath string,
	platform string,
	arch string,
	version string,
) (Target, []byte, error) {
	if ctx == nil {
		return Target{}, nil, errors.New("TUF update context is required")
	}
	download, err := newBundleFetcher(bundlePath)
	if err != nil {
		return Target{}, nil, err
	}
	defer download.Close()

	const metadataURL = "https://offline.submux.invalid/metadata/"
	return v.refresh(
		ctx,
		download,
		metadataURL,
		"https://offline.submux.invalid/targets/",
		platform,
		arch,
		version,
	)
}

func (v *Verifier) VerifyTarget(target Target, body []byte) error {
	if target.Length <= 0 || int64(len(body)) != target.Length {
		return errors.New("TUF target size does not match signed metadata")
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), target.SHA256) {
		return errors.New("TUF target SHA-256 does not match signed metadata")
	}
	if !strings.EqualFold(target.SHA256, target.UpstreamSHA256) {
		return errors.New("TUF target and upstream release SHA-256 disagree")
	}
	return nil
}

func (v *Verifier) refresh(
	ctx context.Context,
	download fetcher.Fetcher,
	metadataURL string,
	targetURL string,
	platform string,
	arch string,
	version string,
) (Target, []byte, error) {
	if err := ctx.Err(); err != nil {
		return Target{}, nil, err
	}
	if v == nil || len(v.InitialRoot) == 0 || len(v.InitialRoot) > maxAcceptedRootBytes {
		return Target{}, nil, errors.New("embedded initial TUF Root is unavailable or invalid")
	}
	if v.StateRoot == "" || !filepath.IsAbs(v.StateRoot) {
		return Target{}, nil, errors.New("TUF state root must use a fixed absolute path")
	}
	if platform != "linux" && platform != "windows" && platform != "darwin" {
		return Target{}, nil, errors.New("TUF target platform is unsupported")
	}
	if arch != "amd64" && arch != "arm64" {
		return Target{}, nil, errors.New("TUF target architecture is unsupported")
	}
	if version != "" && !stableVersion.MatchString(version) {
		return Target{}, nil, errors.New("TUF target version must be an exact stable vX.Y.Z version")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if err := prepareStateRoot(v.StateRoot); err != nil {
		return Target{}, nil, err
	}
	cfg, trustedRoot, err := v.updaterConfig(download, metadataURL)
	if err != nil {
		return Target{}, nil, err
	}
	cfg.LocalTrustedRoot = trustedRoot
	update, err := updater.New(cfg)
	if err != nil {
		return Target{}, nil, fmt.Errorf("initialize TUF updater: %w", err)
	}
	if v.Now != nil {
		update.UnsafeSetRefTime(v.Now().UTC())
	}
	if err := update.Refresh(); err != nil {
		return Target{}, nil, fmt.Errorf("refresh TUF metadata: %w", err)
	}
	trusted := update.GetTrustedMetadataSet()
	versions, err := versionsFromTrusted(trusted)
	if err != nil {
		return Target{}, nil, err
	}
	if err := v.rejectRollback(versions); err != nil {
		return Target{}, nil, err
	}
	if err := v.persistAcceptedRoot(trusted); err != nil {
		return Target{}, nil, err
	}
	if err := v.persistVersions(versions); err != nil {
		return Target{}, nil, err
	}
	if err := restrictMetadataFiles(filepath.Join(v.StateRoot, "metadata")); err != nil {
		return Target{}, nil, err
	}
	target, info, err := selectMihomoTarget(update.GetTopLevelTargets(), platform, arch, version)
	if err != nil {
		return Target{}, nil, err
	}
	if targetURL == "" {
		return target, nil, nil
	}
	targetPath := target.Path
	if trusted.Root.Signed.ConsistentSnapshot {
		directory, name := path.Split(targetPath)
		targetPath = path.Join(directory, target.SHA256+"."+name)
	}
	targetRequestURL := strings.TrimSuffix(targetURL, "/") + "/" + targetPath
	body, err := download.DownloadFile(targetRequestURL, target.Length, 0)
	if err != nil {
		return Target{}, nil, fmt.Errorf("verify offline TUF target: %w", err)
	}
	if err := info.VerifyLengthHashes(body); err != nil {
		return Target{}, nil, fmt.Errorf("verify offline TUF target: %w", err)
	}
	return target, body, nil
}

func (v *Verifier) updaterConfig(
	download fetcher.Fetcher,
	metadataURL string,
) (*config.UpdaterConfig, []byte, error) {
	root, err := v.latestTrustedRoot()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.New(metadataURL, root)
	if err != nil {
		return nil, nil, err
	}
	cfg.Fetcher = download
	cfg.LocalMetadataDir = filepath.Join(v.StateRoot, "metadata")
	cfg.LocalTargetsDir = filepath.Join(v.StateRoot, "targets")
	cfg.DisableLocalCache = false
	cfg.PrefixTargetsWithHash = true
	cfg.MaxRootRotations = 64
	return cfg, root, nil
}

func (v *Verifier) latestTrustedRoot() ([]byte, error) {
	trusted, err := trustedmetadata.New(v.InitialRoot)
	if err != nil {
		return nil, fmt.Errorf("parse embedded initial TUF Root: %w", err)
	}
	current := append([]byte(nil), v.InitialRoot...)
	rootDir := filepath.Join(v.StateRoot, "roots")
	for next := trusted.Root.Signed.Version + 1; ; next++ {
		name := filepath.Join(rootDir, strconv.FormatInt(next, 10)+".root.json")
		body, readErr := os.ReadFile(name)
		if errors.Is(readErr, os.ErrNotExist) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read accepted TUF Root %d: %w", next, readErr)
		}
		if len(body) == 0 || len(body) > maxAcceptedRootBytes {
			return nil, fmt.Errorf("accepted TUF Root %d has an invalid size", next)
		}
		if _, err := trusted.UpdateRoot(body); err != nil {
			return nil, fmt.Errorf("verify accepted TUF Root %d: %w", next, err)
		}
		current = body
	}
	return current, nil
}

func (v *Verifier) persistAcceptedRoot(trusted trustedmetadata.TrustedMetadata) error {
	if trusted.Root == nil || trusted.Root.Signed.Version <= 0 {
		return errors.New("TUF updater returned no trusted Root")
	}
	body, err := trusted.Root.MarshalJSON()
	if err != nil {
		return fmt.Errorf("encode accepted TUF Root: %w", err)
	}
	rootDir := filepath.Join(v.StateRoot, "roots")
	if err := preparePrivateDir(rootDir); err != nil {
		return err
	}
	name := filepath.Join(rootDir, strconv.FormatInt(trusted.Root.Signed.Version, 10)+".root.json")
	if existing, err := os.ReadFile(name); err == nil {
		if !equalDigest(existing, body) {
			return errors.New("accepted TUF Root history conflicts with the verified Root")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writePrivateAtomic(name, body)
}

func (v *Verifier) rejectRollback(candidate acceptedVersions) error {
	accepted, err := readAcceptedVersions(filepath.Join(v.StateRoot, "accepted-versions.json"))
	if err != nil {
		return err
	}
	if candidate.Root < accepted.Root ||
		candidate.Timestamp < accepted.Timestamp ||
		candidate.Snapshot < accepted.Snapshot ||
		candidate.Targets < accepted.Targets {
		return errors.New("TUF metadata rollback was rejected")
	}
	return nil
}

func (v *Verifier) persistVersions(versions acceptedVersions) error {
	body, err := json.Marshal(versions)
	if err != nil {
		return err
	}
	return writePrivateAtomic(filepath.Join(v.StateRoot, "accepted-versions.json"), append(body, '\n'))
}

func versionsFromTrusted(trusted trustedmetadata.TrustedMetadata) (acceptedVersions, error) {
	if trusted.Root == nil || trusted.Timestamp == nil || trusted.Snapshot == nil ||
		trusted.Targets[metadata.TARGETS] == nil {
		return acceptedVersions{}, errors.New("TUF updater did not return the complete trusted metadata set")
	}
	return acceptedVersions{
		Root:      trusted.Root.Signed.Version,
		Timestamp: trusted.Timestamp.Signed.Version,
		Snapshot:  trusted.Snapshot.Signed.Version,
		Targets:   trusted.Targets[metadata.TARGETS].Signed.Version,
	}, nil
}

func selectMihomoTarget(
	targets map[string]*metadata.TargetFiles,
	platform string,
	arch string,
	requestedVersion string,
) (Target, *metadata.TargetFiles, error) {
	var selected Target
	var selectedInfo *metadata.TargetFiles
	for targetPath, info := range targets {
		target, err := decodeTarget(targetPath, info)
		if err != nil {
			continue
		}
		if target.Platform != platform || target.Arch != arch ||
			requestedVersion != "" && target.Version != requestedVersion {
			continue
		}
		if selectedInfo == nil || compareStable(target.Version, selected.Version) > 0 {
			selected = target
			selectedInfo = info
		}
	}
	if selectedInfo == nil {
		return Target{}, nil, errors.New("no signed stable Mihomo target matches this platform and architecture")
	}
	return selected, selectedInfo, nil
}

func decodeTarget(targetPath string, info *metadata.TargetFiles) (Target, error) {
	if info == nil || info.Custom == nil {
		return Target{}, errors.New("TUF Mihomo target has no signed custom metadata")
	}
	var custom targetCustom
	decoder := json.NewDecoder(bytes.NewReader(*info.Custom))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&custom); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Target{}, errors.New("TUF Mihomo target custom metadata is invalid")
	}
	if custom.Kind != TargetKindMihomo ||
		custom.Repository != OfficialRepository ||
		!stableVersion.MatchString(custom.Version) ||
		(custom.Platform != "linux" && custom.Platform != "windows" && custom.Platform != "darwin") ||
		(custom.Arch != "amd64" && custom.Arch != "arm64") ||
		!validSHA256(custom.UpstreamSHA256) {
		return Target{}, errors.New("TUF Mihomo target custom metadata is outside policy")
	}
	expected := path.Join("mihomo", custom.Version, custom.Platform, custom.Arch, custom.AssetName)
	if targetPath != expected || strings.ContainsAny(custom.AssetName, `/\`) {
		return Target{}, errors.New("TUF Mihomo target path is outside policy")
	}
	if info.Length <= 0 || len(info.Hashes) != 1 {
		return Target{}, errors.New("TUF Mihomo target must have one SHA-256 digest and a positive size")
	}
	digest, ok := info.Hashes["sha256"]
	if !ok || len(digest) != sha256.Size {
		return Target{}, errors.New("TUF Mihomo target SHA-256 is unavailable")
	}
	targetDigest := hex.EncodeToString(digest)
	if !strings.EqualFold(targetDigest, custom.UpstreamSHA256) {
		return Target{}, errors.New("TUF Mihomo target does not bind the upstream release digest")
	}
	return Target{
		Path:           targetPath,
		Version:        custom.Version,
		Platform:       custom.Platform,
		Arch:           custom.Arch,
		Repository:     custom.Repository,
		AssetName:      custom.AssetName,
		UpstreamSHA256: strings.ToLower(custom.UpstreamSHA256),
		Length:         info.Length,
		SHA256:         strings.ToLower(targetDigest),
	}, nil
}

func compareStable(left, right string) int {
	leftParts := stableVersion.FindStringSubmatch(left)
	rightParts := stableVersion.FindStringSubmatch(right)
	for index := 1; index <= 3; index++ {
		leftNumber, _ := strconv.ParseUint(leftParts[index], 10, 64)
		rightNumber, _ := strconv.ParseUint(rightParts[index], 10, 64)
		if leftNumber < rightNumber {
			return -1
		}
		if leftNumber > rightNumber {
			return 1
		}
	}
	return 0
}

func readAcceptedVersions(name string) (acceptedVersions, error) {
	body, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return acceptedVersions{}, nil
	}
	if err != nil {
		return acceptedVersions{}, err
	}
	var versions acceptedVersions
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&versions); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return acceptedVersions{}, errors.New("accepted TUF metadata versions are invalid")
	}
	return versions, nil
}

func prepareStateRoot(root string) error {
	if err := preparePrivateDir(root); err != nil {
		return err
	}
	for _, name := range []string{"metadata", "targets", "roots"} {
		if err := preparePrivateDir(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	return nil
}

func preparePrivateDir(name string) error {
	if err := os.MkdirAll(name, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("TUF state path must be a real directory")
	}
	linked, err := safepath.ContainsLink(name)
	if err != nil {
		return fmt.Errorf("inspect TUF state path: %w", err)
	}
	if linked {
		return errors.New("TUF state path must not contain symbolic or reparse links")
	}
	return os.Chmod(name, 0700)
}

func restrictMetadataFiles(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("TUF metadata cache contains an unexpected entry")
		}
		if err := os.Chmod(filepath.Join(root, entry.Name()), 0600); err != nil {
			return err
		}
	}
	return nil
}

func writePrivateAtomic(name string, body []byte) error {
	dir := filepath.Dir(name)
	if err := preparePrivateDir(dir); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".tuf-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return replaceFile(temp, name)
}

func equalDigest(left, right []byte) bool {
	leftSum := sha256.Sum256(left)
	rightSum := sha256.Sum256(right)
	return leftSum == rightSum
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type onlineFetcher struct {
	ctx    context.Context
	base   *url.URL
	client *http.Client
}

func newOnlineFetcher(ctx context.Context, base string, client *http.Client) (*onlineFetcher, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("TUF metadata URL must be a fixed HTTPS URL")
	}
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	if client == nil {
		client = &http.Client{Timeout: DefaultMetadataTimeout}
	}
	copyClient := *client
	if copyClient.Timeout == 0 {
		copyClient.Timeout = DefaultMetadataTimeout
	}
	fetch := &onlineFetcher{ctx: ctx, base: parsed, client: &copyClient}
	copyClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 5 || !fetch.allowed(request.URL) {
			return errors.New("TUF metadata redirected outside the fixed repository")
		}
		return nil
	}
	return fetch, nil
}

func (f *onlineFetcher) DownloadFile(urlPath string, maxLength int64, _ time.Duration) ([]byte, error) {
	parsed, err := url.Parse(urlPath)
	if err != nil || !f.allowed(parsed) {
		return nil, errors.New("TUF metadata request is outside the fixed repository")
	}
	request, err := http.NewRequestWithContext(f.ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "submux-runtime")
	response, err := f.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: response.StatusCode, URL: parsed.String()}
	}
	if maxLength <= 0 {
		return nil, errors.New("TUF metadata maximum length is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxLength+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxLength {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: "download exceeded signed or configured maximum length"}
	}
	return body, nil
}

func (f *onlineFetcher) allowed(candidate *url.URL) bool {
	return candidate != nil &&
		candidate.Scheme == f.base.Scheme &&
		strings.EqualFold(candidate.Host, f.base.Host) &&
		candidate.User == nil &&
		candidate.RawQuery == "" &&
		candidate.Fragment == "" &&
		strings.HasPrefix(candidate.EscapedPath(), f.base.EscapedPath())
}
