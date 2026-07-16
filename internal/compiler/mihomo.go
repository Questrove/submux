package compiler

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"submux/internal/node"
	"submux/internal/store"
)

func validateMihomoTemplate(content string, slots []store.TemplateSlot) error {
	root, err := parseYAMLMap(content)
	if err != nil {
		return fmt.Errorf("parse mihomo template: %w", err)
	}
	groups, err := objectList(root["proxy-groups"], "proxy-groups")
	if err != nil {
		return err
	}
	byName := map[string]map[string]any{}
	for _, group := range groups {
		name := stringValue(group["name"])
		if name == "" {
			return fmt.Errorf("mihomo proxy group is missing name")
		}
		if byName[name] != nil {
			return fmt.Errorf("duplicate mihomo proxy group %q", name)
		}
		byName[name] = group
	}
	for _, slot := range slots {
		if byName[slot.Target] == nil {
			return fmt.Errorf("mihomo slot %q targets missing proxy group %q", slot.Key, slot.Target)
		}
	}
	return nil
}

func compileMihomo(value resolvedSubscription) ([]byte, error) {
	root, err := parseYAMLMap(value.Template.Content)
	if err != nil {
		return nil, err
	}
	existing, err := optionalObjectList(root["proxies"], "proxies")
	if err != nil {
		return nil, err
	}
	proxyNames := map[string]bool{}
	for _, item := range existing {
		if name := stringValue(item["name"]); name != "" {
			proxyNames[name] = true
		}
	}
	for _, record := range value.Records {
		parsed, err := node.Decode(record)
		if err != nil {
			return nil, err
		}
		copy := deepCopyMap(map[string]any(parsed))
		name := value.Names[record.Fingerprint]
		if proxyNames[name] {
			return nil, fmt.Errorf("generated node name conflicts with template proxy %q", name)
		}
		copy["name"], proxyNames[name] = name, true
		existing = append(existing, copy)
	}
	root["proxies"] = mapsToAny(existing)

	groups, _ := objectList(root["proxy-groups"], "proxy-groups")
	byName, groupNames := map[string]map[string]any{}, map[string]bool{}
	for _, group := range groups {
		name := stringValue(group["name"])
		byName[name], groupNames[name] = group, true
	}
	for _, slot := range value.Template.Slots {
		group := byName[slot.Target]
		current, err := optionalStringList(group["proxies"], "proxy group proxies")
		if err != nil {
			return nil, fmt.Errorf("group %q: %w", slot.Target, err)
		}
		names := value.Slots[slot.Key]
		if slot.Required && len(names) == 0 {
			return nil, fmt.Errorf("required slot %q is empty", slot.Key)
		}
		if slot.Mode == "replace" {
			current = nil
		}
		group["proxies"] = uniqueStrings(append(current, names...))
	}
	if err := validateMihomoReferences(root, proxyNames, groupNames); err != nil {
		return nil, err
	}
	return yaml.Marshal(root)
}

func validateMihomoReferences(root map[string]any, proxies, groups map[string]bool) error {
	allowed := map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true, "GLOBAL": true}
	for key := range proxies {
		allowed[key] = true
	}
	for key := range groups {
		allowed[key] = true
	}
	groupList, _ := objectList(root["proxy-groups"], "proxy-groups")
	for _, group := range groupList {
		name := stringValue(group["name"])
		refs, err := optionalStringList(group["proxies"], "proxies")
		if err != nil {
			return fmt.Errorf("group %q: %w", name, err)
		}
		for _, ref := range refs {
			if !allowed[ref] {
				return fmt.Errorf("group %q references unknown proxy or group %q", name, ref)
			}
		}
	}
	if rawRules, ok := root["rules"]; ok {
		rules, err := optionalStringList(rawRules, "rules")
		if err != nil {
			return err
		}
		for _, rule := range rules {
			parts := strings.Split(rule, ",")
			if len(parts) < 2 {
				return fmt.Errorf("invalid mihomo rule %q", rule)
			}
			target := strings.TrimSpace(parts[len(parts)-1])
			if target == "no-resolve" && len(parts) > 2 {
				target = strings.TrimSpace(parts[len(parts)-2])
			}
			if !allowed[target] {
				return fmt.Errorf("mihomo rule references unknown target %q", target)
			}
		}
	}
	return nil
}

func parseYAMLMap(content string) (map[string]any, error) {
	var root map[string]any
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("template root must be an object")
	}
	return root, nil
}
func objectList(value any, field string) ([]map[string]any, error) {
	if value == nil {
		return nil, fmt.Errorf("%s is required", field)
	}
	return optionalObjectList(value, field)
}
func optionalObjectList(value any, field string) ([]map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", field)
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s item must be an object", field)
		}
		out = append(out, object)
	}
	return out, nil
}
func optionalStringList(value any, field string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		if stringsList, ok := value.([]string); ok {
			return stringsList, nil
		}
		return nil, fmt.Errorf("%s must be a list", field)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s item must be a string", field)
		}
		out = append(out, text)
	}
	return out, nil
}
func mapsToAny(values []map[string]any) []any {
	out := make([]any, len(values))
	for i := range values {
		out[i] = values[i]
	}
	return out
}
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
func deepCopyMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		switch typed := item.(type) {
		case map[string]any:
			out[key] = deepCopyMap(typed)
		case []any:
			copy := make([]any, len(typed))
			for i, child := range typed {
				if childMap, ok := child.(map[string]any); ok {
					copy[i] = deepCopyMap(childMap)
				} else {
					copy[i] = child
				}
			}
			out[key] = copy
		default:
			out[key] = item
		}
	}
	return out
}
