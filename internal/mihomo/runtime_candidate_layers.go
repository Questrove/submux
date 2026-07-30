package mihomo

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

const (
	ManagedResourceKindProxyProvider = "proxy-provider-yaml"
	ManagedResourceKindRuleProvider  = "rule-provider-yaml"
	ManagedResourceKindCertificate   = "certificate-pem"
	ManagedResourceKindPrivateKey    = "private-key-pem"

	CandidateOriginSource   = "source"
	CandidateOriginOverride = "advanced_override"
	CandidateOriginRuntime  = "runtime"

	CandidateStatusKept       = "kept"
	CandidateStatusAdded      = "added"
	CandidateStatusOverridden = "overridden"
	CandidateStatusReplaced   = "replaced"
	CandidateStatusRemoved    = "removed"
)

type ManagedResource struct {
	ID   string
	Kind string
	Path string
}

type CandidateFieldOrigin struct {
	Path           string
	Origin         string
	Status         string
	ReplacedOrigin string
}

type DetailedCandidate struct {
	YAML                []byte
	FieldOrigins        []CandidateFieldOrigin
	ReferencedResources []string
}

func (b ExplicitCandidateBuilder) BuildDetailed(
	source []byte,
	advancedOverride []byte,
	resources []ManagedResource,
) (DetailedCandidate, error) {
	port, platform, err := b.runtimeSettings()
	if err != nil {
		return DetailedCandidate{}, err
	}
	sourceRoot, err := parseConfigurationLayer(source, "source")
	if err != nil {
		return DetailedCandidate{}, err
	}
	overrideRoot, err := parseConfigurationLayer(advancedOverride, "advanced override")
	if err != nil {
		return DetailedCandidate{}, err
	}
	if err := rejectRuntimeOwnedOverride(overrideRoot); err != nil {
		return DetailedCandidate{}, err
	}
	resourceMap, err := indexManagedResources(resources)
	if err != nil {
		return DetailedCandidate{}, err
	}

	tracker := newCandidateOriginTracker(sourceRoot)
	mergeConfigurationMapping(sourceRoot, overrideRoot, "", tracker)
	removeRuntimeOwnedFields(sourceRoot, tracker)
	if err := rewriteManagedReferences(sourceRoot, resourceMap, tracker); err != nil {
		return DetailedCandidate{}, err
	}
	if err := rejectUnknownSensitiveFields(sourceRoot, ""); err != nil {
		return DetailedCandidate{}, err
	}
	if err := prependRuntimeHealthRules(sourceRoot); err != nil {
		return DetailedCandidate{}, err
	}
	tracker.setRuntimeField(sourceRoot, "rules", mappingValue(sourceRoot, "rules"))

	tracker.setRuntimeScalar(sourceRoot, "mixed-port", "0", "!!int")
	tracker.setRuntimeScalar(sourceRoot, "port", "0", "!!int")
	tracker.setRuntimeScalar(sourceRoot, "socks-port", "0", "!!int")
	tracker.setRuntimeScalar(sourceRoot, "redir-port", "0", "!!int")
	tracker.setRuntimeScalar(sourceRoot, "tproxy-port", "0", "!!int")
	tracker.setRuntimeScalar(sourceRoot, "allow-lan", "false", "!!bool")
	tracker.setRuntimeScalar(sourceRoot, "bind-address", "127.0.0.1", "!!str")
	ipv6Enabled := true
	if b.TUN != nil && b.TUN.IPv6Policy == "block" {
		ipv6Enabled = false
	}
	tracker.setRuntimeScalar(sourceRoot, "ipv6", fmt.Sprintf("%t", ipv6Enabled), "!!bool")
	tracker.setRuntimeScalar(sourceRoot, "secret", "", "!!str")
	tracker.setRuntimeField(sourceRoot, "authentication", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
	tun := map[string]*yaml.Node{
		"enable": scalarNode("false", "!!bool"),
	}
	if b.TUN != nil {
		tun = map[string]*yaml.Node{
			"enable":                scalarNode("true", "!!bool"),
			"device":                scalarNode(b.TUN.Device, "!!str"),
			"stack":                 scalarNode("system", "!!str"),
			"auto-route":            scalarNode("false", "!!bool"),
			"auto-redirect":         scalarNode("false", "!!bool"),
			"auto-detect-interface": scalarNode("true", "!!bool"),
			"strict-route":          scalarNode("true", "!!bool"),
		}
		if b.TUN.HijackDNS {
			tun["dns-hijack"] = &yaml.Node{
				Kind: yaml.SequenceNode,
				Tag:  "!!seq",
				Content: []*yaml.Node{
					scalarNode("any:53", "!!str"),
					scalarNode("tcp://any:53", "!!str"),
				},
			}
		}
		if platform == "linux" {
			tracker.setRuntimeScalar(
				sourceRoot,
				"routing-mark",
				fmt.Sprintf("%d", b.TUN.RoutingMark),
				"!!int",
			)
		}
	}
	tracker.setRuntimeField(sourceRoot, "tun", mappingNode(tun))
	if platform == "windows" {
		tracker.setRuntimeScalar(sourceRoot, "external-controller-pipe", b.ControlEndpoint, "!!str")
	} else {
		tracker.setRuntimeScalar(sourceRoot, "external-controller-unix", b.ControlEndpoint, "!!str")
	}
	tracker.setRuntimeField(sourceRoot, "listeners", explicitListeners(port))

	candidate, err := yaml.Marshal(sourceRoot)
	if err != nil {
		return DetailedCandidate{}, fmt.Errorf("encode Runtime-owned Mihomo configuration: %w", err)
	}
	tracker.ensureFinalFields(sourceRoot)
	return DetailedCandidate{
		YAML:                candidate,
		FieldOrigins:        tracker.sorted(),
		ReferencedResources: tracker.sortedReferences(),
	}, nil
}

func ValidateManagedResource(kind string, body []byte) error {
	switch kind {
	case ManagedResourceKindProxyProvider:
		return validateProviderResource(body, "proxies")
	case ManagedResourceKindRuleProvider:
		return validateProviderResource(body, "payload")
	case ManagedResourceKindCertificate:
		return validateCertificateResource(body)
	case ManagedResourceKindPrivateKey:
		return validatePrivateKeyResource(body)
	default:
		return errors.New("managed resource kind is unsupported")
	}
}

func parseConfigurationLayer(body []byte, label string) (*yaml.Node, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}\n")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("parse Mihomo %s: %w", label, err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Mihomo %s must be a YAML mapping", label)
	}
	if err := validateRuntimeYAML(document.Content[0], 0, new(int)); err != nil {
		return nil, err
	}
	return document.Content[0], nil
}

