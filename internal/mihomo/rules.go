package mihomo

import (
	"errors"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const CandidateOriginCurrentConfiguration = "current_configuration"

var runtimeHealthRuleContents = []string{
	"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
	"IP-CIDR6,::1/128,DIRECT,no-resolve",
}

type FinalRule struct {
	Order     int
	Type      string
	Condition string
	Target    string
	Origin    string
	Content   string
}

func FinalRules(candidate, source []byte, userOrigin string) ([]FinalRule, error) {
	contents, err := configurationRuleContents(candidate, "candidate configuration")
	if err != nil {
		return nil, err
	}
	runtimeRuleCount := 0
	for runtimeRuleCount < len(runtimeHealthRuleContents) &&
		runtimeRuleCount < len(contents) &&
		contents[runtimeRuleCount] == runtimeHealthRuleContents[runtimeRuleCount] {
		runtimeRuleCount++
	}
	if userOrigin == "" {
		userOrigin = CandidateOriginCurrentConfiguration
		if len(source) > 0 {
			sourceContents, sourceErr := configurationRuleContents(source, "applied source configuration")
			if sourceErr != nil {
				return nil, sourceErr
			}
			if slices.Equal(contents[runtimeRuleCount:], sourceContents) {
				userOrigin = CandidateOriginSource
			} else {
				userOrigin = CandidateOriginOverride
			}
		}
	}
	rules := make([]FinalRule, 0, len(contents))
	for index, content := range contents {
		rule, parseErr := parseFinalRule(content)
		if parseErr != nil {
			return nil, parseErr
		}
		rule.Order = index + 1
		rule.Origin = userOrigin
		if index < runtimeRuleCount {
			rule.Origin = CandidateOriginRuntime
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func configurationRuleContents(body []byte, label string) ([]string, error) {
	root, err := parseConfigurationLayer(body, label)
	if err != nil {
		return nil, err
	}
	rules := mappingValue(root, "rules")
	if rules == nil {
		return []string{}, nil
	}
	if rules.Kind != yaml.SequenceNode {
		return nil, errors.New("Mihomo rules must be a sequence")
	}
	contents := make([]string, 0, len(rules.Content))
	for _, node := range rules.Content {
		if node.Kind != yaml.ScalarNode || strings.TrimSpace(node.Value) == "" {
			return nil, errors.New("Mihomo rule must be a non-empty scalar")
		}
		contents = append(contents, strings.TrimSpace(node.Value))
	}
	return contents, nil
}

func parseFinalRule(content string) (FinalRule, error) {
	separators := topLevelRuleSeparators(content)
	if len(separators) == 0 {
		return FinalRule{}, errors.New("Mihomo rule must contain a target")
	}
	ruleType := strings.ToUpper(strings.TrimSpace(content[:separators[0]]))
	if ruleType == "" {
		return FinalRule{}, errors.New("Mihomo rule type is empty")
	}
	if ruleType == "MATCH" {
		target := strings.TrimSpace(content[separators[0]+1:])
		if target == "" || strings.Contains(target, ",") {
			return FinalRule{}, errors.New("Mihomo MATCH rule target is invalid")
		}
		return FinalRule{
			Type:      ruleType,
			Condition: "全部流量",
			Target:    target,
			Content:   strings.TrimSpace(content),
		}, nil
	}
	if len(separators) < 2 {
		return FinalRule{}, errors.New("Mihomo rule condition or target is missing")
	}
	targetSeparator := separators[len(separators)-1]
	targetEnd := len(content)
	lastValue := strings.TrimSpace(content[targetSeparator+1:])
	if strings.EqualFold(lastValue, "no-resolve") {
		if len(separators) < 2 {
			return FinalRule{}, errors.New("Mihomo rule target is missing")
		}
		targetEnd = targetSeparator
		targetSeparator = separators[len(separators)-2]
	}
	target := strings.TrimSpace(content[targetSeparator+1 : targetEnd])
	if target == "" {
		return FinalRule{}, errors.New("Mihomo rule target is empty")
	}
	condition := strings.TrimSpace(content[separators[0]+1 : targetSeparator])
	if condition == "" {
		return FinalRule{}, errors.New("Mihomo rule condition is empty")
	}
	return FinalRule{
		Type:      ruleType,
		Condition: condition,
		Target:    target,
		Content:   strings.TrimSpace(content),
	}, nil
}

func topLevelRuleSeparators(content string) []int {
	separators := make([]int, 0, 4)
	parentheses := 0
	brackets := 0
	braces := 0
	escaped := false
	for index, character := range content {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		switch character {
		case '(':
			parentheses++
		case ')':
			if parentheses > 0 {
				parentheses--
			}
		case '[':
			brackets++
		case ']':
			if brackets > 0 {
				brackets--
			}
		case '{':
			braces++
		case '}':
			if braces > 0 {
				braces--
			}
		case ',':
			if parentheses == 0 && brackets == 0 && braces == 0 {
				separators = append(separators, index)
			}
		}
	}
	return separators
}
