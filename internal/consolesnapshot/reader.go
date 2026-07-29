package consolesnapshot

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"submux/internal/fakeip"
	"submux/internal/lifecycle"
	"submux/internal/resourceproxy"
	"submux/internal/rulecatalog"
	"submux/internal/store"
)

const defaultFetchIntervalSec = 10800

type Settings struct {
	BaseURL               string               `json:"base_url"`
	FetchIntervalSec      int                  `json:"fetch_interval_sec"`
	PlatformResourceProxy resourceproxy.Config `json:"platform_resource_proxy"`
	SharedFakeIPFilter    fakeip.Config        `json:"shared_fake_ip_filter"`
}

type Source struct {
	ID               int64             `json:"id"`
	Kind             string            `json:"kind"`
	Builtin          bool              `json:"builtin,omitempty"`
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	Tags             []string          `json:"tags,omitempty"`
	URL              string            `json:"url,omitempty"`
	UserAgent        string            `json:"user_agent,omitempty"`
	Enabled          bool              `json:"enabled"`
	SortOrder        int               `json:"sort_order"`
	LifecyclePolicy  string            `json:"lifecycle_policy,omitempty"`
	WarnBeforeDays   int               `json:"warn_before_days,omitempty"`
	TrustNodeNotices bool              `json:"trust_node_notices,omitempty"`
	FetchMode        string            `json:"fetch_mode,omitempty"`
	NodeCount        int               `json:"node_count"`
	NoticeCount      int               `json:"notice_count,omitempty"`
	LastSuccessAt    string            `json:"last_success_at,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
	LastSuccessRoute string            `json:"last_success_route,omitempty"`
	LastDirectError  string            `json:"last_direct_error,omitempty"`
	LastProxyError   string            `json:"last_proxy_error,omitempty"`
	Userinfo         string            `json:"userinfo,omitempty"`
	Lifecycle        *lifecycle.Status `json:"lifecycle,omitempty"`
}

type Problem struct {
	Code         string `json:"code"`
	ResourceKind string `json:"resource_kind"`
	ResourceID   int64  `json:"resource_id"`
	Message      string `json:"message"`
}

type Template struct {
	store.Template
	Versions []store.TemplateVersion `json:"versions"`
	Problems []Problem               `json:"problems,omitempty"`
}

type ArtifactStatus struct {
	ContentType string `json:"content_type,omitempty"`
	Revision    string `json:"revision,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type OutputSubscription struct {
	store.OutputSubscription
	Artifact *ArtifactStatus                `json:"artifact,omitempty"`
	Update   *store.SubscriptionUpdateState `json:"update,omitempty"`
	URL      string                         `json:"url"`
	Scenario string                         `json:"scenario,omitempty"`
	Problems []Problem                      `json:"problems,omitempty"`
}

type RuleCatalog struct {
	rulecatalog.Snapshot
	ActiveCommit   string                   `json:"active_commit"`
	EmbeddedCommit string                   `json:"embedded_commit"`
	Refresh        rulecatalog.RefreshState `json:"refresh"`
}

type Snapshot struct {
	GeneratedAt         string                 `json:"generated_at"`
	Settings            Settings               `json:"settings"`
	Sources             []Source               `json:"sources"`
	Nodes               []store.NodeRecord     `json:"nodes"`
	LifecycleEvents     []store.LifecycleEvent `json:"lifecycle_events"`
	Templates           []Template             `json:"templates"`
	RuleCatalog         RuleCatalog            `json:"rule_catalog"`
	RuleProfiles        []store.RuleProfile    `json:"rule_profiles"`
	OutputSubscriptions []OutputSubscription   `json:"subscriptions"`
}

type Reader struct {
	store *store.Store
	now   func() time.Time
}

func New(st *store.Store) *Reader {
	return &Reader{store: st, now: time.Now}
}

func (r *Reader) Read() (Snapshot, error) {
	state, err := r.store.ReadConsoleState()
	if err != nil {
		return Snapshot{}, err
	}
	now := r.now().UTC()
	settings, err := consoleSettings(state.Settings)
	if err != nil {
		return Snapshot{}, err
	}
	catalog, err := activeRuleCatalog(state)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		GeneratedAt:         now.Format(time.RFC3339),
		Settings:            settings,
		Sources:             consoleSources(state, now),
		Nodes:               append([]store.NodeRecord{}, state.Nodes...),
		LifecycleEvents:     append([]store.LifecycleEvent{}, state.LifecycleEvents...),
		Templates:           consoleTemplates(state),
		RuleCatalog:         catalog,
		RuleProfiles:        append([]store.RuleProfile{}, state.RuleProfiles...),
		OutputSubscriptions: consoleOutputSubscriptions(state),
	}, nil
}

