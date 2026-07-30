package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"submux/internal/releasepolicy"
	"submux/internal/runtimeupdate"
)

type productDescriptor struct {
	Path   string          `json:"path"`
	Source string          `json:"source"`
	Custom json.RawMessage `json:"custom"`
}

type mihomoCustom struct {
	Kind           string `json:"kind"`
	Version        string `json:"version"`
	Platform       string `json:"platform"`
	Arch           string `json:"arch"`
	Repository     string `json:"repository"`
	AssetName      string `json:"asset_name"`
	UpstreamSHA256 string `json:"upstream_sha256"`
}

func runPrepareManifest(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("prepare-manifest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	descriptorsDir := flags.String("descriptors-dir", "", "directory containing product target descriptors")
	productDir := flags.String("product-dir", "", "directory containing product ZIP files")
	provenancePath := flags.String("mihomo-provenance", "", "Mihomo provenance JSON")
	assetsDir := flags.String("mihomo-assets-dir", "", "directory containing fixed Mihomo release assets")
	output := flags.String("output", "", "new targets publication manifest")
	version := flags.Int64("metadata-version", 0, "positive TUF metadata version")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "prepare-manifest does not accept positional arguments")
		return 2
	}
	manifest, err := preparePublicationManifest(
		*descriptorsDir,
		*productDir,
		*provenancePath,
		*assetsDir,
		*output,
		*version,
		time.Now().UTC(),
	)
	if err != nil {
		fmt.Fprintf(stderr, "prepare TUF publication manifest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "prepared metadata v%d with %d product and Mihomo targets: %s\n",
		manifest.MetadataVersion, len(manifest.Targets), *output)
	return 0
}

func preparePublicationManifest(
	descriptorsDir string,
	productDir string,
	provenancePath string,
	assetsDir string,
	output string,
	metadataVersion int64,
	now time.Time,
) (publishManifest, error) {
	if metadataVersion <= 0 {
		return publishManifest{}, errors.New("metadata version must be positive")
	}
	directories := map[string]string{
		"descriptors": descriptorsDir,
		"products":    productDir,
		"Mihomo":      assetsDir,
	}
	for label, directory := range directories {
		if directory == "" || !filepath.IsAbs(directory) {
			return publishManifest{}, fmt.Errorf("%s directory must be absolute", label)
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return publishManifest{}, fmt.Errorf("%s directory must be real", label)
		}
	}
	if output == "" || !filepath.IsAbs(output) || filepath.Ext(output) != ".json" {
		return publishManifest{}, errors.New("manifest output must be an absolute .json path")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return publishManifest{}, errors.New("manifest output already exists")
		}
		return publishManifest{}, err
	}
	provenance, err := releasepolicy.LoadMihomoProvenance(provenancePath)
	if err != nil {
		return publishManifest{}, err
	}
	if err := releasepolicy.VerifyMihomoAssets(assetsDir, provenance); err != nil {
		return publishManifest{}, err
	}
	entries, err := os.ReadDir(descriptorsDir)
	if err != nil {
		return publishManifest{}, err
	}
	var descriptorNames []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".json" {
			descriptorNames = append(descriptorNames, entry.Name())
		}
	}
	sort.Strings(descriptorNames)
	if len(descriptorNames) == 0 {
		return publishManifest{}, errors.New("no product target descriptors found")
	}
	manifest := publishManifest{
		Schema:           targetManifestSchema,
		MetadataVersion:  metadataVersion,
		TargetsExpires:   now.Add(90 * 24 * time.Hour).UTC().Truncate(time.Second),
		SnapshotExpires:  now.Add(30 * 24 * time.Hour).UTC().Truncate(time.Second),
		TimestampExpires: now.Add(7 * 24 * time.Hour).UTC().Truncate(time.Second),
	}
	for _, name := range descriptorNames {
		body, err := os.ReadFile(filepath.Join(descriptorsDir, name))
		if err != nil {
			return publishManifest{}, err
		}
		var descriptor productDescriptor
		if err := decodeStrictJSON(body, &descriptor); err != nil {
			return publishManifest{}, fmt.Errorf("decode product descriptor %s: %w", name, err)
		}
		source := filepath.Join(productDir, path.Base(descriptor.Path))
		target := publishTarget{Path: descriptor.Path, Source: source, Custom: descriptor.Custom}
		if err := validatePublishTarget(target); err != nil {
			return publishManifest{}, fmt.Errorf("product descriptor %s: %w", name, err)
		}
		manifest.Targets = append(manifest.Targets, target)
	}
	for _, asset := range provenance.Assets {
		custom, err := json.Marshal(mihomoCustom{
			Kind:           runtimeupdate.TargetKindMihomo,
			Version:        provenance.Version,
			Platform:       asset.Platform,
			Arch:           asset.Arch,
			Repository:     provenance.Repository,
			AssetName:      asset.Name,
			UpstreamSHA256: asset.SHA256,
		})
		if err != nil {
			return publishManifest{}, err
		}
		manifest.Targets = append(manifest.Targets, publishTarget{
			Path: path.Join(
				"mihomo",
				provenance.Version,
				asset.Platform,
				asset.Arch,
				asset.Name,
			),
			Source: filepath.Join(assetsDir, asset.Name),
			Custom: custom,
		})
	}
	sort.Slice(manifest.Targets, func(left, right int) bool {
		return manifest.Targets[left].Path < manifest.Targets[right].Path
	})
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return publishManifest{}, err
	}
	if err := writeNewFile(output, append(body, '\n'), 0644); err != nil {
		return publishManifest{}, err
	}
	return manifest, nil
}
