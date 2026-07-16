package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"submux/internal/lifecycle"
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
	Warnings    []string
}

type resolvedSubscription struct {
	Subscription store.OutputSubscription
	Template     store.TemplateVersion
	Records      []store.NodeRecord
	Names        map[string]string
	Slots        map[string][]string
	Counts       map[string]int
	Warnings     []string
}

type BlockedError struct {
	Reason   string
	Warnings []string
}

func (e *BlockedError) Error() string { return e.Reason }

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

func (s *Service) Preview(subscription store.OutputSubscription) (Result, error) {
	resolved, err := s.resolve(subscription)
	if err != nil {
		return Result{}, err
	}
	return compileResolved(resolved)
}

func (s *Service) CompileAndStore(subscriptionID int64) (Result, error) {
	subscription, err := s.store.GetOutputSubscription(subscriptionID)
	if err != nil {
		return Result{}, err
	}
	result, compileErr := s.Preview(subscription)
	if compileErr != nil {
		artifact, _ := s.store.GetSubscriptionArtifact(subscriptionID)
		artifact.SubscriptionID = subscriptionID
		artifact.LastError = compileErr.Error()
		var blocked *BlockedError
		if errors.As(compileErr, &blocked) {
			artifact.BlockedReason = blocked.Reason
			artifact.Warnings = append([]string(nil), blocked.Warnings...)
		}
		_ = s.store.PutSubscriptionArtifact(artifact)
		return Result{}, compileErr
	}
	artifact := store.SubscriptionArtifact{
		SubscriptionID: subscriptionID, Body: result.Body, ContentType: result.ContentType,
		Revision: result.Revision, LastSuccess: time.Now().UTC().Format(time.RFC3339),
		Warnings: append([]string(nil), result.Warnings...),
	}
	if err := s.store.PutSubscriptionArtifact(artifact); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Service) RebuildAll() error {
	subscriptions, err := s.store.ListOutputSubscriptions()
	if err != nil {
		return err
	}
	var failures []string
	for _, subscription := range subscriptions {
		if !subscription.Enabled {
			continue
		}
		if _, err := s.CompileAndStore(subscription.ID); err != nil {
			failures = append(failures, fmt.Sprintf("%d:%v", subscription.ID, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("output subscription rebuild failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (s *Service) resolve(subscription store.OutputSubscription) (resolvedSubscription, error) {
	version, err := s.store.GetTemplateVersion(subscription.TemplateVersionID)
	if err != nil {
		return resolvedSubscription{}, err
	}
	template, err := s.store.GetTemplate(version.TemplateID)
	if err != nil {
		return resolvedSubscription{}, err
	}
	if subscription.Engine == "" {
		subscription.Engine = template.Engine
	}
	if subscription.Engine != template.Engine {
		return resolvedSubscription{}, fmt.Errorf("output subscription engine %q does not match template engine %q", subscription.Engine, template.Engine)
	}
	if err := ValidateTemplate(subscription.Engine, version.Content, version.Slots); err != nil {
		return resolvedSubscription{}, err
	}
	bindingBySlot := map[string][]int64{}
	for _, binding := range subscription.Bindings {
		if _, exists := bindingBySlot[binding.Slot]; exists {
			return resolvedSubscription{}, fmt.Errorf("slot %q is bound more than once", binding.Slot)
		}
		bindingBySlot[binding.Slot] = append([]int64(nil), binding.NodeIDs...)
	}
	slotDefs := map[string]store.TemplateSlot{}
	for _, slot := range version.Slots {
		slotDefs[slot.Key] = slot
	}
	for key := range bindingBySlot {
		if _, ok := slotDefs[key]; !ok {
			return resolvedSubscription{}, fmt.Errorf("output subscription binds unknown slot %q", key)
		}
	}

	slotRecords := map[string][]store.NodeRecord{}
	counts := map[string]int{}
	warningSet := map[string]bool{}
	for _, slot := range version.Slots {
		nodeIDs, bound := bindingBySlot[slot.Key]
		if !bound || len(nodeIDs) == 0 {
			if slot.Required {
				return resolvedSubscription{}, fmt.Errorf("required slot %q has no selected nodes", slot.Key)
			}
			continue
		}
		resolution, err := s.resolveNodeSelection(nodeIDs)
		if err != nil {
			return resolvedSubscription{}, fmt.Errorf("slot %q: %w", slot.Key, err)
		}
		for _, warning := range resolution.warnings {
			warningSet[warning] = true
		}
		if slot.Required && len(resolution.records) == 0 {
			if resolution.strictExcluded > 0 {
				warnings := sortedKeys(warningSet)
				return resolvedSubscription{}, &BlockedError{Reason: fmt.Sprintf("required slot %q has no nodes after strict lifecycle filtering", slot.Key), Warnings: warnings}
			}
			return resolvedSubscription{}, fmt.Errorf("required slot %q has no available selected nodes", slot.Key)
		}
		slotRecords[slot.Key], counts[slot.Key] = resolution.records, len(resolution.records)
	}

	seenFingerprints := map[string]bool{}
	records := make([]store.NodeRecord, 0)
	for _, slot := range version.Slots {
		for _, record := range slotRecords[slot.Key] {
			if !seenFingerprints[record.Fingerprint] {
				seenFingerprints[record.Fingerprint] = true
				records = append(records, record)
			}
		}
	}

	sources, err := s.store.ListSources()
	if err != nil {
		return resolvedSubscription{}, err
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
	return resolvedSubscription{Subscription: subscription, Template: version, Records: records, Names: names, Slots: slots, Counts: counts, Warnings: sortedKeys(warningSet)}, nil
}

type nodeSelectionResolution struct {
	records        []store.NodeRecord
	warnings       []string
	strictExcluded int
}

func (s *Service) resolveNodeSelection(nodeIDs []int64) (nodeSelectionResolution, error) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return nodeSelectionResolution{}, err
	}
	sources, err := s.store.ListSources()
	if err != nil {
		return nodeSelectionResolution{}, err
	}
	enabledSources := map[int64]bool{}
	sourceByID := map[int64]store.Source{}
	statusBySource := map[int64]lifecycle.Status{}
	for _, source := range sources {
		enabledSources[source.ID] = source.Enabled
		sourceByID[source.ID] = source
		if source.Kind == store.SourceKindSubscription {
			cache, _ := s.store.GetCache(source.ID)
			statusBySource[source.ID] = lifecycle.Evaluate(source, cache, time.Now())
		}
	}
	nodeByID := make(map[int64]store.NodeRecord, len(nodes))
	for _, record := range nodes {
		nodeByID[record.ID] = record
	}
	var out []store.NodeRecord
	warningSet := map[string]bool{}
	strictExcluded := 0
	for _, nodeID := range nodeIDs {
		record, exists := nodeByID[nodeID]
		if !exists {
			warningSet[fmt.Sprintf("selected node %d no longer exists", nodeID)] = true
			continue
		}
		if record.Role == "notice" {
			warningSet[fmt.Sprintf("selected node %d is an informational notice", nodeID)] = true
			continue
		}
		if !record.Enabled {
			warningSet[fmt.Sprintf("selected node %q is disabled", node.DisplayName(record))] = true
			continue
		}
		source, sourceExists := sourceByID[record.SourceID]
		if !sourceExists {
			warningSet[fmt.Sprintf("selected node %d has no source", nodeID)] = true
			continue
		}
		if !enabledSources[record.SourceID] {
			warningSet[fmt.Sprintf("source %q is disabled", source.Name)] = true
			continue
		}
		if status, exists := statusBySource[record.SourceID]; exists {
			for _, warning := range status.Warnings {
				warningSet[fmt.Sprintf("source %q: %s", source.Name, warning)] = true
			}
			if lifecycle.ShouldExclude(sourceByID[record.SourceID], status) {
				strictExcluded++
				continue
			}
		}
		out = append(out, record)
	}
	return nodeSelectionResolution{records: out, warnings: sortedKeys(warningSet), strictExcluded: strictExcluded}, nil
}

func compileResolved(value resolvedSubscription) (Result, error) {
	var body []byte
	var contentType string
	var err error
	if value.Subscription.Engine == EngineMihomo {
		body, err = compileMihomo(value)
		contentType = "text/yaml; charset=utf-8"
	} else if value.Subscription.Engine == EngineSingBox {
		body, err = compileSingBox(value)
		contentType = "application/json; charset=utf-8"
	} else {
		err = fmt.Errorf("unsupported output subscription engine %q", value.Subscription.Engine)
	}
	if err != nil {
		return Result{}, err
	}
	// The revision identifies the actual compiled artifact, including slot
	// placement, selected order and deterministic generated names.
	sum := sha256.Sum256(body)
	return Result{Body: body, ContentType: contentType, Revision: hex.EncodeToString(sum[:]), NodeCount: len(value.Records), SlotCounts: value.Counts, Warnings: append([]string(nil), value.Warnings...)}, nil
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
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
		if source := strings.TrimSpace(sourceNames[record.SourceID]); source != "" {
			base = "[" + source + "] " + base
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

func revisionJSON(value any) string { b, _ := json.Marshal(value); return string(b) }