func consoleSettings(values map[string]string) (Settings, error) {
	interval := defaultFetchIntervalSec
	if raw := values["fetch_interval_sec"]; raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			interval = parsed
		}
	}
	proxy := resourceproxy.Normalize(resourceproxy.Config{
		Mode: values[resourceproxy.SettingMode],
		URL:  values[resourceproxy.SettingURL],
	})
	if err := resourceproxy.Validate(proxy); err != nil {
		proxy = resourceproxy.Config{Mode: resourceproxy.ModeDirect}
	}
	sharedFakeIP, err := fakeip.Parse(values[fakeip.SettingKey])
	if err != nil {
		return Settings{}, fmt.Errorf("decode console shared fake-ip filter setting: %w", err)
	}
	return Settings{
		BaseURL:               values["base_url"],
		FetchIntervalSec:      interval,
		PlatformResourceProxy: proxy,
		SharedFakeIPFilter:    sharedFakeIP,
	}, nil
}

func consoleSources(state store.ConsoleState, now time.Time) []Source {
	nodeCounts := make(map[int64]int)
	noticeCounts := make(map[int64]int)
	for _, node := range state.Nodes {
		if node.Role == "notice" {
			noticeCounts[node.SourceID]++
		} else {
			nodeCounts[node.SourceID]++
		}
	}
	values := make([]Source, 0, len(state.Sources))
	for _, source := range state.Sources {
		value := Source{
			ID: source.ID, Kind: source.Kind, Builtin: source.Builtin,
			Name: source.Name, Description: source.Description, Tags: source.Tags,
			URL: source.URL, UserAgent: source.UserAgent, Enabled: source.Enabled,
			SortOrder: source.SortOrder, LifecyclePolicy: source.LifecyclePolicy,
			WarnBeforeDays: source.WarnBeforeDays, TrustNodeNotices: source.TrustNodeNotices,
			FetchMode: source.FetchMode, NodeCount: nodeCounts[source.ID],
			NoticeCount: noticeCounts[source.ID],
		}
		if source.Kind == store.SourceKindSubscription {
			cache := state.Caches[source.ID]
			value.LastSuccessAt = cache.LastSuccessAt
			value.LastError = cache.LastError
			value.LastSuccessRoute = cache.LastSuccessRoute
			value.LastDirectError = cache.LastDirectError
			value.LastProxyError = cache.LastProxyError
			value.Userinfo = cache.UserinfoJSON
			status := lifecycle.Evaluate(source, cache, now)
			value.Lifecycle = &status
		}
		values = append(values, value)
	}
	return values
}

func consoleTemplates(state store.ConsoleState) []Template {
	versionsByTemplate := make(map[int64][]store.TemplateVersion)
	versionByID := make(map[int64]store.TemplateVersion)
	for _, version := range state.TemplateVersions {
		versionsByTemplate[version.TemplateID] = append(versionsByTemplate[version.TemplateID], version)
		versionByID[version.ID] = version
	}
	values := make([]Template, 0, len(state.Templates))
	for _, template := range state.Templates {
		value := Template{
			Template: template,
			Versions: append([]store.TemplateVersion{}, versionsByTemplate[template.ID]...),
		}
		if template.CurrentVersionID != 0 {
			if _, exists := versionByID[template.CurrentVersionID]; !exists {
				value.Problems = append(value.Problems, Problem{
					Code: "missing_current_template_version", ResourceKind: "template",
					ResourceID: template.ID,
					Message:    fmt.Sprintf("current template version %d does not exist", template.CurrentVersionID),
				})
			}
		}
		values = append(values, value)
	}
	return values
}

