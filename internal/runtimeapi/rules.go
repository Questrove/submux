package runtimeapi

const (
	RuleViewApplied   = "applied"
	RuleViewCandidate = "candidate"

	RuleOriginSource               = "source"
	RuleOriginAdvancedOverride     = "advanced_override"
	RuleOriginRuntime              = "runtime"
	RuleOriginCurrentConfiguration = "current_configuration"

	RuleFilterMaxLength = 256
)

type RuleQuery struct {
	Content string `json:"content,omitempty"`
	Type    string `json:"type,omitempty"`
	Target  string `json:"target,omitempty"`
}

type FinalRule struct {
	Order     int    `json:"order"`
	Type      string `json:"type"`
	Condition string `json:"condition"`
	Target    string `json:"target"`
	Origin    string `json:"origin"`
	Content   string `json:"content"`
}

type RuleSet struct {
	View                string      `json:"view"`
	ConfigurationSHA256 string      `json:"configuration_sha256"`
	SourceID            string      `json:"source_id,omitempty"`
	Items               []FinalRule `json:"items"`
	Total               int         `json:"total"`
}
