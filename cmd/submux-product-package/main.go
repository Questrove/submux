package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"submux/internal/productupdate"
	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
	"submux/internal/runtimeupdate"
	"submux/internal/safepath"
)

var stableVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

const (
	maxComponentBytes       = 200 << 20
	maxProductExpandedBytes = 256 << 20
)

type componentInput struct {
	Name string
	Path string
	Body []byte
}

type targetDescriptor struct {
	Path   string          `json:"path"`
	Source string          `json:"source"`
	Custom json.RawMessage `json:"custom"`
}

type productCustom struct {
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("submux-product-package", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "stable vX.Y.Z Runtime version")
	platform := flags.String("platform", "", "linux, windows, or darwin")
	arch := flags.String("arch", "", "amd64 or arm64")
	runtimePath := flags.String("runtime", "", "submux-runtime executable")
	networkPath := flags.String("runtime-net", "", "submux-runtime-net executable")
	guiPath := flags.String("gui", "", "submux-runtime-gui executable")
	output := flags.String("output", "", "new product update ZIP")
	descriptor := flags.String("descriptor", "", "new TUF target descriptor JSON")
	releaseNotes := flags.String("release-notes", "", "signed release note summary")
	migrationSummary := flags.String("migration-summary", "", "signed database migration summary")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "submux-product-package does not accept positional arguments")
		return 2
	}
	result, err := buildProductPackage(packageRequest{
		Version:          *version,
		Platform:         *platform,
		Arch:             *arch,
		Runtime:          *runtimePath,
		RuntimeNet:       *networkPath,
		GUI:              *guiPath,
		Output:           *output,
		Descriptor:       *descriptor,
		ReleaseNotes:     *releaseNotes,
		MigrationSummary: *migrationSummary,
	})
	if err != nil {
		fmt.Fprintf(stderr, "build Runtime product package: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\nsha256=%s\n", result.Output, result.SHA256)
	return 0
}

type packageRequest struct {
	Version          string
	Platform         string
	Arch             string
	Runtime          string
	RuntimeNet       string
	GUI              string
	Output           string
	Descriptor       string
	ReleaseNotes     string
	MigrationSummary string
}

type packageResult struct {
	Output     string
	Descriptor string
	SHA256     string
}