func consoleOutputSubscriptions(state store.ConsoleState) []OutputSubscription {
	nodes := make(map[int64]bool, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes[node.ID] = true
	}
	templates := make(map[int64]store.Template, len(state.Templates))
	for _, template := range state.Templates {
		templates[template.ID] = template
	}
	versions := make(map[int64]store.TemplateVersion, len(state.TemplateVersions))
	for _, version := range state.TemplateVersions {
		versions[version.ID] = version
	}
	ruleProfiles := make(map[int64]bool, len(state.RuleProfiles))
	for _, profile := range state.RuleProfiles {
		ruleProfiles[profile.ID] = true
	}

	baseURL := strings.TrimRight(state.Settings["base_url"], "/")
	values := make([]OutputSubscription, 0, len(state.OutputSubscriptions))
	for _, subscription := range state.OutputSubscriptions {
		value := OutputSubscription{
			OutputSubscription: subscription,
			URL:                "/sub/" + subscription.Token,
		}
		if baseURL != "" {
			value.URL = baseURL + value.URL
		}
		if version, exists := versions[subscription.TemplateVersionID]; !exists {
			value.Problems = append(value.Problems, Problem{
				Code: "missing_template_version", ResourceKind: "output_subscription",
				ResourceID: subscription.ID,
				Message:    fmt.Sprintf("template version %d does not exist", subscription.TemplateVersionID),
			})
		} else if template, exists := templates[version.TemplateID]; !exists {
			value.Problems = append(value.Problems, Problem{
				Code: "missing_template", ResourceKind: "output_subscription",
				ResourceID: subscription.ID,
				Message:    fmt.Sprintf("template %d does not exist", version.TemplateID),
			})
		} else {
			value.Scenario = template.Scenario
		}
		if subscription.RuleProfileID != 0 && !ruleProfiles[subscription.RuleProfileID] {
			value.Problems = append(value.Problems, Problem{
				Code: "missing_rule_profile", ResourceKind: "output_subscription",
				ResourceID: subscription.ID,
				Message:    fmt.Sprintf("rule profile %d does not exist", subscription.RuleProfileID),
			})
		}
		missingNodes := make(map[int64]bool)
		for _, binding := range subscription.Bindings {
			for _, nodeID := range binding.NodeIDs {
				if !nodes[nodeID] {
					missingNodes[nodeID] = true
				}
			}
		}
		missingNodeIDs := make([]int64, 0, len(missingNodes))
		for nodeID := range missingNodes {
			missingNodeIDs = append(missingNodeIDs, nodeID)
		}
		sort.Slice(missingNodeIDs, func(i, j int) bool { return missingNodeIDs[i] < missingNodeIDs[j] })
		for _, nodeID := range missingNodeIDs {
			value.Problems = append(value.Problems, Problem{
				Code: "missing_node", ResourceKind: "output_subscription",
				ResourceID: subscription.ID,
				Message:    fmt.Sprintf("node %d does not exist", nodeID),
			})
		}
		if artifact, exists := state.SubscriptionArtifacts[subscription.ID]; exists {
			value.Artifact = &ArtifactStatus{
				ContentType: artifact.ContentType, Revision: artifact.Revision,
				LastSuccess: artifact.LastSuccess, UpdatedAt: artifact.UpdatedAt,
			}
		}
		if update, exists := state.SubscriptionUpdates[subscription.ID]; exists {
			copy := update
			value.Update = &copy
		}
		values = append(values, value)
	}
	return values
}

func activeRuleCatalog(state store.ConsoleState) (RuleCatalog, error) {
	activeCommit := strings.TrimSpace(state.Settings["rule_catalog_active_commit"])
	embedded := rulecatalog.Catalog()
	active := embedded
	if activeCommit != "" && activeCommit != embedded.Commit {
		if len(state.RuleCatalogRaw) == 0 {
			return RuleCatalog{}, fmt.Errorf("active rule catalog snapshot %q is missing", activeCommit)
		}
		if err := json.Unmarshal(state.RuleCatalogRaw, &active); err != nil {
			return RuleCatalog{}, fmt.Errorf("decode active rule catalog snapshot: %w", err)
		}
		if active.Commit != activeCommit || len(active.Entries) == 0 {
			return RuleCatalog{}, fmt.Errorf("active rule catalog snapshot %q is invalid", activeCommit)
		}
	}
	var refresh rulecatalog.RefreshState
	if raw := strings.TrimSpace(state.Settings["rule_catalog_refresh_state"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &refresh); err != nil {
			return RuleCatalog{}, fmt.Errorf("decode rule catalog refresh state: %w", err)
		}
	}
	return RuleCatalog{
		Snapshot: active, ActiveCommit: active.Commit,
		EmbeddedCommit: embedded.Commit, Refresh: refresh,
	}, nil
}