func rejectRuntimeOwnedOverride(root *yaml.Node) error {
	for _, path := range explicitRuntimeOwnedFields {
		parts := strings.Split(path, ".")
		current := root
		found := true
		for _, part := range parts {
			current = mappingValue(current, part)
			if current == nil {
				found = false
				break
			}
		}
		if found {
			return fmt.Errorf("advanced override cannot modify Runtime-owned field %q", path)
		}
	}
	return nil
}

func removeRuntimeOwnedFields(root *yaml.Node, tracker *candidateOriginTracker) {
	for _, path := range explicitRuntimeOwnedFields {
		parts := strings.Split(path, ".")
		if len(parts) == 1 {
			if mappingValue(root, parts[0]) != nil {
				tracker.removeField(root, parts[0], parts[0])
			}
			continue
		}
		parent := mappingValue(root, parts[0])
		if parent != nil && parent.Kind == yaml.MappingNode && mappingValue(parent, parts[1]) != nil {
			tracker.removeField(parent, parts[1], path)
		}
	}
}

func mergeConfigurationMapping(
	destination *yaml.Node,
	override *yaml.Node,
	prefix string,
	tracker *candidateOriginTracker,
) {
	for index := 0; index+1 < len(override.Content); index += 2 {
		key := override.Content[index].Value
		path := joinFieldPath(prefix, key)
		overrideValue := override.Content[index+1]
		existing := mappingValue(destination, key)
		if existing != nil &&
			existing.Kind == yaml.MappingNode &&
			overrideValue.Kind == yaml.MappingNode {
			mergeConfigurationMapping(existing, overrideValue, path, tracker)
			continue
		}
		replacement := cloneYAMLNode(overrideValue)
		tracker.replaceField(destination, key, path, replacement, CandidateOriginOverride)
	}
}

