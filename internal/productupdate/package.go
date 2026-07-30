package productupdate

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"

	"submux/internal/runtimeupdate"
)

const (
	PackageFormatVersion   = 1
	maxPackageEntries      = 8
	maxPackageManifestSize = 256 << 10
	maxPackageExpandedSize = 600 << 20
)

type Package struct {
	Manifest PackageManifest
	Body     []byte
	Payloads map[string][]byte
}

type PackageManifest struct {
	FormatVersion       int                `json:"format_version"`
	Version             string             `json:"version"`
	Platform            string             `json:"platform"`
	Arch                string             `json:"arch"`
	RuntimeSchemaTarget int                `json:"runtime_schema_target"`
	ProtocolVersion     int                `json:"protocol_version"`
	Components          []PackageComponent `json:"components"`
}

type PackageComponent struct {
	Name   string `json:"name"`
	Entry  string `json:"entry"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func ParsePackage(body []byte, target runtimeupdate.ProductTarget) (Package, error) {
	if len(body) == 0 || len(body) > runtimeupdate.MaxBundleBytes {
		return Package{}, errors.New("Runtime product package is empty or too large")
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(reader.File) < 4 || len(reader.File) > maxPackageEntries {
		return Package{}, errors.New("Runtime product package is not a bounded ZIP file")
	}
	files := make(map[string]*zip.File, len(reader.File))
	var expanded uint64
	for _, entry := range reader.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		if !entry.Mode().IsRegular() || name == "" || path.Clean(name) != name ||
			strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") {
			return Package{}, errors.New("Runtime product package contains an unsafe path")
		}
		if _, duplicate := files[name]; duplicate {
			return Package{}, errors.New("Runtime product package contains duplicate entries")
		}
		if entry.UncompressedSize64 > maxPackageExpandedSize ||
			expanded > maxPackageExpandedSize-entry.UncompressedSize64 {
			return Package{}, errors.New("Runtime product package expands beyond its limit")
		}
		expanded += entry.UncompressedSize64
		files[name] = entry
	}
	manifestBody, err := readPackageFile(files["product-manifest.json"], maxPackageManifestSize)
	if err != nil {
		return Package{}, errors.New("Runtime product manifest is unavailable")
	}
	var manifest PackageManifest
	if err := decodePackageJSON(manifestBody, &manifest); err != nil {
		return Package{}, errors.New("Runtime product manifest is invalid")
	}
	if manifest.FormatVersion != PackageFormatVersion ||
		manifest.Version != target.Version ||
		manifest.Platform != target.Platform ||
		manifest.Arch != target.Arch ||
		manifest.RuntimeSchemaTarget != target.RuntimeSchemaTarget ||
		manifest.ProtocolVersion < target.ProtocolMin ||
		manifest.ProtocolVersion > target.ProtocolMax ||
		len(manifest.Components) != len(target.Components) {
		return Package{}, errors.New("Runtime product manifest does not match signed target metadata")
	}
	components := make(map[string]PackageComponent, len(manifest.Components))
	expectedEntries := map[string]struct{}{"product-manifest.json": {}}
	payloads := make(map[string][]byte, len(manifest.Components))
	for _, component := range manifest.Components {
		expectedEntry := path.Join("payload", component.Name)
		if !validProductComponent(component.Name, target.Components) ||
			component.Entry != expectedEntry ||
			component.Size <= 0 ||
			component.Size > maxPackageExpandedSize ||
			!validDigest(component.SHA256) {
			return Package{}, errors.New("Runtime product manifest contains an invalid component")
		}
		if _, duplicate := components[component.Name]; duplicate {
			return Package{}, errors.New("Runtime product manifest repeats a component")
		}
		components[component.Name] = component
		expectedEntries[component.Entry] = struct{}{}
		payload, err := readPackageFile(files[component.Entry], component.Size)
		if err != nil || int64(len(payload)) != component.Size {
			return Package{}, errors.New("Runtime product component size does not match its manifest")
		}
		sum := sha256.Sum256(payload)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), component.SHA256) {
			return Package{}, errors.New("Runtime product component digest does not match its manifest")
		}
		payloads[component.Name] = payload
	}
	for _, required := range target.Components {
		if _, ok := components[required]; !ok {
			return Package{}, errors.New("Runtime product package omits a required component")
		}
	}
	if len(files) != len(expectedEntries) {
		return Package{}, errors.New("Runtime product package contains unlisted content")
	}
	return Package{
		Manifest: manifest,
		Body:     bytes.Clone(body),
		Payloads: payloads,
	}, nil
}

func validProductComponent(name string, allowed []string) bool {
	for _, candidate := range allowed {
		if name == candidate {
			return true
		}
	}
	return false
}

func readPackageFile(entry *zip.File, maximum int64) ([]byte, error) {
	if entry == nil || maximum <= 0 || entry.UncompressedSize64 > uint64(maximum) {
		return nil, errors.New("Runtime product package entry exceeds its limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, maximum+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > maximum || int64(len(body)) != int64(entry.UncompressedSize64) {
		return nil, errors.New("Runtime product package entry length is invalid")
	}
	return body, nil
}

func decodePackageJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
