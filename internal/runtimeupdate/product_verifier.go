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
	"path"
	"path/filepath"
	"strings"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
	"github.com/theupdateframework/go-tuf/v2/metadata/updater"
)

const (
	TargetKindProduct    = "submux-runtime-product"
	ProductChannelStable = "stable"
	DefaultTargetsURL    = "https://raw.githubusercontent.com/Questrove/submux/tuf/targets/"
)

type ProductTarget struct {
	Path                string
	Version             string
	Platform            string
	Arch                string
	Channel             string
	AssetName           string
	Length              int64
	SHA256              string
	ReleaseNotes        string
	MigrationSummary    string
	RuntimeSchemaMin    int
	RuntimeSchemaMax    int
	RuntimeSchemaTarget int
	ProtocolMin         int
	ProtocolMax         int
	RequiredFreeBytes   int64
	Components          []string
	ConsistentSnapshot  bool
}

type productTargetCustom struct {
	Kind                string   `json:"kind"`
	Version             string   `json:"version"`
	Platform            string   `json:"platform"`
	Arch                string   `json:"arch"`
	Channel             string   `json:"channel"`
	AssetName           string   `json:"asset_name"`
	ReleaseNotes        string   `json:"release_notes"`
	MigrationSummary    string   `json:"migration_summary"`
	RuntimeSchemaMin    int      `json:"runtime_schema_min"`
	RuntimeSchemaMax    int      `json:"runtime_schema_max"`
	RuntimeSchemaTarget int      `json:"runtime_schema_target"`
	ProtocolMin         int      `json:"protocol_min"`
	ProtocolMax         int      `json:"protocol_max"`
	RequiredFreeBytes   int64    `json:"required_free_bytes"`
	Components          []string `json:"components"`
}

func (v *Verifier) RefreshProductOnline(
	ctx context.Context,
	platform string,
	arch string,
	version string,
) (ProductTarget, error) {
	if ctx == nil {
		return ProductTarget{}, errors.New("TUF product update context is required")
	}
	metadataURL := v.MetadataURL
	if metadataURL == "" {
		metadataURL = DefaultMetadataURL
	}
	download, err := newOnlineFetcher(ctx, metadataURL, v.HTTPClient)
	if err != nil {
		return ProductTarget{}, err
	}
	target, _, err := v.refreshProduct(
		ctx,
		download,
		metadataURL,
		"",
		platform,
		arch,
		version,
	)
	return target, err
}

func (v *Verifier) RefreshProductOffline(
	ctx context.Context,
	bundlePath string,
	platform string,
	arch string,
	version string,
) (ProductTarget, []byte, error) {
	if ctx == nil {
		return ProductTarget{}, nil, errors.New("TUF product update context is required")
	}
	download, err := newBundleFetcher(bundlePath)
	if err != nil {
		return ProductTarget{}, nil, err
	}
	defer download.Close()
	return v.refreshProduct(
		ctx,
		download,
		"https://offline.submux.invalid/metadata/",
		"https://offline.submux.invalid/targets/",
		platform,
		arch,
		version,
	)
}

func (v *Verifier) DownloadProduct(
	ctx context.Context,
	target ProductTarget,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("TUF product download context is required")
	}
	if err := validateProductTarget(target); err != nil {
		return nil, err
	}
	targetsURL := v.TargetsURL
	if targetsURL == "" {
		targetsURL = DefaultTargetsURL
	}
	download, err := newOnlineFetcher(ctx, targetsURL, v.HTTPClient)
	if err != nil {
		return nil, err
	}
	targetPath := target.Path
	if target.ConsistentSnapshot {
		directory, name := path.Split(targetPath)
		targetPath = path.Join(directory, target.SHA256+"."+name)
	}
	requestURL := strings.TrimSuffix(targetsURL, "/") + "/" + targetPath
	body, err := download.DownloadFile(requestURL, target.Length, 0)
	if err != nil {
		return nil, fmt.Errorf("download signed Runtime product target: %w", err)
	}
	if err := v.VerifyProductTarget(target, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (v *Verifier) VerifyProductTarget(target ProductTarget, body []byte) error {
	if err := validateProductTarget(target); err != nil {
		return err
	}
	if int64(len(body)) != target.Length {
		return errors.New("TUF Runtime product target size does not match signed metadata")
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), target.SHA256) {
		return errors.New("TUF Runtime product target SHA-256 does not match signed metadata")
	}
	return nil
}