func indexManagedResources(resources []ManagedResource) (map[string]ManagedResource, error) {
	result := make(map[string]ManagedResource, len(resources))
	for _, resource := range resources {
		if !validManagedResourceID(resource.ID) ||
			!validManagedResourceKind(resource.Kind) ||
			!filepath.IsAbs(resource.Path) ||
			hasControlText(resource.Path) {
			return nil, errors.New("Runtime managed resource is invalid")
		}
		if _, exists := result[resource.ID]; exists {
			return nil, errors.New("Runtime managed resource ID is duplicated")
		}
		result[resource.ID] = resource
	}
	return result, nil
}

func rewriteManagedReferences(
	root *yaml.Node,
	resources map[string]ManagedResource,
	tracker *candidateOriginTracker,
) error {
	for _, section := range []struct {
		name string
		kind string
	}{
		{name: "proxy-providers", kind: ManagedResourceKindProxyProvider},
		{name: "rule-providers", kind: ManagedResourceKindRuleProvider},
	} {
		providers := mappingValue(root, section.name)
		if providers == nil {
			continue
		}
		if providers.Kind != yaml.MappingNode {
			return fmt.Errorf("%s must be a mapping", section.name)
		}
		for index := 0; index+1 < len(providers.Content); index += 2 {
			name := providers.Content[index].Value
			provider := providers.Content[index+1]
			if provider.Kind != yaml.MappingNode {
				return fmt.Errorf("%s.%s must be a mapping", section.name, name)
			}
			typeNode := mappingValue(provider, "type")
			pathNode := mappingValue(provider, "path")
			if typeNode == nil || typeNode.Kind != yaml.ScalarNode ||
				strings.ToLower(typeNode.Value) != "file" ||
				pathNode == nil || pathNode.Kind != yaml.ScalarNode ||
				mappingValue(provider, "url") != nil {
				return fmt.Errorf("%s.%s must use one Runtime-managed file resource", section.name, name)
			}
			fieldPath := section.name + "." + name + ".path"
			resource, err := resolveManagedResourceReference(pathNode.Value, section.kind, resources)
			if err != nil {
				return fmt.Errorf("%s: %w", fieldPath, err)
			}
			tracker.replaceScalarPath(fieldPath, pathNode, resource.Path)
			tracker.references[resource.ID] = struct{}{}
		}
	}
	return rewriteCertificateReferences(root, "", resources, tracker)
}

func rewriteCertificateReferences(
	node *yaml.Node,
	path string,
	resources map[string]ManagedResource,
	tracker *candidateOriginTracker,
) error {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index].Value
			value := node.Content[index+1]
			fieldPath := joinFieldPath(path, key)
			var expectedKind string
			switch strings.ToLower(key) {
			case "certificate", "ca-file":
				expectedKind = ManagedResourceKindCertificate
			case "private-key":
				expectedKind = ManagedResourceKindPrivateKey
			}
			if expectedKind != "" {
				ids, err := rewriteResourceValue(value, expectedKind, resources)
				if err != nil {
					return fmt.Errorf("%s: %w", fieldPath, err)
				}
				tracker.markRuntimeReplacement(fieldPath)
				for _, id := range ids {
					tracker.references[id] = struct{}{}
				}
				continue
			}
			if err := rewriteCertificateReferences(value, fieldPath, resources, tracker); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := rewriteCertificateReferences(child, indexedFieldPath(path, index), resources, tracker); err != nil {
				return err
			}
		}
	}
	return nil
}

