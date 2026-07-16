package lifecycle

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"submux/internal/store"
)

const (
	EntitlementUnknown   = "unknown"
	EntitlementActive    = "active"
	EntitlementExpiring  = "expiring"
	EntitlementExpired   = "expired"
	EntitlementExhausted = "quota_exhausted"

	FreshnessNever = "never_refreshed"
	FreshnessFresh = "fresh"
	FreshnessStale = "stale"
	FreshnessError = "refresh_error"

	trafficConflictMinTolerance = 100 << 20
	trafficConflictMaxTolerance = 1 << 30
)

type Status struct {
	Entitlement      string   `json:"entitlement"`
	Freshness        string   `json:"freshness"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
	NeverExpires     bool     `json:"never_expires,omitempty"`
	DaysRemaining    *int     `json:"days_remaining,omitempty"`
	RemainingBytes   *int64   `json:"remaining_bytes,omitempty"`
	TotalBytes       *int64   `json:"total_bytes,omitempty"`
	UnlimitedTraffic bool     `json:"unlimited_traffic,omitempty"`
	Policy           string   `json:"policy"`
	MetadataStale    bool     `json:"metadata_stale"`
	Enforceable      bool     `json:"enforceable"`
	Conflicts        []string `json:"conflicts,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

var (
	remainingPattern   = regexp.MustCompile(`(?i)^(?:剩余流量|流量剩余|traffic\s*remaining|remaining\s*traffic)\s*[:：]?\s*([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb|tb|kib|mib|gib|tib)\s*$`)
	combinedPattern    = regexp.MustCompile(`(?i)^(?:剩余流量|流量剩余)\s*[:：]?\s*([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb|tb|kib|mib|gib|tib)\s*[|/]\s*(?:总流量)\s*[:：]?\s*([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb|tb|kib|mib|gib|tib)\s*$`)
	bandwidthPattern   = regexp.MustCompile(`(?i)^bandwidth\s*[:：]?\s*([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb|tb|kib|mib|gib|tib)\s*/\s*([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb|tb|kib|mib|gib|tib)\s*$`)
	expiryPattern      = regexp.MustCompile(`(?i)^(?:到期时间|过期时间|套餐到期|expire(?:s|d)?(?:\s+at)?|expiration)\s*[:：]?\s*(\d{4})[-/](\d{1,2})[-/](\d{1,2})(?:\s+(\d{1,2}):(\d{2})(?::(\d{2}))?)?\s*$`)
	neverExpiryPattern = regexp.MustCompile(`(?i)^(?:到期时间|过期时间|套餐到期|有效期|expire(?:s|d)?(?:\s+at)?|expiration)\s*[:：]?\s*(?:长期有效|永久有效|永不过期|无限期|不过期|never\s+expires?|lifetime)\s*$`)
)

func NormalizeLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufe0f':
			return -1
		case '｜':
			return '|'
		case '／':
			return '/'
		case '\u00a0':
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(value)
	value = strings.TrimLeftFunc(value, func(r rune) bool {
		return unicode.IsSymbol(r) || r == '-' || r == '_' || r == '|' || r == '·'
	})
	return strings.TrimSpace(value)
}

