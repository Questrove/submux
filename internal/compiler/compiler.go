package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"submux/internal/node"
	"submux/internal/store"
)

const (
	EngineMihomo  = "mihomo"
	EngineSingBox = "sing-box"
)

type Service struct{ store *store.Store }

type Result struct {
	Body        []byte
	ContentType string
	Revision    string
	NodeCount   int
	SlotCounts  map[string]int
}

type resolvedProfile struct {
	Profile  store.Profile
	Template store.TemplateVersion
	Records  []store.NodeRecord
	Names    map[string]string
	Slots    map[string][]string
	Counts   map[string]int
}

func New(st *store.Store) *Service { return &Service{store: st} }

func ValidateTemplate(engine, content string, slots []store.TemplateSlot) error {
	if engine != EngineMihomo && engine != EngineSingBox {
		return fmt.Errorf("unsupported template engine %q", engine)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("template content is empty")
	}
	if err := validateSlots(slots); err != nil {
		return err
	}
	if engine == EngineMihomo {
		return validateMihomoTemplate(content, slots)
	}
	return validateSingBoxTemplate(content, slots)
}

func (s *Service) Preview(profile store.Profile) (Result, error) {
	resolved, err := s.resolve(profile)
	if err != nil {
		return Result{}, err
	}
	return compileResolved(resolved)
}