func (v *Verifier) refreshProduct(
	ctx context.Context,
	download fetcher.Fetcher,
	metadataURL string,
	targetURL string,
	platform string,
	arch string,
	version string,
) (ProductTarget, []byte, error) {
	if err := ctx.Err(); err != nil {
		return ProductTarget{}, nil, err
	}
	if v == nil || len(v.InitialRoot) == 0 || len(v.InitialRoot) > maxAcceptedRootBytes {
		return ProductTarget{}, nil, errors.New("embedded initial TUF Root is unavailable or invalid")
	}
	if v.StateRoot == "" || !filepath.IsAbs(v.StateRoot) {
		return ProductTarget{}, nil, errors.New("TUF state root must use a fixed absolute path")
	}
	if platform != "linux" && platform != "windows" && platform != "darwin" {
		return ProductTarget{}, nil, errors.New("TUF product target platform is unsupported")
	}
	if arch != "amd64" && arch != "arm64" {
		return ProductTarget{}, nil, errors.New("TUF product target architecture is unsupported")
	}
	if version != "" && !stableVersion.MatchString(version) {
		return ProductTarget{}, nil, errors.New("TUF product target version must be an exact stable vX.Y.Z version")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if err := prepareStateRoot(v.StateRoot); err != nil {
		return ProductTarget{}, nil, err
	}
	cfg, trustedRoot, err := v.updaterConfig(download, metadataURL)
	if err != nil {
		return ProductTarget{}, nil, err
	}
	cfg.LocalTrustedRoot = trustedRoot
	update, err := updater.New(cfg)
	if err != nil {
		return ProductTarget{}, nil, fmt.Errorf("initialize TUF updater: %w", err)
	}
	if v.Now != nil {
		update.UnsafeSetRefTime(v.Now().UTC())
	}
	if err := update.Refresh(); err != nil {
		return ProductTarget{}, nil, fmt.Errorf("refresh TUF metadata: %w", err)
	}
	trusted := update.GetTrustedMetadataSet()
	versions, err := versionsFromTrusted(trusted)
	if err != nil {
		return ProductTarget{}, nil, err
	}
	if err := v.rejectRollback(versions); err != nil {
		return ProductTarget{}, nil, err
	}
	if err := v.persistAcceptedRoot(trusted); err != nil {
		return ProductTarget{}, nil, err
	}
	if err := v.persistVersions(versions); err != nil {
		return ProductTarget{}, nil, err
	}
	if err := restrictMetadataFiles(filepath.Join(v.StateRoot, "metadata")); err != nil {
		return ProductTarget{}, nil, err
	}
	target, info, err := selectProductTarget(
		update.GetTopLevelTargets(),
		platform,
		arch,
		version,
	)
	if err != nil {
		return ProductTarget{}, nil, err
	}
	target.ConsistentSnapshot = trusted.Root.Signed.ConsistentSnapshot
	if targetURL == "" {
		return target, nil, nil
	}
	targetPath := target.Path
	if target.ConsistentSnapshot {
		directory, name := path.Split(targetPath)
		targetPath = path.Join(directory, target.SHA256+"."+name)
	}
	targetRequestURL := strings.TrimSuffix(targetURL, "/") + "/" + targetPath
	body, err := download.DownloadFile(targetRequestURL, target.Length, 0)
	if err != nil {
		return ProductTarget{}, nil, fmt.Errorf("verify offline TUF Runtime product target: %w", err)
	}
	if err := info.VerifyLengthHashes(body); err != nil {
		return ProductTarget{}, nil, fmt.Errorf("verify offline TUF Runtime product target: %w", err)
	}
	if err := v.VerifyProductTarget(target, body); err != nil {
		return ProductTarget{}, nil, err
	}
	return target, body, nil
}

func selectProductTarget(
	targets map[string]*metadata.TargetFiles,
	platform string,
	arch string,
	requestedVersion string,
) (ProductTarget, *metadata.TargetFiles, error) {
	var selected ProductTarget
	var selectedInfo *metadata.TargetFiles
	for targetPath, info := range targets {
		target, err := decodeProductTarget(targetPath, info)
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
		return ProductTarget{}, nil, errors.New("no signed stable Runtime product target matches this platform and architecture")
	}
	return selected, selectedInfo, nil
}

func decodeProductTarget(targetPath string, info *metadata.TargetFiles) (ProductTarget, error) {
	if info == nil || info.Custom == nil {
		return ProductTarget{}, errors.New("TUF Runtime product target has no signed custom metadata")
	}
	var custom productTargetCustom
	decoder := json.NewDecoder(bytes.NewReader(*info.Custom))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&custom); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ProductTarget{}, errors.New("TUF Runtime product target custom metadata is invalid")
	}
	target := ProductTarget{
		Path:                targetPath,
		Version:             custom.Version,
		Platform:            custom.Platform,
		Arch:                custom.Arch,
		Channel:             custom.Channel,
		AssetName:           custom.AssetName,
		ReleaseNotes:        custom.ReleaseNotes,
		MigrationSummary:    custom.MigrationSummary,
		RuntimeSchemaMin:    custom.RuntimeSchemaMin,
		RuntimeSchemaMax:    custom.RuntimeSchemaMax,
		RuntimeSchemaTarget: custom.RuntimeSchemaTarget,
		ProtocolMin:         custom.ProtocolMin,
		ProtocolMax:         custom.ProtocolMax,
		RequiredFreeBytes:   custom.RequiredFreeBytes,
		Components:          append([]string(nil), custom.Components...),
	}
	if info.Length <= 0 || len(info.Hashes) != 1 {
		return ProductTarget{}, errors.New("TUF Runtime product target must have one SHA-256 digest and a positive size")
	}
	digest, ok := info.Hashes["sha256"]
	if !ok || len(digest) != sha256.Size {
		return ProductTarget{}, errors.New("TUF Runtime product target SHA-256 is unavailable")
	}
	target.Length = info.Length
	target.SHA256 = strings.ToLower(hex.EncodeToString(digest))
	if err := validateProductTarget(target); err != nil {
		return ProductTarget{}, err
	}
	expected := path.Join("product", target.Version, target.Platform, target.Arch, target.AssetName)
	if targetPath != expected {
		return ProductTarget{}, errors.New("TUF Runtime product target path is outside policy")
	}
	return target, nil
}