func ClassifyLabel(raw string) *store.NodeNotice {
	label := NormalizeLabel(raw)
	if match := combinedPattern.FindStringSubmatch(label); match != nil {
		remaining, ok1 := parseBytes(match[1], match[2])
		total, ok2 := parseBytes(match[3], match[4])
		if ok1 && ok2 {
			return &store.NodeNotice{Type: "traffic_remaining", RawText: raw, Value: remaining, TotalValue: total, Unit: "bytes", Confidence: "high"}
		}
	}
	if match := remainingPattern.FindStringSubmatch(label); match != nil {
		if value, ok := parseBytes(match[1], match[2]); ok {
			return &store.NodeNotice{Type: "traffic_remaining", RawText: raw, Value: value, Unit: "bytes", Confidence: "high"}
		}
	}
	if match := bandwidthPattern.FindStringSubmatch(label); match != nil {
		used, ok1 := parseBytes(match[1], match[2])
		total, ok2 := parseBytes(match[3], match[4])
		if ok1 && ok2 && total >= used {
			return &store.NodeNotice{Type: "traffic_remaining", RawText: raw, Value: total - used, TotalValue: total, Unit: "bytes", Confidence: "high"}
		}
	}
	if neverExpiryPattern.MatchString(label) {
		return &store.NodeNotice{Type: "expires_never", RawText: raw, TextValue: "long_term", Confidence: "high"}
	}
	if match := expiryPattern.FindStringSubmatch(label); match != nil {
		if value, ok := parseExpiry(match); ok {
			return &store.NodeNotice{Type: "expires_at", RawText: raw, TextValue: value, Confidence: "high"}
		}
	}
	lower := strings.ToLower(label)
	for _, prefix := range []string{"官网", "官方网站", "客服", "公告", "website", "support"} {
		if strings.HasPrefix(lower, prefix) {
			return &store.NodeNotice{Type: "announcement", RawText: raw, TextValue: label, Confidence: "medium"}
		}
	}
	return nil
}

func parseBytes(number, unit string) (int64, bool) {
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	powers := map[string]int{"B": 0, "KB": 1, "KIB": 1, "MB": 2, "MIB": 2, "GB": 3, "GIB": 3, "TB": 4, "TIB": 4}
	power, ok := powers[strings.ToUpper(unit)]
	if !ok {
		return 0, false
	}
	bytes := value * math.Pow(1024, float64(power))
	if bytes > math.MaxInt64 {
		return 0, false
	}
	return int64(math.Round(bytes)), true
}

func parseExpiry(match []string) (string, bool) {
	parts := make([]int, 6)
	for index := 1; index <= 6; index++ {
		if match[index] == "" {
			continue
		}
		value, err := strconv.Atoi(match[index])
		if err != nil {
			return "", false
		}
		parts[index-1] = value
	}
	if match[4] == "" {
		parts[3], parts[4], parts[5] = 23, 59, 59
	}
	value := time.Date(parts[0], time.Month(parts[1]), parts[2], parts[3], parts[4], parts[5], 0, time.UTC)
	if value.Year() != parts[0] || int(value.Month()) != parts[1] || value.Day() != parts[2] {
		return "", false
	}
	return value.Format(time.RFC3339), true
}

// ParseSubscriptionUserinfo parses the de-facto upload/download/total/expire
// response header without assuming that missing fields are zero.
func ParseSubscriptionUserinfo(header string, now time.Time) (store.SubscriptionMetadata, bool) {
	metadata := store.SubscriptionMetadata{Provenance: map[string]string{}}
	found := false
	for _, part := range strings.Split(header, ";") {
		pair := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(pair) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(pair[0]))
		value, err := strconv.ParseInt(strings.TrimSpace(pair[1]), 10, 64)
		if err != nil || value < 0 {
			continue
		}
		switch key {
		case "upload":
			metadata.Upload, metadata.Provenance["upload"], found = value, "header", true
		case "download":
			metadata.Download, metadata.Provenance["download"], found = value, "header", true
		case "total":
			metadata.Total, metadata.Provenance["total"], found = value, "header", true
		case "expire":
			found = true
			if value > 0 {
				metadata.ExpiresAt = time.Unix(value, 0).UTC().Format(time.RFC3339)
				metadata.Provenance["expires_at"] = "header"
			}
		}
	}
	_, upload := metadata.Provenance["upload"]
	_, download := metadata.Provenance["download"]
	_, total := metadata.Provenance["total"]
	// The ecosystem does not define total=0 consistently. Providers commonly
	// use it for an unlimited or unspecified quota, so it cannot prove that the
	// subscription is exhausted.
	if upload && download && total && metadata.Total > 0 {
		metadata.Remaining = metadata.Total - metadata.Upload - metadata.Download
		metadata.Provenance["remaining"] = "header"
	}
	if found {
		metadata.ObservedAt = now.UTC().Format(time.RFC3339)
	}
	return metadata, found
}