func (s *Service) CompileAndStore(profileID int64) (Result, error) {
	profile, err := s.store.GetProfile(profileID)
	if err != nil {
		return Result{}, err
	}
	result, compileErr := s.Preview(profile)
	if compileErr != nil {
		artifact, _ := s.store.GetProfileArtifact(profileID)
		artifact.ProfileID = profileID
		artifact.LastError = compileErr.Error()
		_ = s.store.PutProfileArtifact(artifact)
		return Result{}, compileErr
	}
	artifact := store.ProfileArtifact{
		ProfileID: profileID, Body: result.Body, ContentType: result.ContentType,
		Revision: result.Revision, LastSuccess: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.store.PutProfileArtifact(artifact); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Service) RebuildAll() error {
	profiles, err := s.store.ListProfiles()
	if err != nil {
		return err
	}
	var failures []string
	for _, profile := range profiles {
		if !profile.Enabled {
			continue
		}
		if _, err := s.CompileAndStore(profile.ID); err != nil {
			failures = append(failures, fmt.Sprintf("%d:%v", profile.ID, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("profile rebuild failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (s *Service) resolve(profile store.Profile) (resolvedProfile, error) {
	version, err := s.store.GetTemplateVersion(profile.TemplateVersionID)
	if err != nil {
		return resolvedProfile{}, err
	}
	template, err := s.store.GetTemplate(version.TemplateID)
	if err != nil {
		return resolvedProfile{}, err
	}
	if profile.Engine == "" {
		profile.Engine = template.Engine
	}
	if profile.Engine != template.Engine {
		return resolvedProfile{}, fmt.Errorf("profile engine %q does not match template engine %q", profile.Engine, template.Engine)
	}
	if err := ValidateTemplate(profile.Engine, version.Content, version.Slots); err != nil {
		return resolvedProfile{}, err
	}
	bindingBySlot := map[string]int64{}
	for _, binding := range profile.Bindings {
		if _, exists := bindingBySlot[binding.Slot]; exists {
			return resolvedProfile{}, fmt.Errorf("slot %q is bound more than once", binding.Slot)
		}
		bindingBySlot[binding.Slot] = binding.NodeSetID
	}
	slotDefs := map[string]store.TemplateSlot{}
	for _, slot := range version.Slots {
		slotDefs[slot.Key] = slot
	}
	for key := range bindingBySlot {
		if _, ok := slotDefs[key]; !ok {
			return resolvedProfile{}, fmt.Errorf("profile binds unknown slot %q", key)
		}
	}

	slotRecords := map[string][]store.NodeRecord{}
	counts := map[string]int{}
	for _, slot := range version.Slots {
		nodeSetID := bindingBySlot[slot.Key]
		if nodeSetID == 0 {
			if slot.Required {
				return resolvedProfile{}, fmt.Errorf("required slot %q is not bound", slot.Key)
			}
			continue
		}
		nodeSet, err := s.store.GetNodeSet(nodeSetID)
		if err != nil {
			return resolvedProfile{}, fmt.Errorf("slot %q: %w", slot.Key, err)
		}
		records, err := s.resolveNodeSet(nodeSet)
		if err != nil {
			return resolvedProfile{}, fmt.Errorf("slot %q: %w", slot.Key, err)
		}
		if slot.Required && len(records) == 0 {
			return resolvedProfile{}, fmt.Errorf("required slot %q resolved to no nodes", slot.Key)
		}
		slotRecords[slot.Key], counts[slot.Key] = records, len(records)
	}

	allByFingerprint := map[string]store.NodeRecord{}
	for _, slot := range version.Slots {
		for _, record := range slotRecords[slot.Key] {
			if _, exists := allByFingerprint[record.Fingerprint]; !exists {
				allByFingerprint[record.Fingerprint] = record
			}
		}
	}
	records := make([]store.NodeRecord, 0, len(allByFingerprint))
	for _, record := range allByFingerprint {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].SourceID != records[j].SourceID {
			return records[i].SourceID < records[j].SourceID
		}
		return records[i].ID < records[j].ID
	})

	sources, err := s.store.ListSources()
	if err != nil {
		return resolvedProfile{}, err
	}
	sourceNames := map[int64]string{}
	for _, source := range sources {
		sourceNames[source.ID] = source.Name
	}
	names := uniqueNames(records, sourceNames)
	slots := map[string][]string{}
	for key, values := range slotRecords {
		seen := map[string]bool{}
		for _, record := range values {
			name := names[record.Fingerprint]
			if !seen[name] {
				seen[name] = true
				slots[key] = append(slots[key], name)
			}
		}
	}
	return resolvedProfile{Profile: profile, Template: version, Records: records, Names: names, Slots: slots, Counts: counts}, nil
}

func (s *Service) resolveNodeSet(value store.NodeSet) ([]store.NodeRecord, error) {
	if !value.Enabled {
		return nil, fmt.Errorf("node set %q is disabled", value.Name)
	}
	nodes, err := s.store.ListNodes()
	if err != nil {
		return nil, err
	}
	sources, err := s.store.ListSources()
	if err != nil {
		return nil, err
	}
	enabledSources := map[int64]bool{}
	for _, source := range sources {
		enabledSources[source.ID] = source.Enabled
	}
	includeSources, includeNodes, excludeNodes := intSet(value.SourceIDs), intSet(value.NodeIDs), intSet(value.ExcludeNodeIDs)
	protocols, requiredTags := stringSetLower(value.Protocols), stringSetLower(value.Tags)
	selectAll := len(includeSources) == 0 && len(includeNodes) == 0
	needle := strings.ToLower(strings.TrimSpace(value.NameContains))
	var out []store.NodeRecord
	for _, record := range nodes {
		if !record.Enabled || !enabledSources[record.SourceID] || excludeNodes[record.ID] {
			continue
		}
		if !selectAll && !includeSources[record.SourceID] && !includeNodes[record.ID] {
			continue
		}
		if len(protocols) > 0 && !protocols[strings.ToLower(record.Protocol)] {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(node.DisplayName(record)), needle) {
			continue
		}
		if !containsAllTags(record.Tags, requiredTags) {
			continue
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func compileResolved(value resolvedProfile) (Result, error) {
	var body []byte
	var contentType string
	var err error
	if value.Profile.Engine == EngineMihomo {
		body, err = compileMihomo(value)
		contentType = "text/yaml; charset=utf-8"
	} else if value.Profile.Engine == EngineSingBox {
		body, err = compileSingBox(value)
		contentType = "application/json; charset=utf-8"
	} else {
		err = fmt.Errorf("unsupported profile engine %q", value.Profile.Engine)
	}
	if err != nil {
		return Result{}, err
	}
	// The revision identifies the actual compiled artifact, including slot
	// placement and deterministic generated names, not just the node set.
	sum := sha256.Sum256(body)
	return Result{Body: body, ContentType: contentType, Revision: hex.EncodeToString(sum[:]), NodeCount: len(value.Records), SlotCounts: value.Counts}, nil
}

func validateSlots(slots []store.TemplateSlot) error {
	keys, targets := map[string]bool{}, map[string]bool{}
	for _, slot := range slots {
		slot.Key, slot.Target, slot.Mode = strings.TrimSpace(slot.Key), strings.TrimSpace(slot.Target), strings.TrimSpace(slot.Mode)
		if slot.Key == "" || slot.Target == "" {
			return fmt.Errorf("template slot key and target are required")
		}
		if keys[slot.Key] {
			return fmt.Errorf("duplicate template slot key %q", slot.Key)
		}
		if targets[slot.Target] {
			return fmt.Errorf("template target %q is used by multiple slots", slot.Target)
		}
		if slot.Mode != "" && slot.Mode != "append" && slot.Mode != "replace" {
			return fmt.Errorf("slot %q has unsupported mode %q", slot.Key, slot.Mode)
		}
		keys[slot.Key], targets[slot.Target] = true, true
	}
	return nil
}

func uniqueNames(records []store.NodeRecord, sourceNames map[int64]string) map[string]string {
	out, used := map[string]string{}, map[string]bool{}
	for _, record := range records {
		base := strings.TrimSpace(node.DisplayName(record))
		if record.Alias == "" {
			if source := strings.TrimSpace(sourceNames[record.SourceID]); source != "" {
				base = "[" + source + "] " + base
			}
		}
		if base == "" {
			base = record.Protocol
		}
		name := base
		if used[name] {
			name = fmt.Sprintf("%s #%d", base, record.ID)
		}
		for used[name] {
			name += "_"
		}
		used[name], out[record.Fingerprint] = true, name
	}
	return out
}

func intSet(values []int64) map[int64]bool {
	out := map[int64]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}
func stringSetLower(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out[value] = true
		}
	}
	return out
}
func containsAllTags(values []string, required map[string]bool) bool {
	if len(required) == 0 {
		return true
	}
	have := stringSetLower(values)
	for value := range required {
		if !have[value] {
			return false
		}
	}
	return true
}

func revisionJSON(value any) string { b, _ := json.Marshal(value); return string(b) }
