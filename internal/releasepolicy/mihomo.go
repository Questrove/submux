package releasepolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	stableMihomoVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitSHA           = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestSHA256        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type MihomoProvenance struct {
	Schema        int                   `json:"schema"`
	Repository    string                `json:"repository"`
	Version       string                `json:"version"`
	Tag           string                `json:"tag"`
	Commit        string                `json:"commit"`
	License       string                `json:"license"`
	LicenseURL    string                `json:"license_url"`
	SourceArchive MihomoSourceArchive   `json:"source_archive"`
	Assets        []MihomoPlatformAsset `json:"assets"`
}

type MihomoSourceArchive struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type MihomoPlatformAsset struct {
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Name     string `json:"name"`
	SHA256   string `json:"sha256"`
}

func LoadMihomoProvenance(name string) (MihomoProvenance, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return MihomoProvenance{}, err
	}
	var provenance MihomoProvenance
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&provenance); err != nil {
		return MihomoProvenance{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return MihomoProvenance{}, err
	}
	if err := ValidateMihomoProvenance(provenance); err != nil {
		return MihomoProvenance{}, err
	}
	return provenance, nil
}

func ValidateMihomoProvenance(provenance MihomoProvenance) error {
	if provenance.Schema != 1 ||
		provenance.Repository != "MetaCubeX/mihomo" ||
		!stableMihomoVersion.MatchString(provenance.Version) ||
		provenance.Tag != provenance.Version ||
		!commitSHA.MatchString(provenance.Commit) ||
		provenance.License != "GPL-3.0-only" ||
		!strings.HasPrefix(provenance.LicenseURL, "https://raw.githubusercontent.com/MetaCubeX/mihomo/") {
		return errors.New("Mihomo release identity, tag, commit, or GPL license is invalid")
	}
	if provenance.SourceArchive.Name == "" ||
		!strings.Contains(provenance.SourceArchive.Name, provenance.Version) ||
		!strings.HasPrefix(provenance.SourceArchive.URL, "https://api.github.com/repos/MetaCubeX/mihomo/") ||
		!digestSHA256.MatchString(provenance.SourceArchive.SHA256) {
		return errors.New("Mihomo source archive provenance is invalid")
	}
	expected := map[string]struct{}{
		"linux/amd64":   {},
		"linux/arm64":   {},
		"windows/amd64": {},
		"windows/arm64": {},
		"darwin/amd64":  {},
		"darwin/arm64":  {},
	}
	for _, asset := range provenance.Assets {
		key := asset.Platform + "/" + asset.Arch
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("Mihomo asset target %q is unexpected or duplicated", key)
		}
		delete(expected, key)
		if filepath.Base(asset.Name) != asset.Name ||
			!strings.Contains(asset.Name, provenance.Version) ||
			!digestSHA256.MatchString(asset.SHA256) {
			return fmt.Errorf("Mihomo asset %q provenance is invalid", asset.Name)
		}
	}
	if len(expected) != 0 {
		return errors.New("Mihomo provenance omits a release platform asset")
	}
	return nil
}

func VerifyMihomoAssets(root string, provenance MihomoProvenance) error {
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("Mihomo asset directory must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Mihomo asset directory must be a real directory")
	}
	files := append([]MihomoPlatformAsset(nil), provenance.Assets...)
	files = append(files, MihomoPlatformAsset{
		Name:   provenance.SourceArchive.Name,
		SHA256: provenance.SourceArchive.SHA256,
	})
	for _, asset := range files {
		name := filepath.Join(root, asset.Name)
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Mihomo release file is missing or unsafe: %s", asset.Name)
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != asset.SHA256 {
			return fmt.Errorf("Mihomo release file digest differs: %s", asset.Name)
		}
	}
	return nil
}
