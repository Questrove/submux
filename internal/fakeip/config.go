package fakeip

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	SettingKey    = "shared_fake_ip_filter"
	ModeBlacklist = "blacklist"
	ModeWhitelist = "whitelist"
)

var domainEntry = regexp.MustCompile(`^(?:\+\.|\*\.)?(?:[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,62}\.)*)[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,62})$`)

// Config is the platform-wide fake-ip-filter prefix. Revision is computed from
// the normalized mode and entries and is never accepted as caller authority.
type Config struct {
	Mode     string   `json:"mode"`
	Entries  []string `json:"entries"`
	Revision string   `json:"revision"`
}

func Default() Config {
	return normalize(Config{Mode: ModeBlacklist})
}

func Parse(raw string) (Config, error) {
	if strings.TrimSpace(raw) == "" {
		return Default(), nil
	}
	var value Config
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Config{}, fmt.Errorf("parse shared fake-ip-filter: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("parse shared fake-ip-filter: trailing JSON content")
	}
	return Validate(value)
}

func Validate(value Config) (Config, error) {
	value = normalize(value)
	if value.Mode != ModeBlacklist && value.Mode != ModeWhitelist {
		return Config{}, errors.New("shared fake-ip-filter mode must be blacklist or whitelist")
	}
	if len(value.Entries) > 1024 {
		return Config{}, errors.New("shared fake-ip-filter has more than 1024 entries")
	}
	for _, entry := range value.Entries {
		if len(entry) > 253 || !domainEntry.MatchString(entry) {
			return Config{}, fmt.Errorf("invalid shared fake-ip-filter entry %q", entry)
		}
	}
	value.Revision = revision(value.Mode, value.Entries)
	return value, nil
}

func Marshal(value Config) (string, Config, error) {
	value, err := Validate(value)
	if err != nil {
		return "", Config{}, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", Config{}, err
	}
	return string(raw), value, nil
}

func normalize(value Config) Config {
	value.Mode = strings.ToLower(strings.TrimSpace(value.Mode))
	if value.Mode == "" {
		value.Mode = ModeBlacklist
	}
	seen := make(map[string]bool, len(value.Entries))
	entries := make([]string, 0, len(value.Entries))
	for _, raw := range value.Entries {
		entry := strings.TrimSpace(raw)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		entries = append(entries, entry)
	}
	value.Entries = entries
	value.Revision = revision(value.Mode, value.Entries)
	return value
}

func revision(mode string, entries []string) string {
	raw, _ := json.Marshal(struct {
		Mode    string   `json:"mode"`
		Entries []string `json:"entries"`
	}{Mode: mode, Entries: entries})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
