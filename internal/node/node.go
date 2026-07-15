package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"submux/internal/parse"
	"submux/internal/store"
)

func FromParsed(sourceID int64, origin string, parsed parse.Node) (store.NodeRecord, error) {
	name, _ := parsed["name"].(string)
	kind, _ := parsed["type"].(string)
	name, kind = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(kind))
	if name == "" || kind == "" {
		return store.NodeRecord{}, fmt.Errorf("node is missing name or type")
	}
	config, err := json.Marshal(parsed)
	if err != nil {
		return store.NodeRecord{}, err
	}
	fingerprint, err := Fingerprint(parsed)
	if err != nil {
		return store.NodeRecord{}, err
	}
	return store.NodeRecord{
		SourceID: sourceID, Origin: origin, Name: name, Protocol: kind,
		Config: config, Fingerprint: fingerprint, Enabled: true, Role: "proxy",
	}, nil
}

func Import(sourceID int64, origin, raw string) ([]store.NodeRecord, error) {
	parsed, err := parse.ParseSubscription(raw)
	if err != nil {
		return nil, err
	}
	out := make([]store.NodeRecord, 0, len(parsed))
	for index, item := range parsed {
		record, err := FromParsed(sourceID, origin, item)
		if err != nil {
			return nil, fmt.Errorf("node %d: %w", index, err)
		}
		out = append(out, record)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("input contains no nodes")
	}
	return out, nil
}

func Decode(record store.NodeRecord) (parse.Node, error) {
	var parsed parse.Node
	if err := json.Unmarshal(record.Config, &parsed); err != nil {
		return nil, fmt.Errorf("decode node %d: %w", record.ID, err)
	}
	return parsed, nil
}

func Fingerprint(parsed parse.Node) (string, error) {
	identity := make(map[string]any, len(parsed)-1)
	for key, value := range parsed {
		if key != "name" {
			identity[key] = value
		}
	}
	if kind, ok := identity["type"].(string); ok {
		identity["type"] = strings.ToLower(kind)
	}
	b, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func DisplayName(record store.NodeRecord) string {
	if name := strings.TrimSpace(record.Alias); name != "" {
		return name
	}
	return record.Name
}
