package runtimeprivacy

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"submux/internal/runtimeapi"
)

const replacement = "[REDACTED]"

var (
	urlPattern              = regexp.MustCompile(`(?i)\b(?:https?|socks5?)://[^\s"'<>]+`)
	headerPattern           = regexp.MustCompile(`(?im)^(.*?["']?\b(?:authorization|proxy-authorization|cookie|set-cookie)["']?\s*:\s*).*$`)
	secretAssignmentPattern = regexp.MustCompile(`(?im)^(.*?["']?\b(?:password|passwd|token|access[_-]?token|refresh[_-]?token|secret|private[_-]?key|api[_-]?key|client[_-]?secret|uuid|auth(?:entication)?|psk)["']?\s*[:=]\s*).*$`)
	privateKeyPattern       = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	windowsPathPattern      = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|\\\\)[^\s"'<>|]+`)
	posixPathPattern        = regexp.MustCompile(`(^|[\s="'(])(/[A-Za-z0-9._~@%+,-]+(?:/[A-Za-z0-9._~@%+,-]+)+)`)
)

func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return replacement
	}
	parsed.User = nil
	if parsed.RawQuery != "" {
		parts := strings.Split(parsed.RawQuery, "&")
		for index, part := range parts {
			key, _, found := strings.Cut(part, "=")
			if !found {
				parts[index] = key + "=" + url.QueryEscape(replacement)
				continue
			}
			parts[index] = key + "=" + url.QueryEscape(replacement)
		}
		parsed.RawQuery = strings.Join(parts, "&")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		parsed.Fragment = replacement
		parsed.RawFragment = ""
	}
	return parsed.String()
}

func RedactText(value string) string {
	if value == "" {
		return ""
	}
	value = privateKeyPattern.ReplaceAllString(value, replacement)
	value = headerPattern.ReplaceAllString(value, `${1}`+replacement)
	value = secretAssignmentPattern.ReplaceAllString(value, `${1}`+replacement)
	value = urlPattern.ReplaceAllStringFunc(value, RedactURL)
	value = windowsPathPattern.ReplaceAllString(value, "<local-path>")
	value = posixPathPattern.ReplaceAllString(value, `${1}<local-path>`)
	return value
}

func RedactError(err error) string {
	if err == nil {
		return ""
	}
	return RedactText(err.Error())
}

func SanitizeSnapshot(snapshot runtimeapi.Snapshot) runtimeapi.Snapshot {
	snapshot.Runtime.Fault = sanitizeFault(snapshot.Runtime.Fault)
	snapshot.Mihomo.Fault = sanitizeFault(snapshot.Mihomo.Fault)
	for index := range snapshot.Sources.Items {
		snapshot.Sources.Items[index].RedactedTarget = RedactText(snapshot.Sources.Items[index].RedactedTarget)
		snapshot.Sources.Items[index].LastRefreshResult = RedactText(snapshot.Sources.Items[index].LastRefreshResult)
		snapshot.Sources.Items[index].FailureClass = RedactText(snapshot.Sources.Items[index].FailureClass)
	}
	snapshot.Sources.LastRefreshResult = RedactText(snapshot.Sources.LastRefreshResult)
	return snapshot
}

func SanitizeCandidatePreview(preview runtimeapi.CandidatePreview) runtimeapi.CandidatePreview {
	preview.CandidateYAML = RedactText(preview.CandidateYAML)
	for index := range preview.FieldOrigins {
		preview.FieldOrigins[index].Path = RedactText(preview.FieldOrigins[index].Path)
		preview.FieldOrigins[index].Origin = RedactText(preview.FieldOrigins[index].Origin)
		preview.FieldOrigins[index].ReplacedOrigin = RedactText(preview.FieldOrigins[index].ReplacedOrigin)
	}
	return preview
}

func SanitizeOperation(operation runtimeapi.Operation) runtimeapi.Operation {
	operation.ClientType = RedactText(operation.ClientType)
	operation.ClientVersion = RedactText(operation.ClientVersion)
	if operation.Error != nil {
		copy := *operation.Error
		copy.Message = RedactText(copy.Message)
		operation.Error = &copy
	}
	return operation
}

func SanitizeAudit(record runtimeapi.AuditRecord) runtimeapi.AuditRecord {
	record.Actor = RedactText(record.Actor)
	record.ClientType = RedactText(record.ClientType)
	record.ClientVersion = RedactText(record.ClientVersion)
	record.Action = RedactText(record.Action)
	record.ObjectID = RedactText(record.ObjectID)
	record.Stage = RedactText(record.Stage)
	record.Result = RedactText(record.Result)
	if record.Error != nil {
		copy := *record.Error
		copy.Message = RedactText(copy.Message)
		record.Error = &copy
	}
	return record
}

func SanitizeEvent(event runtimeapi.Event) runtimeapi.Event {
	event.Type = RedactText(event.Type)
	event.OperationID = RedactText(event.OperationID)
	event.AdditionalPayload = RedactJSON(event.AdditionalPayload)
	return event
}

func RedactJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		encoded, _ := json.Marshal(RedactText(string(raw)))
		return encoded
	}
	redacted := redactJSONValue(value)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return nil
	}
	return encoded
}

func redactJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveKey(key) {
				typed[key] = replacement
				continue
			}
			typed[key] = redactJSONValue(child)
		}
		return typed
	case []any:
		for index := range typed {
			typed[index] = redactJSONValue(typed[index])
		}
		return typed
	case string:
		return RedactText(typed)
	default:
		return typed
	}
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, fragment := range []string{
		"authorization",
		"cookie",
		"password",
		"token",
		"secret",
		"private_key",
		"config_body",
		"config_yaml",
		"local_path",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func sanitizeFault(fault *runtimeapi.Fault) *runtimeapi.Fault {
	if fault == nil {
		return nil
	}
	copy := *fault
	copy.Message = RedactText(copy.Message)
	return &copy
}
