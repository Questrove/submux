package fakeip

import (
	"strings"
	"testing"
)

func TestValidateNormalizesStableEntriesAndOwnsRevision(t *testing.T) {
	value, err := Validate(Config{
		Mode:     " BLACKLIST ",
		Entries:  []string{" +.lan ", "printer.lan", "+.lan", ""},
		Revision: strings.Repeat("f", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.Mode != ModeBlacklist {
		t.Fatalf("mode = %q", value.Mode)
	}
	if len(value.Entries) != 2 || value.Entries[0] != "+.lan" || value.Entries[1] != "printer.lan" {
		t.Fatalf("entries = %#v", value.Entries)
	}
	if len(value.Revision) != 64 || value.Revision == strings.Repeat("f", 64) {
		t.Fatalf("revision was not recomputed: %q", value.Revision)
	}
	again, err := Validate(value)
	if err != nil || again.Revision != value.Revision {
		t.Fatalf("normalization is not stable: %#v, %v", again, err)
	}
}

func TestParseRejectsUnknownFieldsTrailingJSONAndInvalidEntries(t *testing.T) {
	for _, raw := range []string{
		`{"mode":"blacklist","entries":[],"unknown":true}`,
		`{"mode":"blacklist","entries":[]} {"mode":"blacklist"}`,
		`{"mode":"other","entries":[]}`,
		`{"mode":"blacklist","entries":["example.com; flush ruleset"]}`,
		`{"mode":"blacklist","entries":["https://example.com"]}`,
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("invalid shared fake-IP setting was accepted: %s", raw)
		}
	}
}

func TestMarshalRoundTripAndDefault(t *testing.T) {
	raw, normalized, err := Marshal(Config{Mode: ModeWhitelist, Entries: []string{"example.com", "*.local"}})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Mode != normalized.Mode || parsed.Revision != normalized.Revision || len(parsed.Entries) != 2 {
		t.Fatalf("round trip changed config: normalized=%#v parsed=%#v", normalized, parsed)
	}
	if empty, err := Parse(""); err != nil || empty.Mode != ModeBlacklist || len(empty.Entries) != 0 || len(empty.Revision) != 64 {
		t.Fatalf("default = %#v, %v", empty, err)
	}
}
