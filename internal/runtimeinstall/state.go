package runtimeinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	ReceiptFormatVersion = 1
	ProductID            = "com.byworld.submux.runtime"

	ModeFresh     = "fresh"
	ModeUpgrade   = "upgrade"
	ModeRepair    = "repair"
	ModeDowngrade = "downgrade"

	PackageOnline  = "online"
	PackageOffline = "offline"
)

var stableVersion = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

type Receipt struct {
	FormatVersion int       `json:"format_version"`
	ProductID     string    `json:"product_id"`
	Version       string    `json:"version"`
	InstallRoot   string    `json:"install_root"`
	Platform      string    `json:"platform"`
	Arch          string    `json:"arch"`
	PackageKind   string    `json:"package_kind"`
	InstalledAt   time.Time `json:"installed_at"`
}

type Request struct {
	Version            string
	InstallRoot        string
	Platform           string
	Arch               string
	PackageKind        string
	Repair             bool
	AllowDowngrade     bool
	DatabaseCompatible bool
}

type Plan struct {
	Mode          string `json:"mode"`
	FromVersion   string `json:"from_version,omitempty"`
	ToVersion     string `json:"to_version"`
	InstallRoot   string `json:"install_root"`
	PreserveState bool   `json:"preserve_state"`
	PackageKind   string `json:"package_kind"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
}

func Decide(current *Receipt, request Request) (Plan, error) {
	if err := validateRequest(request); err != nil {
		return Plan{}, err
	}
	plan := Plan{
		ToVersion:     request.Version,
		InstallRoot:   filepath.Clean(request.InstallRoot),
		PreserveState: true,
		PackageKind:   request.PackageKind,
		Platform:      request.Platform,
		Arch:          request.Arch,
	}
	if current == nil {
		plan.Mode = ModeFresh
		return plan, nil
	}
	if err := ValidateReceipt(*current); err != nil {
		return Plan{}, errors.New("installed Runtime receipt is invalid")
	}
	if filepath.Clean(current.InstallRoot) != plan.InstallRoot {
		return Plan{}, errors.New("a second Runtime installation target is not allowed")
	}
	if current.Platform != request.Platform || current.Arch != request.Arch {
		return Plan{}, errors.New("installed Runtime platform or architecture does not match this package")
	}
	plan.FromVersion = current.Version
	comparison, err := compareVersions(request.Version, current.Version)
	if err != nil {
		return Plan{}, err
	}
	switch {
	case comparison > 0:
		plan.Mode = ModeUpgrade
	case comparison == 0 && request.Repair:
		plan.Mode = ModeRepair
	case comparison == 0:
		return Plan{}, errors.New("same-version Runtime installation requires explicit repair")
	case !request.AllowDowngrade:
		return Plan{}, errors.New("Runtime downgrade requires explicit authorization")
	case !request.DatabaseCompatible:
		return Plan{}, errors.New("Runtime downgrade is incompatible with the current database")
	default:
		plan.Mode = ModeDowngrade
	}
	return plan, nil
}

func NewReceipt(plan Plan, installedAt time.Time) (Receipt, error) {
	receipt := Receipt{
		FormatVersion: ReceiptFormatVersion,
		ProductID:     ProductID,
		Version:       plan.ToVersion,
		InstallRoot:   filepath.Clean(plan.InstallRoot),
		Platform:      plan.Platform,
		Arch:          plan.Arch,
		PackageKind:   plan.PackageKind,
		InstalledAt:   installedAt.UTC(),
	}
	if err := ValidateReceipt(receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func DecodeReceipt(body []byte) (Receipt, error) {
	var receipt Receipt
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, errors.New("Runtime installation receipt is invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Receipt{}, errors.New("Runtime installation receipt contains trailing data")
	}
	if err := ValidateReceipt(receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func ValidateReceipt(receipt Receipt) error {
	if receipt.FormatVersion != ReceiptFormatVersion ||
		receipt.ProductID != ProductID ||
		!stableVersion.MatchString(receipt.Version) ||
		receipt.InstallRoot == "" ||
		!filepath.IsAbs(receipt.InstallRoot) ||
		filepath.Clean(receipt.InstallRoot) != receipt.InstallRoot ||
		!validPlatformArch(receipt.Platform, receipt.Arch) ||
		!validPackageKind(receipt.PackageKind) ||
		receipt.InstalledAt.IsZero() {
		return errors.New("Runtime installation receipt is invalid")
	}
	return nil
}

func validateRequest(request Request) error {
	if !stableVersion.MatchString(request.Version) {
		return errors.New("Runtime package version must be an exact stable vX.Y.Z version")
	}
	if request.InstallRoot == "" ||
		!filepath.IsAbs(request.InstallRoot) ||
		filepath.Clean(request.InstallRoot) != request.InstallRoot {
		return errors.New("Runtime package installation root must be fixed, absolute, and clean")
	}
	if !validPlatformArch(request.Platform, request.Arch) {
		return errors.New("Runtime package platform or architecture is unsupported")
	}
	if !validPackageKind(request.PackageKind) {
		return errors.New("Runtime package kind must be online or offline")
	}
	return nil
}

func validPlatformArch(platform, arch string) bool {
	switch platform {
	case "linux", "windows":
		return arch == "amd64" || arch == "arm64"
	case "darwin":
		return arch == "universal"
	default:
		return false
	}
}

func validPackageKind(kind string) bool {
	return kind == PackageOnline || kind == PackageOffline
}

func compareVersions(left, right string) (int, error) {
	leftParts := stableVersion.FindStringSubmatch(left)
	rightParts := stableVersion.FindStringSubmatch(right)
	if len(leftParts) != 4 || len(rightParts) != 4 {
		return 0, errors.New("Runtime installation receipt contains an invalid version")
	}
	for index := 1; index <= 3; index++ {
		leftValue, err := strconv.ParseUint(leftParts[index], 10, 64)
		if err != nil {
			return 0, err
		}
		rightValue, err := strconv.ParseUint(rightParts[index], 10, 64)
		if err != nil {
			return 0, err
		}
		if leftValue < rightValue {
			return -1, nil
		}
		if leftValue > rightValue {
			return 1, nil
		}
	}
	return 0, nil
}

func IsPreviewPlatform(platform, arch string) bool {
	return platform == "darwin" ||
		(platform == "windows" && arch == "arm64") ||
		strings.Contains(platform, "musl") ||
		platform == "openwrt" ||
		platform == "linux-nonsystemd"
}
