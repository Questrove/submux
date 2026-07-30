package releasepolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const ArtifactPolicySchema = 1

type ArtifactPolicy struct {
	Schema                 int              `json:"schema"`
	ForbiddenTokens        []string         `json:"forbidden_tokens"`
	SecretPrefixes         []string         `json:"secret_prefixes"`
	BudgetRequiredPatterns []string         `json:"budget_required_patterns"`
	Budgets                []ArtifactBudget `json:"budgets"`
}

type ArtifactBudget struct {
	Pattern          string `json:"pattern"`
	MaxBytes         int64  `json:"max_bytes"`
	ApprovedMaxBytes int64  `json:"approved_max_bytes,omitempty"`
	Approval         string `json:"approval,omitempty"`
}

type ArtifactAudit struct {
	Files              int
	Bytes              int64
	Artifacts          []ArtifactSize
	ApprovedExceptions []string
}

type ArtifactSize struct {
	Path  string
	Bytes int64
}

func LoadArtifactPolicy(name string) (ArtifactPolicy, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return ArtifactPolicy{}, err
	}
	var policy ArtifactPolicy
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return ArtifactPolicy{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ArtifactPolicy{}, err
	}
	if policy.Schema != ArtifactPolicySchema || len(policy.ForbiddenTokens) == 0 ||
		len(policy.BudgetRequiredPatterns) == 0 || len(policy.Budgets) == 0 {
		return ArtifactPolicy{}, errors.New("artifact policy schema or rules are invalid")
	}
	for _, pattern := range policy.BudgetRequiredPatterns {
		if pattern == "" {
			return ArtifactPolicy{}, errors.New("artifact budget-required pattern is invalid")
		}
		if _, err := filepath.Match(pattern, "probe"); err != nil {
			return ArtifactPolicy{}, fmt.Errorf("artifact budget-required pattern %q is invalid: %w", pattern, err)
		}
	}
	for _, budget := range policy.Budgets {
		if budget.Pattern == "" || budget.MaxBytes <= 0 {
			return ArtifactPolicy{}, errors.New("artifact budget is invalid")
		}
		if budget.ApprovedMaxBytes != 0 &&
			(budget.ApprovedMaxBytes <= budget.MaxBytes || strings.TrimSpace(budget.Approval) == "") {
			return ArtifactPolicy{}, errors.New("artifact budget exception lacks a bounded approval")
		}
	}
	return policy, nil
}

func AuditArtifacts(root string, policy ArtifactPolicy) (ArtifactAudit, error) {
	if root == "" || !filepath.IsAbs(root) {
		return ArtifactAudit{}, errors.New("artifact root must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ArtifactAudit{}, errors.New("artifact root must be a real directory")
	}
	var result ArtifactAudit
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact tree contains a symbolic or reparse link: %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("artifact tree contains a non-regular file: %s", name)
		}
		result.Files++
		result.Bytes += info.Size()
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		result.Artifacts = append(result.Artifacts, ArtifactSize{
			Path:  filepath.ToSlash(relative),
			Bytes: info.Size(),
		})
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		lowerBody := bytes.ToLower(body)
		lowerName := strings.ToLower(filepath.ToSlash(name))
		for _, token := range policy.ForbiddenTokens {
			if strings.Contains(lowerName, strings.ToLower(token)) ||
				bytes.Contains(lowerBody, bytes.ToLower([]byte(token))) {
				return fmt.Errorf("artifact %s contains forbidden release token %q", name, token)
			}
		}
		for _, prefix := range policy.SecretPrefixes {
			lowerPrefix := bytes.ToLower([]byte(prefix))
			if containsCredentialPrefix([]byte(lowerName), lowerPrefix) ||
				containsCredentialPrefix(lowerBody, lowerPrefix) {
				return fmt.Errorf("artifact %s contains forbidden secret prefix %q", name, prefix)
			}
		}
		budgetMatched := false
		for _, budget := range policy.Budgets {
			matched, err := filepath.Match(budget.Pattern, filepath.Base(name))
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
			budgetMatched = true
			if info.Size() <= budget.MaxBytes {
				continue
			}
			if budget.ApprovedMaxBytes == 0 || info.Size() > budget.ApprovedMaxBytes {
				return fmt.Errorf(
					"artifact %s is %d bytes, exceeding budget %d",
					name,
					info.Size(),
					budget.MaxBytes,
				)
			}
			result.ApprovedExceptions = append(
				result.ApprovedExceptions,
				fmt.Sprintf("%s: %d > %d (%s)", filepath.Base(name), info.Size(), budget.MaxBytes, budget.Approval),
			)
		}
		budgetRequired := false
		for _, pattern := range policy.BudgetRequiredPatterns {
			matched, err := filepath.Match(pattern, filepath.Base(name))
			if err != nil {
				return err
			}
			if matched {
				budgetRequired = true
				break
			}
		}
		if budgetRequired && !budgetMatched {
			return fmt.Errorf("artifact %s requires a declared size budget", name)
		}
		return nil
	})
	if err != nil {
		return ArtifactAudit{}, err
	}
	if result.Files == 0 {
		return ArtifactAudit{}, errors.New("artifact tree is empty")
	}
	return result, nil
}

func containsCredentialPrefix(body, prefix []byte) bool {
	const minimumSuffix = 8
	for offset := 0; offset < len(body); {
		index := bytes.Index(body[offset:], prefix)
		if index < 0 {
			return false
		}
		suffix := offset + index + len(prefix)
		length := 0
		for suffix+length < len(body) && credentialByte(body[suffix+length]) {
			length++
		}
		if length >= minimumSuffix {
			return true
		}
		offset = suffix
	}
	return false
}

func credentialByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= '0' && value <= '9' ||
		value == '_' || value == '-'
}
