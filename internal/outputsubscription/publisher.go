package outputsubscription

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"submux/internal/compiler"
	"submux/internal/outputupdate"
	"submux/internal/store"
)

const maxInputRetries = 3

const (
	OutcomeCompleted = "completed"
	OutcomeDegraded  = "degraded"
	OutcomeBlocked   = "blocked"
	OutcomeStale     = "stale"
)

var (
	ErrInvalidIntent = errors.New("invalid output subscription intent")
	ErrNotFound      = errors.New("output subscription not found")
	ErrConflict      = errors.New("output subscription changed concurrently")
	ErrInternal      = errors.New("output subscription operation failed")
)

// SaveIntent contains only administrator-controlled fields. Publisher owns
// engine selection, tokens, enabled state and optimistic record versions.
type SaveIntent struct {
	ID                int64                       `json:"id"`
	Name              string                      `json:"name"`
	TemplateVersionID int64                       `json:"template_version_id"`
	RuleProfileID     int64                       `json:"rule_profile_id,omitempty"`
	Bindings          []store.SubscriptionBinding `json:"bindings"`
	ExpiresAt         string                      `json:"expires_at,omitempty"`
}

// Result describes the requested output subscription only. Work on other due
// subscriptions remains an implementation detail of the update executor.
type Result struct {
	SubscriptionID int64
	Token          string
	Revision       string
	Enabled        bool
	Outcome        string
	Detail         string
}

type Publisher struct {
	store   *store.Store
	compile func(store.OutputSubscription) (compiler.Result, error)
	retry   func(int64) (outputupdate.Report, error)
	now     func() time.Time
	token   func() (string, error)
}

func New(st *store.Store, compilerModule *compiler.Service, updater *outputupdate.Service) *Publisher {
	return newWithDependencies(st, compilerModule.Preview, updater.Retry, time.Now, randomToken)
}

func newWithDependencies(
	st *store.Store,
	compile func(store.OutputSubscription) (compiler.Result, error),
	retry func(int64) (outputupdate.Report, error),
	now func() time.Time,
	token func() (string, error),
) *Publisher {
	return &Publisher{store: st, compile: compile, retry: retry, now: now, token: token}
}

func (p *Publisher) Save(intent SaveIntent) (Result, error) {
	var base store.OutputSubscription
	if intent.ID == 0 {
		token, err := p.token()
		if err != nil {
			return Result{}, internalError(err)
		}
		base = store.OutputSubscription{Token: token, Enabled: true}
	} else {
		current, err := p.store.GetOutputSubscription(intent.ID)
		if err != nil {
			return Result{}, notFoundError(intent.ID)
		}
		base = current
	}
	return p.compileAndSave(intent, base, base.Enabled)
}

func (p *Publisher) SetEnabled(id int64, enabled bool) (Result, error) {
	current, err := p.store.GetOutputSubscription(id)
	if err != nil {
		return Result{}, notFoundError(id)
	}
	if enabled {
		return p.compileAndSave(SaveIntent{
			ID: current.ID, Name: current.Name, TemplateVersionID: current.TemplateVersionID,
			RuleProfileID: current.RuleProfileID, Bindings: cloneBindings(current.Bindings),
			ExpiresAt: current.ExpiresAt,
		}, current, true)
	}
	if err := p.store.DisableOutputSubscriptionIfCurrent(id, current.RecordVersion); err != nil {
		if errors.Is(err, store.ErrOutputSubscriptionChanged) {
			return Result{}, conflictError(err)
		}
		return Result{}, internalError(err)
	}
	revision := ""
	if artifact, artifactErr := p.store.GetSubscriptionArtifact(id); artifactErr == nil {
		revision = artifact.Revision
	}
	return Result{
		SubscriptionID: id, Token: current.Token, Revision: revision,
		Enabled: false, Outcome: OutcomeCompleted,
	}, nil
}

func (p *Publisher) Republish(id int64) (Result, error) {
	current, err := p.store.GetOutputSubscription(id)
	if err != nil {
		return Result{}, notFoundError(id)
	}
	if !current.Enabled {
		return Result{}, invalidError(fmt.Errorf("output subscription %d is disabled", id))
	}
	report, err := p.retry(id)
	if err != nil {
		latest, latestErr := p.store.GetOutputSubscription(id)
		if latestErr != nil || !latest.Enabled {
			return Result{SubscriptionID: id, Outcome: OutcomeStale}, nil
		}
		return Result{}, internalError(err)
	}
	if report.Error != "" {
		return Result{}, internalError(errors.New(report.Error))
	}
	attempt, ok := report.ResultFor(id)
	if !ok {
		return Result{SubscriptionID: id, Token: current.Token, Enabled: true, Outcome: OutcomeStale}, nil
	}
	if attempt.Outcome == "failed" {
		return Result{}, internalError(errors.New(attempt.Error))
	}
	outcome := attempt.Outcome
	if outcome == OutcomeDegraded {
		if update, updateErr := p.store.GetSubscriptionUpdate(id); updateErr == nil && update.Status == store.SubscriptionUpdateBlocked {
			outcome = OutcomeBlocked
		}
	}
	revision := ""
	if artifact, artifactErr := p.store.GetSubscriptionArtifact(id); artifactErr == nil {
		revision = artifact.Revision
	}
	return Result{
		SubscriptionID: id, Token: current.Token, Revision: revision,
		Enabled: true, Outcome: outcome, Detail: attempt.Error,
	}, nil
}

