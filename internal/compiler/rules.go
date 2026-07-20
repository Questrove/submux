package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"submux/internal/rulecatalog"
	"submux/internal/store"
)

const builtinRuleProfilesCatalogVersion = "builtin_rule_profiles_v1"

var safeRuleName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func (s *Service) EnsureBuiltinRuleProfiles() error {
	profiles, err := s.store.ListRuleProfiles()
	if err != nil {
		return err
	}
	var defaultProfile store.RuleProfile
	for _, profile := range profiles {
		if profile.CatalogCommit == "" {
			profile.CatalogCommit = rulecatalog.Commit()
			if _, err := s.store.SaveRuleProfile(profile); err != nil {
				return err
			}
		}
		if profile.Key == "default" {
			defaultProfile = profile
		}
	}
	if defaultProfile.ID == 0 {
		catalog, catalogErr := rulecatalog.ActiveCatalog(s.store)
		if catalogErr != nil {
			return catalogErr
		}
		defaultProfile = rulecatalog.DefaultProfileFor(catalog)
		id, err := s.store.SaveRuleProfile(defaultProfile)
		if err != nil {
			return err
		}
		defaultProfile.ID = id
	}
	if err := s.ValidateRuleProfile(defaultProfile); err != nil {
		return fmt.Errorf("default rule profile: %w", err)
	}

	subscriptionsChanged := false
	subscriptions, err := s.store.ListOutputSubscriptions()
	if err != nil {
		return err
	}
	for _, subscription := range subscriptions {
		if subscription.Engine != EngineMihomo || subscription.RuleProfileID != 0 {
			continue
		}
		subscription.RuleProfileID = defaultProfile.ID
		if _, err := s.store.SaveOutputSubscription(subscription); err != nil {
			return err
		}
		subscriptionsChanged = true
	}
	if subscriptionsChanged {
		_ = s.RebuildAll()
	}
	return s.store.SetSetting(builtinRuleProfilesCatalogVersion, "1")
}

func ValidateRuleProfile(profile store.RuleProfile) error {
	return ValidateRuleProfileWithCatalog(profile, rulecatalog.Catalog())
}

func (s *Service) ValidateRuleProfile(profile store.RuleProfile) error {
	catalog, err := rulecatalog.CatalogAt(s.store, profile.CatalogCommit)
	if err != nil {
		return err
	}
	return ValidateRuleProfileWithCatalog(profile, catalog)
}

func ValidateRuleProfileWithCatalog(profile store.RuleProfile, catalog rulecatalog.Snapshot) error {
	if len(profile.Rules) > 4096 {
		return fmt.Errorf("a rule profile may contain at most 4096 catalog rules")
	}
	if len(profile.CustomRules) > 256 {
		return fmt.Errorf("a rule profile may contain at most 256 custom rules")
	}
	if !rulecatalog.ValidAction(profile.FallbackAction) || profile.FallbackAction == rulecatalog.ActionReject {
		return fmt.Errorf("fallback action must be direct, proxy or media")
	}
	seen := map[string]bool{}
	for _, selection := range profile.Rules {
		if seen[selection.Key] {
			return fmt.Errorf("rule %q is selected more than once", selection.Key)
		}
		seen[selection.Key] = true
		if _, ok := rulecatalog.LookupIn(catalog, selection.Key); !ok {
			return fmt.Errorf("unknown catalog rule %q", selection.Key)
		}
		if !rulecatalog.ValidAction(selection.Action) {
			return fmt.Errorf("rule %q has invalid action %q", selection.Key, selection.Action)
		}
	}
	for index, custom := range profile.CustomRules {
		if !rulecatalog.ValidAction(custom.Action) {
			return fmt.Errorf("custom rule %d has invalid action %q", index+1, custom.Action)
		}
		if err := validateCustomRule(custom); err != nil {
			return fmt.Errorf("custom rule %d: %w", index+1, err)
		}
	}
	return nil
}

func validateCustomRule(rule store.CustomRule) error {
	value := strings.TrimSpace(rule.Value)
	if value == "" || len(value) > 512 || strings.ContainsAny(value, ",\r\n") {
		return fmt.Errorf("value is empty, too long, or contains a separator")
	}
	switch strings.ToUpper(strings.TrimSpace(rule.Type)) {
	case "DOMAIN", "DOMAIN-SUFFIX":
		if strings.ContainsAny(value, " /\\") {
			return fmt.Errorf("domain value is invalid")
		}
	case "DOMAIN-KEYWORD", "DOMAIN-WILDCARD":
		// Mihomo validates the keyword or wildcard syntax when the generated
		// configuration is applied.
	case "IP-CIDR", "IP-CIDR6":
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("CIDR value is invalid")
		}
		if strings.EqualFold(rule.Type, "IP-CIDR") && !prefix.Addr().Is4() {
			return fmt.Errorf("IP-CIDR requires an IPv4 prefix")
		}
		if strings.EqualFold(rule.Type, "IP-CIDR6") && !prefix.Addr().Is6() {
			return fmt.Errorf("IP-CIDR6 requires an IPv6 prefix")
		}
	default:
		return fmt.Errorf("type must be DOMAIN, DOMAIN-SUFFIX, DOMAIN-KEYWORD, DOMAIN-WILDCARD, IP-CIDR or IP-CIDR6")
	}
	return nil
}