func rewriteResourceValue(
	node *yaml.Node,
	expectedKind string,
	resources map[string]ManagedResource,
) ([]string, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		resource, err := resolveManagedResourceReference(node.Value, expectedKind, resources)
		if err != nil {
			return nil, err
		}
		node.Value = resource.Path
		node.Tag = "!!str"
		return []string{resource.ID}, nil
	case yaml.SequenceNode:
		var ids []string
		for _, child := range node.Content {
			childIDs, err := rewriteResourceValue(child, expectedKind, resources)
			if err != nil {
				return nil, err
			}
			ids = append(ids, childIDs...)
		}
		return ids, nil
	default:
		return nil, errors.New("managed resource reference must be a string or sequence of strings")
	}
}

func resolveManagedResourceReference(
	value string,
	expectedKind string,
	resources map[string]ManagedResource,
) (ManagedResource, error) {
	id, ok := strings.CutPrefix(strings.TrimSpace(value), "resource://")
	if !ok || !validManagedResourceID(id) {
		return ManagedResource{}, errors.New("file reference must use resource://<resource-id>")
	}
	resource, found := resources[id]
	if !found {
		return ManagedResource{}, errors.New("referenced Runtime managed resource does not exist")
	}
	if resource.Kind != expectedKind {
		return ManagedResource{}, errors.New("referenced Runtime managed resource has the wrong type")
	}
	return resource, nil
}

