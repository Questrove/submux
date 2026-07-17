package compiler

import (
	"fmt"
	"net"
	"strings"

	"gopkg.in/yaml.v3"
)

const RuntimeContractMihomoAgentV1 = "mihomo-agent/v1"

type RuntimeRisk struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RuntimeAnalysis struct {
	Contract         string        `json:"contract,omitempty"`
	SelectableGroups []string      `json:"selectable_groups,omitempty"`
	Risks            []RuntimeRisk `json:"risks,omitempty"`
	RequiresNetAdmin bool          `json:"requires_net_admin"`
	HasAgentContract bool          `json:"has_agent_contract"`
}

func ValidateRuntimeContract(engine, contract string) error {
	contract = strings.TrimSpace(contract)
	if contract == "" {
		return nil
	}
	if contract != RuntimeContractMihomoAgentV1 {
		return fmt.Errorf("unsupported runtime contract %q", contract)
	}
	if engine != EngineMihomo {
		return fmt.Errorf("runtime contract %q requires the mihomo engine", contract)
	}
	return nil
}

// AnalyzeMihomoRuntime reports deployment risks without rewriting or rejecting
// an otherwise valid custom configuration. It analyzes the immutable base
// artifact; messages for Agent-owned fields make their deployment override
// explicit, while additional endpoints remain the template owner's choice.
func AnalyzeMihomoRuntime(content, contract string) (RuntimeAnalysis, error) {
	result := RuntimeAnalysis{Contract: contract, HasAgentContract: contract == RuntimeContractMihomoAgentV1}
	var root map[string]any
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return result, fmt.Errorf("parse mihomo config: %w", err)
	}
	if enabled(root["tun"]) {
		result.RequiresNetAdmin = true
		result.Risks = append(result.Risks, RuntimeRisk{Code: "tun_enabled", Message: "TUN is enabled and may require explicit host network privileges"})
	}
	for _, key := range []string{"redir-port", "tproxy-port"} {
		if nonzero(root[key]) {
			result.RequiresNetAdmin = true
			result.Risks = append(result.Risks, RuntimeRisk{Code: strings.ReplaceAll(key, "-", "_"), Message: key + " enables transparent proxy behavior"})
		}
	}
	if addr, ok := root["external-controller"].(string); ok && addr != "" && !loopbackListener(addr) {
		result.Risks = append(result.Risks, RuntimeRisk{Code: "external_controller_overridden", Message: "the base config requests a non-loopback Mihomo controller; Agent deployments override it with an authenticated loopback listener"})
	}
	for _, endpoint := range []struct {
		key, code, message string
	}{
		{"external-controller-unix", "external_controller_unix_enabled", "the base config enables a Mihomo Unix socket API that does not authenticate with the controller secret"},
		{"external-controller-pipe", "external_controller_pipe_enabled", "the base config enables a Mihomo named pipe API that does not authenticate with the controller secret"},
		{"external-controller-tls", "external_controller_tls_enabled", "the base config enables an additional Mihomo HTTPS API listener"},
		{"external-doh-server", "external_doh_server_enabled", "the base config enables a DoH endpoint that does not authenticate with the controller secret"},
	} {
		if nonemptyString(root[endpoint.key]) {
			result.Risks = append(result.Risks, RuntimeRisk{Code: endpoint.code, Message: endpoint.message})
		}
	}
	if _, ok := root["external-controller-cors"].(map[string]any); ok {
		result.Risks = append(result.Risks, RuntimeRisk{Code: "external_controller_cors_configured", Message: "the base config customizes browser access to the Mihomo controller"})
	}
	if nonemptyString(root["external-ui"]) || nonemptyString(root["external-ui-url"]) {
		result.Risks = append(result.Risks, RuntimeRisk{Code: "external_ui_enabled", Message: "the base config enables or downloads a Mihomo external UI"})
	}
	allowLAN, _ := root["allow-lan"].(bool)
	bind, _ := root["bind-address"].(string)
	if allowLAN || bind == "*" || (bind != "" && !loopbackHost(bind)) {
		result.Risks = append(result.Risks, RuntimeRisk{Code: "proxy_listener_exposed", Message: "the proxy listener may be reachable beyond the local host"})
	}
	if groups, ok := root["proxy-groups"].([]any); ok {
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok || group["type"] != "select" {
				continue
			}
			if name, ok := group["name"].(string); ok && name != "" {
				result.SelectableGroups = append(result.SelectableGroups, name)
			}
		}
	}
	return result, nil
}

func nonemptyString(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}

func enabled(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case map[string]any:
		value, _ := typed["enable"].(bool)
		return value
	default:
		return false
	}
}

func nonzero(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed != 0
	case uint64:
		return typed != 0
	case float64:
		return typed != 0
	default:
		return false
	}
}

func loopbackListener(value string) bool {
	host, _, err := net.SplitHostPort(value)
	return err == nil && loopbackHost(host)
}

func loopbackHost(value string) bool {
	value = strings.Trim(value, "[]")
	if strings.EqualFold(value, "localhost") {
		return true
	}
	ip := net.ParseIP(value)
	return ip != nil && ip.IsLoopback()
}