func buildProductPackage(request packageRequest) (packageResult, error) {
	if !stableVersion.MatchString(request.Version) {
		return packageResult{}, errors.New("version must be an exact stable vX.Y.Z value")
	}
	if request.Platform != "linux" && request.Platform != "windows" && request.Platform != "darwin" {
		return packageResult{}, errors.New("platform must be linux, windows, or darwin")
	}
	if request.Arch != "amd64" && request.Arch != "arm64" {
		return packageResult{}, errors.New("arch must be amd64 or arm64")
	}
	if strings.TrimSpace(request.ReleaseNotes) == "" || strings.TrimSpace(request.MigrationSummary) == "" {
		return packageResult{}, errors.New("release notes and migration summary are required signed metadata")
	}
	output, descriptor, err := validateOutputPaths(request.Output, request.Descriptor)
	if err != nil {
		return packageResult{}, err
	}
	inputs := []componentInput{
		{Name: "submux-runtime", Path: request.Runtime},
		{Name: "submux-runtime-net", Path: request.RuntimeNet},
		{Name: "submux-runtime-gui", Path: request.GUI},
	}
	totalComponentBytes := 0
	for index := range inputs {
		body, err := readComponent(inputs[index].Path)
		if err != nil {
			return packageResult{}, fmt.Errorf("%s: %w", inputs[index].Name, err)
		}
		if len(body) > maxProductExpandedBytes-totalComponentBytes {
			return packageResult{}, errors.New("product components exceed the expanded-size limit")
		}
		totalComponentBytes += len(body)
		inputs[index].Body = body
	}
	manifest := productupdate.PackageManifest{
		FormatVersion:       productupdate.PackageFormatVersion,
		Version:             request.Version,
		Platform:            request.Platform,
		Arch:                request.Arch,
		RuntimeSchemaTarget: runtimestate.SchemaVersion,
		ProtocolVersion:     runtimeapi.ProtocolVersion,
		Components:          make([]productupdate.PackageComponent, 0, len(inputs)),
	}
	for _, input := range inputs {
		sum := sha256.Sum256(input.Body)
		manifest.Components = append(manifest.Components, productupdate.PackageComponent{
			Name:   input.Name,
			Entry:  "payload/" + input.Name,
			Size:   int64(len(input.Body)),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return packageResult{}, err
	}
	manifestBody = append(manifestBody, '\n')
	body, err := encodeProductPackage(manifestBody, inputs)
	if err != nil {
		return packageResult{}, err
	}
	if len(body) > runtimeupdate.MaxBundleBytes {
		return packageResult{}, errors.New("product package exceeds the Runtime download limit")
	}
	if err := writeNew(output, body, 0644); err != nil {
		return packageResult{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(output)
			if descriptor != "" {
				_ = os.Remove(descriptor)
			}
		}
	}()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	components := []string{"submux-runtime", "submux-runtime-net", "submux-runtime-gui"}
	customValue := productCustom{
		Kind:                runtimeupdate.TargetKindProduct,
		Version:             request.Version,
		Platform:            request.Platform,
		Arch:                request.Arch,
		Channel:             runtimeupdate.ProductChannelStable,
		AssetName:           filepath.Base(output),
		ReleaseNotes:        strings.TrimSpace(request.ReleaseNotes),
		MigrationSummary:    strings.TrimSpace(request.MigrationSummary),
		RuntimeSchemaMin:    runtimestate.SchemaVersion,
		RuntimeSchemaMax:    runtimestate.SchemaVersion,
		RuntimeSchemaTarget: runtimestate.SchemaVersion,
		ProtocolMin:         runtimeapi.ProtocolVersion,
		ProtocolMax:         runtimeapi.ProtocolVersion,
		RequiredFreeBytes:   int64(len(body)) * 4,
		Components:          components,
	}
	target := runtimeupdate.ProductTarget{
		Path:                "product/" + request.Version + "/" + request.Platform + "/" + request.Arch + "/" + filepath.Base(output),
		Version:             request.Version,
		Platform:            request.Platform,
		Arch:                request.Arch,
		Channel:             runtimeupdate.ProductChannelStable,
		AssetName:           filepath.Base(output),
		ReleaseNotes:        customValue.ReleaseNotes,
		MigrationSummary:    customValue.MigrationSummary,
		RuntimeSchemaMin:    customValue.RuntimeSchemaMin,
		RuntimeSchemaMax:    customValue.RuntimeSchemaMax,
		RuntimeSchemaTarget: customValue.RuntimeSchemaTarget,
		ProtocolMin:         customValue.ProtocolMin,
		ProtocolMax:         customValue.ProtocolMax,
		RequiredFreeBytes:   customValue.RequiredFreeBytes,
		Components:          components,
		Length:              int64(len(body)),
		SHA256:              digest,
	}
	if _, err := productupdate.ParsePackage(body, target); err != nil {
		return packageResult{}, fmt.Errorf("self-verify product package: %w", err)
	}
	if descriptor != "" {
		custom, err := json.Marshal(customValue)
		if err != nil {
			return packageResult{}, err
		}
		descriptorBody, err := json.MarshalIndent(targetDescriptor{
			Path:   target.Path,
			Source: output,
			Custom: custom,
		}, "", "  ")
		if err != nil {
			return packageResult{}, err
		}
		if err := writeNew(descriptor, append(descriptorBody, '\n'), 0644); err != nil {
			return packageResult{}, err
		}
	}
	cleanup = false
	return packageResult{Output: output, Descriptor: descriptor, SHA256: digest}, nil
}

func validateOutputPaths(output, descriptor string) (string, string, error) {
	if output == "" || !filepath.IsAbs(output) || filepath.Ext(output) != ".zip" {
		return "", "", errors.New("output must be an absolute .zip path")
	}
	output = filepath.Clean(output)
	if descriptor != "" {
		if !filepath.IsAbs(descriptor) || filepath.Ext(descriptor) != ".json" {
			return "", "", errors.New("descriptor must be an absolute .json path")
		}
		descriptor = filepath.Clean(descriptor)
		if descriptor == output {
			return "", "", errors.New("descriptor and package outputs must differ")
		}
	}
	for _, candidate := range []string{output, descriptor} {
		if candidate == "" {
			continue
		}
		if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return "", "", fmt.Errorf("output already exists: %s", candidate)
			}
			return "", "", err
		}
		info, err := os.Lstat(filepath.Dir(candidate))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("output parent must be an existing real directory")
		}
	}
	return output, descriptor, nil
}

func readComponent(name string) ([]byte, error) {
	if name == "" || !filepath.IsAbs(name) {
		return nil, errors.New("component path must be absolute")
	}
	linked, err := safepath.ContainsLinkInExistingPath(name)
	if err != nil {
		return nil, err
	}
	if linked {
		return nil, errors.New("component path must not contain symbolic or reparse links")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maxComponentBytes {
		return nil, errors.New("component must be a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxComponentBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) ||
		int64(len(body)) != before.Size() || len(body) > maxComponentBytes {
		return nil, errors.New("component changed while it was being read")
	}
	return body, nil
}

func encodeProductPackage(manifest []byte, inputs []componentInput) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	if err := writeZipEntry(writer, "product-manifest.json", manifest, 0644); err != nil {
		return nil, err
	}
	for _, input := range inputs {
		if err := writeZipEntry(writer, "payload/"+input.Name, input.Body, 0755); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeZipEntry(writer *zip.Writer, name string, body []byte, mode os.FileMode) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(mode)
	header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
	destination, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = destination.Write(body)
	return err
}

func writeNew(name string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		_ = os.Remove(name)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(name)
		return err
	}
	return file.Close()
}