func rejectUnknownSensitiveFields(node *yaml.Node, path string) error {
	switch node.Kind {
	case yaml.MappingNode:
		typeNode := mappingValue(node, "type")
		if typeNode != nil && typeNode.Kind == yaml.ScalarNode &&
			strings.EqualFold(typeNode.Value, "file") &&
			!strings.HasPrefix(path, "proxy-providers.") &&
			!strings.HasPrefix(path, "rule-providers.") {
			return fmt.Errorf("%s uses an unmanaged file-backed field", path)
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index].Value
			value := node.Content[index+1]
			fieldPath := joinFieldPath(path, key)
			lower := strings.ToLower(key)
			switch {
			case lower == "certificate" || lower == "private-key" || lower == "ca-file":
			case lower == "listen":
				return fmt.Errorf("%s is an unauthorized listener field", fieldPath)
			case lower == "path" && !allowedPathField(fieldPath):
				return fmt.Errorf("%s is an unmanaged path field", fieldPath)
			case lower == "url" && !allowedURLField(fieldPath):
				return fmt.Errorf("%s is an unauthorized URL field", fieldPath)
			case suspiciousSensitiveKey(lower):
				return fmt.Errorf("%s is an unsupported path, write, or download field", fieldPath)
			}
			if err := rejectUnknownSensitiveFields(value, fieldPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := rejectUnknownSensitiveFields(child, indexedFieldPath(path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func allowedPathField(path string) bool {
	if strings.HasPrefix(path, "proxy-providers.") ||
		strings.HasPrefix(path, "rule-providers.") {
		return true
	}
	for _, segment := range []string{".ws-opts.path", ".http-opts.path", ".h2-opts.path"} {
		if strings.HasSuffix(path, segment) {
			return true
		}
	}
	return false
}

func allowedURLField(path string) bool {
	return strings.Contains(path, ".health-check.url") ||
		strings.HasPrefix(path, "proxy-groups[")
}

func suspiciousSensitiveKey(key string) bool {
	if key == "file" ||
		key == "filename" ||
		key == "directory" ||
		key == "dir" ||
		key == "output" ||
		key == "script" ||
		key == "write" {
		return true
	}
	return strings.HasSuffix(key, "-file") ||
		strings.HasSuffix(key, "-filename") ||
		strings.HasSuffix(key, "-directory") ||
		strings.HasSuffix(key, "-dir") ||
		strings.HasSuffix(key, "-path") ||
		strings.HasSuffix(key, "-url") ||
		strings.Contains(key, "download")
}

func validateProviderResource(body []byte, expectedField string) error {
	root, err := parseConfigurationLayer(body, "managed provider resource")
	if err != nil {
		return err
	}
	if len(root.Content) != 2 ||
		root.Content[0].Value != expectedField ||
		root.Content[1].Kind != yaml.SequenceNode {
		return fmt.Errorf("managed provider resource must contain only a %s sequence", expectedField)
	}
	if len(root.Content[1].Content) == 0 {
		return fmt.Errorf("managed provider resource %s sequence is empty", expectedField)
	}
	return nil
}

func validateCertificateResource(body []byte) error {
	remaining := body
	found := false
	for {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return errors.New("certificate resource contains a non-certificate PEM block")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errors.New("certificate resource contains an invalid certificate")
		}
		found = true
		remaining = rest
	}
	if !found || len(strings.TrimSpace(string(remaining))) != 0 {
		return errors.New("certificate resource must contain only valid PEM certificates")
	}
	return nil
}

func validatePrivateKeyResource(body []byte) error {
	block, rest := pem.Decode(body)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("private key resource must contain exactly one PEM private key")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return errors.New("private key resource PEM type is unsupported")
	}
	if err != nil {
		return errors.New("private key resource is invalid")
	}
	switch key.(type) {
	case *rsa.PrivateKey, *ecdsa.PrivateKey:
		return nil
	default:
		return errors.New("private key resource algorithm is unsupported")
	}
}

func validManagedResourceID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "res_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validManagedResourceKind(kind string) bool {
	switch kind {
	case ManagedResourceKindProxyProvider,
		ManagedResourceKindRuleProvider,
		ManagedResourceKindCertificate,
		ManagedResourceKindPrivateKey:
		return true
	default:
		return false
	}
}

func hasControlText(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

type candidateOriginTracker struct {
	fields     map[string]CandidateFieldOrigin
	references map[string]struct{}
}

func newCandidateOriginTracker(root *yaml.Node) *candidateOriginTracker {
	tracker := &candidateOriginTracker{
		fields:     make(map[string]CandidateFieldOrigin),
		references: make(map[string]struct{}),
	}
	tracker.markSubtree(root, "", CandidateOriginSource, CandidateStatusKept, "")
	return tracker
}

func (t *candidateOriginTracker) replaceField(
	mapping *yaml.Node,
	key string,
	path string,
	value *yaml.Node,
	origin string,
) {
	replaced := t.originAt(path)
	status := CandidateStatusAdded
	if mappingValue(mapping, key) != nil {
		status = CandidateStatusOverridden
	}
	t.removeSubtree(path)
	setMappingNode(mapping, key, value)
	t.markFieldAndChildren(path, value, origin, status, replaced)
}

func (t *candidateOriginTracker) removeField(mapping *yaml.Node, key, path string) {
	replaced := t.originAt(path)
	removeMappingKey(mapping, key)
	t.removeSubtree(path)
	t.fields[path] = CandidateFieldOrigin{
		Path:           path,
		Origin:         CandidateOriginRuntime,
		Status:         CandidateStatusRemoved,
		ReplacedOrigin: replaced,
	}
}

func (t *candidateOriginTracker) setRuntimeScalar(
	mapping *yaml.Node,
	key string,
	value string,
	tag string,
) {
	t.setRuntimeField(mapping, key, scalarNode(value, tag))
}

func (t *candidateOriginTracker) setRuntimeField(mapping *yaml.Node, key string, value *yaml.Node) {
	replaced := t.originAt(key)
	status := CandidateStatusAdded
	if mappingValue(mapping, key) != nil {
		status = CandidateStatusReplaced
	} else if removed, found := t.fields[key]; found && removed.Status == CandidateStatusRemoved {
		status = CandidateStatusReplaced
		replaced = removed.ReplacedOrigin
	}
	t.removeSubtree(key)
	setMappingNode(mapping, key, value)
	t.markFieldAndChildren(key, value, CandidateOriginRuntime, status, replaced)
}

func (t *candidateOriginTracker) replaceScalarPath(path string, node *yaml.Node, value string) {
	node.Value = value
	node.Tag = "!!str"
	t.markRuntimeReplacement(path)
}

func (t *candidateOriginTracker) markRuntimeReplacement(path string) {
	replaced := t.originAt(path)
	t.fields[path] = CandidateFieldOrigin{
		Path:           path,
		Origin:         CandidateOriginRuntime,
		Status:         CandidateStatusReplaced,
		ReplacedOrigin: replaced,
	}
}

func (t *candidateOriginTracker) markFieldAndChildren(
	path string,
	value *yaml.Node,
	origin string,
	status string,
	replaced string,
) {
	t.fields[path] = CandidateFieldOrigin{
		Path:           path,
		Origin:         origin,
		Status:         status,
		ReplacedOrigin: replaced,
	}
	t.markSubtree(value, path, origin, CandidateStatusAdded, "")
}

func (t *candidateOriginTracker) markSubtree(
	node *yaml.Node,
	path string,
	origin string,
	status string,
	replaced string,
) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			fieldPath := joinFieldPath(path, node.Content[index].Value)
			t.fields[fieldPath] = CandidateFieldOrigin{
				Path:           fieldPath,
				Origin:         origin,
				Status:         status,
				ReplacedOrigin: replaced,
			}
			t.markSubtree(node.Content[index+1], fieldPath, origin, status, replaced)
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			t.markSubtree(child, indexedFieldPath(path, index), origin, status, replaced)
		}
	}
}