func MergeMetadata(previous, header store.SubscriptionMetadata, headerOK bool, notices []*store.NodeNotice, now time.Time) store.SubscriptionMetadata {
	result := previous
	if result.Provenance == nil {
		result.Provenance = map[string]string{}
	}
	result.Conflicts = nil
	observed := false
	updated := map[string]bool{}
	headerUnlimited := headerOK && header.Provenance["total"] == "header" && header.Total == 0
	if headerOK {
		observed = true
		for _, field := range []string{"upload", "download", "total", "remaining", "expires_at"} {
			if header.Provenance[field] != "" {
				copyMetadataField(&result, header, field, "header")
				updated[field] = true
			}
		}
		if headerUnlimited {
			// An explicit zero total supersedes an older finite quota. Do not let
			// a stale remaining value or a node-name notice turn it into exhaustion.
			result.Remaining = 0
			delete(result.Provenance, "remaining")
			updated["remaining"] = true
		}
	}
	for _, notice := range notices {
		if notice == nil || notice.Confidence != "high" {
			continue
		}
		observed = true
		switch notice.Type {
		case "traffic_remaining":
			if headerUnlimited {
				continue
			}
			mergeNoticeInt(&result, "remaining", notice.Value)
			updated["remaining"] = true
			if notice.TotalValue > 0 {
				mergeNoticeInt(&result, "total", notice.TotalValue)
				updated["total"] = true
			}
		case "expires_at":
			mergeNoticeString(&result, "expires_at", notice.TextValue)
			updated["expires_at"] = true
		case "expires_never":
			mergeNeverExpires(&result)
			updated["expires_at"] = true
		}
	}
	result.Stale = false
	for _, field := range []string{"remaining", "expires_at"} {
		if previous.Provenance[field] != "" && !updated[field] {
			result.Stale = true
		}
	}
	if !observed && len(result.Provenance) > 0 {
		result.Stale = true
	}
	if observed {
		result.ObservedAt = now.UTC().Format(time.RFC3339)
	}
	return result
}

func copyMetadataField(target *store.SubscriptionMetadata, source store.SubscriptionMetadata, field, provenance string) {
	switch field {
	case "upload":
		target.Upload = source.Upload
	case "download":
		target.Download = source.Download
	case "total":
		target.Total = source.Total
	case "remaining":
		target.Remaining = source.Remaining
	case "expires_at":
		target.ExpiresAt = source.ExpiresAt
	}
	target.Provenance[field] = provenance
}

func mergeNoticeInt(metadata *store.SubscriptionMetadata, field string, value int64) {
	if metadata.Provenance[field] == "header" {
		var current int64
		if field == "remaining" {
			current = metadata.Remaining
		} else {
			current = metadata.Total
		}
		if trafficValuesConflict(current, value) {
			metadata.Conflicts = append(metadata.Conflicts, fmt.Sprintf("%s differs between header and node name", field))
		}
		return
	}
	if field == "remaining" {
		metadata.Remaining = value
	} else {
		metadata.Total = value
	}
	metadata.Provenance[field] = "node_name"
}

func trafficValuesConflict(headerValue, nodeValue int64) bool {
	difference := math.Abs(float64(headerValue) - float64(nodeValue))
	baseline := math.Max(math.Abs(float64(headerValue)), math.Abs(float64(nodeValue)))
	tolerance := baseline * 0.01
	if tolerance < trafficConflictMinTolerance {
		tolerance = trafficConflictMinTolerance
	}
	if tolerance > trafficConflictMaxTolerance {
		tolerance = trafficConflictMaxTolerance
	}
	return difference > tolerance
}

func mergeNoticeString(metadata *store.SubscriptionMetadata, field, value string) {
	if metadata.Provenance[field] == "header" {
		if expiryValuesConflict(metadata.ExpiresAt, value) {
			metadata.Conflicts = append(metadata.Conflicts, fmt.Sprintf("%s differs between header and node name", field))
		}
		return
	}
	metadata.ExpiresAt = value
	metadata.Provenance[field] = "node_name"
}