func applyMihomoRuleProfile(root map[string]any, profile store.RuleProfile) error {
	return applyMihomoRuleProfileWithCatalog(root, profile, rulecatalog.Catalog())
}

func applyMihomoRuleProfileWithCatalog(root map[string]any, profile store.RuleProfile, catalog rulecatalog.Snapshot) error {
	if err := ValidateRuleProfileWithCatalog(profile, catalog); err != nil {
		return err
	}
	providers := make(map[string]any, len(profile.Rules))
	rules := make([]any, 0, len(profile.CustomRules)+len(profile.Rules)+1)
	providerNames := make(map[string]string, len(profile.Rules))
	for _, custom := range profile.CustomRules {
		target := mihomoRuleTarget(custom.Action)
		rule := strings.ToUpper(custom.Type) + "," + strings.TrimSpace(custom.Value) + "," + target
		if strings.EqualFold(custom.Type, "IP-CIDR") || strings.EqualFold(custom.Type, "IP-CIDR6") {
			rule += ",no-resolve"
		}
		rules = append(rules, rule)
	}
	for _, selection := range profile.Rules {
		entry, _ := rulecatalog.LookupIn(catalog, selection.Key)
		providerName := ruleProviderName(entry)
		providerNames[selection.Key] = providerName
		providers[providerName] = map[string]any{
			"type":     "http",
			"behavior": entry.Behavior,
			"format":   "mrs",
			"interval": 86400,
			"proxy":    "PROXY",
			"url":      ruleProviderURL(entry, catalog.Commit),
		}
		rule := "RULE-SET," + providerName + "," + mihomoRuleTarget(selection.Action)
		if entry.Behavior == "ipcidr" {
			rule += ",no-resolve"
		}
		rules = append(rules, rule)
	}
	rules = append(rules, "MATCH,"+mihomoRuleTarget(profile.FallbackAction))
	root["rule-providers"] = providers
	root["rules"] = rules
	applyMihomoDNSPolicies(root, profile, providerNames, catalog)
	return nil
}

func applyMihomoDNSPolicies(root map[string]any, profile store.RuleProfile, providerNames map[string]string, catalog rulecatalog.Snapshot) {
	dns, ok := root["dns"].(map[string]any)
	if !ok {
		return
	}
	policy := map[string]any{}
	if existing, ok := dns["nameserver-policy"].(map[string]any); ok {
		for key, value := range existing {
			if !strings.HasPrefix(key, "rule-set:") {
				policy[key] = value
			}
		}
	}
	direct := dnsServerList(dns["direct-nameserver"], []any{"https://doh.pub/dns-query", "https://dns.alidns.com/dns-query"})
	proxy := []any{"https://1.1.1.1/dns-query#PROXY", "https://8.8.8.8/dns-query#PROXY"}
	media := []any{"https://1.1.1.1/dns-query#MEDIA", "https://8.8.8.8/dns-query#MEDIA"}
	for _, custom := range profile.CustomRules {
		key := customDNSPolicyKey(custom)
		if key != "" {
			policy[key] = dnsPolicyForAction(custom.Action, direct, proxy, media)
		}
	}
	for _, selection := range profile.Rules {
		entry, ok := rulecatalog.LookupIn(catalog, selection.Key)
		if !ok || entry.Behavior != "domain" {
			continue
		}
		policy["rule-set:"+providerNames[selection.Key]] = dnsPolicyForAction(selection.Action, direct, proxy, media)
	}
	dns["nameserver-policy"] = policy
}

func dnsServerList(value any, fallback []any) []any {
	if list, ok := value.([]any); ok && len(list) > 0 {
		return append([]any(nil), list...)
	}
	return append([]any(nil), fallback...)
}

func dnsPolicyForAction(action string, direct, proxy, media []any) any {
	switch action {
	case rulecatalog.ActionDirect:
		return append([]any(nil), direct...)
	case rulecatalog.ActionMedia:
		return append([]any(nil), media...)
	case rulecatalog.ActionReject:
		return "rcode://success"
	default:
		return append([]any(nil), proxy...)
	}
}

func customDNSPolicyKey(rule store.CustomRule) string {
	switch strings.ToUpper(rule.Type) {
	case "DOMAIN":
		return strings.TrimSpace(rule.Value)
	case "DOMAIN-SUFFIX":
		return "+." + strings.TrimLeft(strings.TrimSpace(rule.Value), ".")
	default:
		return ""
	}
}

func mihomoRuleTarget(action string) string {
	switch action {
	case rulecatalog.ActionDirect:
		return "DIRECT"
	case rulecatalog.ActionMedia:
		return "MEDIA"
	case rulecatalog.ActionReject:
		return "REJECT"
	default:
		return "PROXY"
	}
}

func ruleProviderName(entry rulecatalog.Entry) string {
	base := safeRuleName.ReplaceAllString(entry.Name, "_")
	base = strings.Trim(base, "_")
	if base == "" {
		base = "rule"
	}
	if len(base) > 42 {
		base = base[:42]
	}
	sum := sha256.Sum256([]byte(entry.Key))
	return entry.Kind + "_" + base + "_" + hex.EncodeToString(sum[:8])
}

func ruleProviderURL(entry rulecatalog.Entry, commit string) string {
	return "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/" + commit + "/geo/" + entry.Kind + "/" + url.PathEscape(entry.Name) + ".mrs"
}