func validateProductTarget(target ProductTarget) error {
	if target.Path == "" ||
		target.Channel != ProductChannelStable ||
		!stableVersion.MatchString(target.Version) ||
		(target.Platform != "linux" && target.Platform != "windows" && target.Platform != "darwin") ||
		(target.Arch != "amd64" && target.Arch != "arm64") ||
		target.AssetName == "" ||
		strings.ContainsAny(target.AssetName, `/\`) ||
		target.Length <= 0 ||
		!validSHA256(target.SHA256) ||
		target.RuntimeSchemaMin <= 0 ||
		target.RuntimeSchemaMax < target.RuntimeSchemaMin ||
		target.RuntimeSchemaTarget < target.RuntimeSchemaMin ||
		target.RuntimeSchemaTarget > target.RuntimeSchemaMax ||
		target.ProtocolMin <= 0 ||
		target.ProtocolMax < target.ProtocolMin ||
		target.RequiredFreeBytes < target.Length ||
		len(target.ReleaseNotes) == 0 ||
		len(target.ReleaseNotes) > 32<<10 ||
		len(target.MigrationSummary) > 8<<10 ||
		!validProductComponents(target.Components) {
		return errors.New("TUF Runtime product target custom metadata is outside policy")
	}
	return nil
}

func validProductComponents(components []string) bool {
	if len(components) != 3 {
		return false
	}
	required := map[string]bool{
		"submux-runtime":     false,
		"submux-runtime-net": false,
		"submux-runtime-gui": false,
	}
	for _, component := range components {
		if _, ok := required[component]; !ok || required[component] {
			return false
		}
		required[component] = true
	}
	for _, present := range required {
		if !present {
			return false
		}
	}
	return true
}