func (p *Publisher) compileAndSave(intent SaveIntent, base store.OutputSubscription, enabled bool) (Result, error) {
	for range maxInputRetries {
		inputsGeneration, err := p.store.OutputInputsGeneration()
		if err != nil {
			return Result{}, internalError(err)
		}
		value, err := p.prepare(intent, base, enabled)
		if err != nil {
			return Result{}, err
		}
		compiled, err := p.compile(value)
		if err != nil {
			return Result{}, invalidError(err)
		}
		id, err := p.store.SaveOutputSubscriptionWithArtifact(value, store.SubscriptionArtifact{
			Body: compiled.Body, ContentType: compiled.ContentType, Revision: compiled.Revision,
		}, compiled.Warnings, store.OutputPublicationGuard{
			InputsGeneration: inputsGeneration, SubscriptionVersion: base.RecordVersion,
		})
		switch {
		case errors.Is(err, store.ErrOutputInputsChanged):
			continue
		case errors.Is(err, store.ErrOutputSubscriptionChanged):
			return Result{}, conflictError(err)
		case err != nil:
			return Result{}, internalError(err)
		}
		return Result{
			SubscriptionID: id, Token: value.Token, Revision: compiled.Revision,
			Enabled: enabled, Outcome: OutcomeCompleted,
		}, nil
	}
	return Result{}, conflictError(store.ErrOutputInputsChanged)
}

func (p *Publisher) prepare(intent SaveIntent, base store.OutputSubscription, enabled bool) (store.OutputSubscription, error) {
	name := strings.TrimSpace(intent.Name)
	if name == "" || intent.TemplateVersionID <= 0 {
		return store.OutputSubscription{}, invalidError(errors.New("name and template_version_id are required"))
	}
	version, err := p.store.GetTemplateVersion(intent.TemplateVersionID)
	if err != nil {
		return store.OutputSubscription{}, invalidError(fmt.Errorf("template version %d does not exist", intent.TemplateVersionID))
	}
	template, err := p.store.GetTemplate(version.TemplateID)
	if err != nil {
		return store.OutputSubscription{}, invalidError(fmt.Errorf("template %d does not exist", version.TemplateID))
	}
	ruleProfileID := intent.RuleProfileID
	if template.Engine == compiler.EngineMihomo {
		if ruleProfileID == 0 && base.ID != 0 {
			ruleProfileID = base.RuleProfileID
		}
		if ruleProfileID == 0 {
			defaultProfile, defaultErr := p.store.GetRuleProfileByKey("default")
			if defaultErr != nil {
				return store.OutputSubscription{}, invalidError(errors.New("a valid rule profile is required for mihomo subscriptions"))
			}
			ruleProfileID = defaultProfile.ID
		}
		if _, profileErr := p.store.GetRuleProfile(ruleProfileID); profileErr != nil {
			return store.OutputSubscription{}, invalidError(errors.New("a valid rule profile is required for mihomo subscriptions"))
		}
	} else if ruleProfileID != 0 {
		return store.OutputSubscription{}, invalidError(errors.New("rule profiles are only supported by mihomo subscriptions"))
	}
	bindings, err := p.validateBindings(intent.Bindings)
	if err != nil {
		return store.OutputSubscription{}, err
	}
	expiresAt := ""
	if intent.ExpiresAt != "" {
		expires, parseErr := time.Parse(time.RFC3339, intent.ExpiresAt)
		if parseErr != nil || !expires.After(p.now()) {
			return store.OutputSubscription{}, invalidError(errors.New("expires_at must be a future RFC3339 timestamp"))
		}
		expiresAt = expires.UTC().Format(time.RFC3339)
	}
	return store.OutputSubscription{
		ID: base.ID, Name: name, TemplateVersionID: intent.TemplateVersionID,
		RuleProfileID: ruleProfileID, Engine: template.Engine, Bindings: bindings,
		Token: base.Token, Enabled: enabled, ExpiresAt: expiresAt,
		RecordVersion: base.RecordVersion,
	}, nil
}

func (p *Publisher) validateBindings(values []store.SubscriptionBinding) ([]store.SubscriptionBinding, error) {
	out := cloneBindings(values)
	seenSlots := map[string]bool{}
	for index := range out {
		out[index].Slot = strings.TrimSpace(out[index].Slot)
		if out[index].Slot == "" || seenSlots[out[index].Slot] {
			return nil, invalidError(errors.New("each template slot may be selected once"))
		}
		seenSlots[out[index].Slot] = true
		seenNodes := map[int64]bool{}
		for _, nodeID := range out[index].NodeIDs {
			if nodeID <= 0 || seenNodes[nodeID] {
				return nil, invalidError(fmt.Errorf("slot %q contains an invalid or duplicate node", out[index].Slot))
			}
			seenNodes[nodeID] = true
			nodeValue, err := p.store.GetNode(nodeID)
			if err != nil {
				return nil, invalidError(fmt.Errorf("unknown node %d", nodeID))
			}
			if nodeValue.Role == "notice" {
				return nil, invalidError(fmt.Errorf("node %d is an informational notice", nodeID))
			}
		}
	}
	return out, nil
}

func cloneBindings(values []store.SubscriptionBinding) []store.SubscriptionBinding {
	out := make([]store.SubscriptionBinding, len(values))
	for index, value := range values {
		out[index] = store.SubscriptionBinding{
			Slot: value.Slot, NodeIDs: append([]int64(nil), value.NodeIDs...),
		}
	}
	return out
}

func randomToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func invalidError(err error) error {
	return fmt.Errorf("%w: %v", ErrInvalidIntent, err)
}

func notFoundError(id int64) error {
	return fmt.Errorf("%w: no output subscription with id %d", ErrNotFound, id)
}

func conflictError(err error) error {
	return fmt.Errorf("%w: %v", ErrConflict, err)
}

func internalError(err error) error {
	return fmt.Errorf("%w: %v", ErrInternal, err)
}