func expiryValuesConflict(headerValue, nodeValue string) bool {
	if headerValue == nodeValue {
		return false
	}
	headerTime, headerErr := time.Parse(time.RFC3339, headerValue)
	nodeTime, nodeErr := time.Parse(time.RFC3339, nodeValue)
	if headerErr != nil || nodeErr != nil {
		return true
	}
	headerYear, headerMonth, headerDay := headerTime.UTC().Date()
	nodeYear, nodeMonth, nodeDay := nodeTime.UTC().Date()
	return headerYear != nodeYear || headerMonth != nodeMonth || headerDay != nodeDay
}

func mergeNeverExpires(metadata *store.SubscriptionMetadata) {
	if metadata.Provenance["expires_at"] == "header" {
		if metadata.ExpiresAt != "" {
			metadata.Conflicts = append(metadata.Conflicts, "expires_at differs between header and node name")
		}
		return
	}
	metadata.ExpiresAt = ""
	metadata.Provenance["expires_at"] = "node_name"
}

func Evaluate(source store.Source, cache store.Cache, now time.Time) Status {
	metadata := cache.Metadata
	unlimitedTraffic := metadata.Provenance["total"] == "header" && metadata.Total == 0
	status := Status{
		Entitlement: EntitlementUnknown, Policy: source.LifecyclePolicy,
		ExpiresAt: metadata.ExpiresAt, MetadataStale: metadata.Stale,
		NeverExpires:     metadata.Provenance["expires_at"] == "node_name" && metadata.ExpiresAt == "",
		UnlimitedTraffic: unlimitedTraffic,
		Conflicts:        append([]string(nil), metadata.Conflicts...),
	}
	if cache.LastSuccessAt == "" {
		status.Freshness = FreshnessNever
	} else if cache.LastError != "" {
		status.Freshness = FreshnessError
	} else if metadata.Stale {
		status.Freshness = FreshnessStale
	} else {
		status.Freshness = FreshnessFresh
	}
	if !unlimitedTraffic && metadata.Provenance["remaining"] != "" {
		remaining := metadata.Remaining
		status.RemainingBytes = &remaining
	}
	if !unlimitedTraffic && metadata.Provenance["total"] != "" {
		total := metadata.Total
		status.TotalBytes = &total
	}
	expired := false
	if metadata.ExpiresAt != "" {
		if expiry, err := time.Parse(time.RFC3339, metadata.ExpiresAt); err == nil {
			days := int(math.Ceil(expiry.Sub(now).Hours() / 24))
			status.DaysRemaining = &days
			if !now.Before(expiry) {
				status.Entitlement, expired = EntitlementExpired, true
			} else if days <= source.WarnBeforeDays {
				status.Entitlement = EntitlementExpiring
			} else {
				status.Entitlement = EntitlementActive
			}
		}
	}
	if !expired && status.RemainingBytes != nil && *status.RemainingBytes <= 0 {
		status.Entitlement = EntitlementExhausted
	}
	if status.Entitlement == EntitlementUnknown && len(metadata.Provenance) > 0 {
		status.Entitlement = EntitlementActive
	}
	reasonField := ""
	if status.Entitlement == EntitlementExpired {
		reasonField = "expires_at"
	} else if status.Entitlement == EntitlementExhausted {
		reasonField = "remaining"
	}
	if reasonField != "" {
		status.Enforceable = metadata.Provenance[reasonField] != "node_name" || source.TrustNodeNotices
	}
	if status.Entitlement == EntitlementExpiring {
		status.Warnings = append(status.Warnings, "upstream subscription is expiring")
	} else if status.Entitlement == EntitlementExpired {
		status.Warnings = append(status.Warnings, "upstream subscription has expired")
	} else if status.Entitlement == EntitlementExhausted {
		status.Warnings = append(status.Warnings, "upstream subscription quota is exhausted")
	}
	if cache.LastError != "" {
		status.Warnings = append(status.Warnings, "upstream refresh failed")
	}
	return status
}

func ShouldExclude(source store.Source, status Status) bool {
	return source.LifecyclePolicy == store.LifecycleStrict && status.Enforceable &&
		(status.Entitlement == EntitlementExpired || status.Entitlement == EntitlementExhausted)
}