func (t *candidateOriginTracker) ensureFinalFields(root *yaml.Node) {
	var walk func(*yaml.Node, string)
	walk = func(node *yaml.Node, path string) {
		switch node.Kind {
		case yaml.MappingNode:
			for index := 0; index+1 < len(node.Content); index += 2 {
				fieldPath := joinFieldPath(path, node.Content[index].Value)
				if _, found := t.fields[fieldPath]; !found {
					t.fields[fieldPath] = CandidateFieldOrigin{
						Path:   fieldPath,
						Origin: CandidateOriginSource,
						Status: CandidateStatusKept,
					}
				}
				walk(node.Content[index+1], fieldPath)
			}
		case yaml.SequenceNode:
			for index, child := range node.Content {
				walk(child, indexedFieldPath(path, index))
			}
		}
	}
	walk(root, "")
}

func (t *candidateOriginTracker) originAt(path string) string {
	if field, found := t.fields[path]; found {
		return field.Origin
	}
	return ""
}

func (t *candidateOriginTracker) removeSubtree(path string) {
	for fieldPath := range t.fields {
		if fieldPath == path ||
			strings.HasPrefix(fieldPath, path+".") ||
			strings.HasPrefix(fieldPath, path+"[") {
			delete(t.fields, fieldPath)
		}
	}
}

func (t *candidateOriginTracker) sorted() []CandidateFieldOrigin {
	result := make([]CandidateFieldOrigin, 0, len(t.fields))
	for _, field := range t.fields {
		result = append(result, field)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Path < result[right].Path
	})
	return result
}

func (t *candidateOriginTracker) sortedReferences() []string {
	result := make([]string, 0, len(t.references))
	for id := range t.references {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		clone.Content[index] = cloneYAMLNode(child)
	}
	clone.Alias = nil
	return &clone
}

func joinFieldPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func indexedFieldPath(path string, index int) string {
	return fmt.Sprintf("%s[%d]", path, index)
}
