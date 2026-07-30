package releasepolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

const SupportMatrixSchema = 1

var RequiredStableEvidence = []string{
	"native-install",
	"ipc-authorization",
	"explicit-proxy-ipv4-tcp",
	"explicit-proxy-ipv6-tcp",
	"tun-ipv4-tcp",
	"tun-ipv4-udp",
	"tun-ipv6-tcp",
	"tun-ipv6-udp",
	"dns",
	"route-conflict",
	"fail-open",
	"runtime-crash",
	"mihomo-crash",
	"network-helper-crash",
	"system-reboot",
	"upgrade",
	"database-migration",
	"program-rollback",
	"uninstall-residuals",
	"source-security",
	"arbitrary-file-rejection",
	"unauthorized-listener-rejection",
	"secret-scan",
	"gui-tui-cli-parity",
}

type SupportMatrix struct {
	Schema  int             `json:"schema"`
	Entries []SupportTarget `json:"entries"`
}

type SupportTarget struct {
	ID       string            `json:"id"`
	Platform string            `json:"platform"`
	System   string            `json:"system"`
	Arch     string            `json:"arch"`
	Status   string            `json:"status"`
	Evidence []SupportEvidence `json:"evidence"`
	Missing  []string          `json:"missing"`
}

type SupportEvidence struct {
	Category string `json:"category"`
	Level    string `json:"level"`
	Source   string `json:"source"`
}

func LoadSupportMatrix(name string) (SupportMatrix, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return SupportMatrix{}, err
	}
	var matrix SupportMatrix
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&matrix); err != nil {
		return SupportMatrix{}, fmt.Errorf("decode support matrix: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return SupportMatrix{}, err
	}
	if err := ValidateSupportMatrix(matrix); err != nil {
		return SupportMatrix{}, err
	}
	return matrix, nil
}

func ValidateSupportMatrix(matrix SupportMatrix) error {
	if matrix.Schema != SupportMatrixSchema {
		return fmt.Errorf("support matrix schema must be %d", SupportMatrixSchema)
	}
	if len(matrix.Entries) == 0 {
		return errors.New("support matrix has no targets")
	}
	seen := make(map[string]struct{}, len(matrix.Entries))
	for _, target := range matrix.Entries {
		if target.ID == "" || target.Platform == "" || target.System == "" || target.Arch == "" {
			return errors.New("support target identity is incomplete")
		}
		if _, duplicate := seen[target.ID]; duplicate {
			return fmt.Errorf("support target %q is duplicated", target.ID)
		}
		seen[target.ID] = struct{}{}
		switch target.Platform {
		case "linux", "windows", "darwin":
		default:
			return fmt.Errorf("support target %q has unsupported platform %q", target.ID, target.Platform)
		}
		switch target.Arch {
		case "amd64", "arm64":
		default:
			return fmt.Errorf("support target %q has unsupported architecture %q", target.ID, target.Arch)
		}
		switch target.Status {
		case "stable":
			if len(target.Missing) != 0 {
				return fmt.Errorf("stable support target %q still lists missing evidence", target.ID)
			}
			if err := validateStableEvidence(target); err != nil {
				return err
			}
		case "preview":
			if len(target.Missing) == 0 {
				return fmt.Errorf("preview support target %q must explain missing evidence", target.ID)
			}
		default:
			return fmt.Errorf("support target %q has invalid status %q", target.ID, target.Status)
		}
		if err := validateEvidence(target); err != nil {
			return err
		}
	}
	return nil
}

func validateEvidence(target SupportTarget) error {
	seen := make(map[string]struct{}, len(target.Evidence))
	for _, evidence := range target.Evidence {
		if evidence.Category == "" || evidence.Source == "" {
			return fmt.Errorf("support target %q has incomplete evidence", target.ID)
		}
		switch evidence.Level {
		case "native", "virtualized", "cross-build", "static":
		default:
			return fmt.Errorf("support target %q has invalid evidence level %q", target.ID, evidence.Level)
		}
		key := evidence.Category + "\x00" + evidence.Level + "\x00" + evidence.Source
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("support target %q repeats evidence %q", target.ID, evidence.Category)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateStableEvidence(target SupportTarget) error {
	native := make(map[string]struct{}, len(target.Evidence))
	for _, evidence := range target.Evidence {
		if evidence.Level == "native" {
			native[evidence.Category] = struct{}{}
		}
	}
	for _, required := range RequiredStableEvidence {
		if _, ok := native[required]; !ok {
			return fmt.Errorf("stable support target %q lacks native %s evidence", target.ID, required)
		}
	}
	if target.Platform == "linux" {
		for _, category := range []string{"gateway-ipv4-tcp", "gateway-ipv4-udp"} {
			if _, ok := native[category]; !ok {
				return fmt.Errorf("stable Linux target %q lacks native %s evidence", target.ID, category)
			}
		}
	}
	return nil
}

func MatrixStatus(matrix SupportMatrix, platform, arch string) (string, error) {
	statuses := make([]string, 0, 2)
	for _, target := range matrix.Entries {
		if target.Platform == platform && target.Arch == arch {
			statuses = append(statuses, target.Status)
		}
	}
	if len(statuses) == 0 {
		return "", fmt.Errorf("support matrix has no %s/%s target", platform, arch)
	}
	if slices.Contains(statuses, "stable") {
		return "stable", nil
	}
	return "preview", nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("JSON contains trailing data")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func ValidReleaseChannel(value string) bool {
	return value == "stable" || value == "preview"
}

func NormalizeArtifactName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, `/\`) || value == "." || value == ".." {
		return "", errors.New("artifact name must be a plain file name")
	}
	return value, nil
}
