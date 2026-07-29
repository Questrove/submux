package outputupdate

import (
	"context"
	"errors"
	"sync"
	"time"

	"submux/internal/compiler"
	"submux/internal/store"
)

const (
	defaultPollInterval = 15 * time.Second
	baseRetryDelay      = 30 * time.Second
	maxRetryDelay       = 6 * time.Hour
)

type Result struct {
	SubscriptionID int64  `json:"subscription_id"`
	Outcome        string `json:"outcome"`
	Error          string `json:"error,omitempty"`
}

type Report struct {
	Results []Result `json:"results"`
	Error   string   `json:"error,omitempty"`
}

func (r Report) ResultFor(subscriptionID int64) (Result, bool) {
	for _, result := range r.Results {
		if result.SubscriptionID == subscriptionID {
			return result, true
		}
	}
	return Result{}, false
}

// TemporaryError marks an operational compiler failure that should be retried.
// Pure configuration and lifecycle errors do not use this wrapper.
type TemporaryError struct{ Err error }

func (e *TemporaryError) Error() string { return e.Err.Error() }
func (e *TemporaryError) Unwrap() error { return e.Err }

// Service is the single owner of persistent artifact update execution. The
// store remains the source of truth.
type Service struct {
	store   *store.Store
	compile func(store.OutputSubscription) (compiler.Result, error)
	now     func() time.Time

	runMu sync.Mutex
}

func New(st *store.Store, compilerService *compiler.Service) *Service {
	return newWithCompiler(st, compilerService.Preview)
}

func newWithCompiler(st *store.Store, compile func(store.OutputSubscription) (compiler.Result, error)) *Service {
	return &Service{
		store: st, compile: compile, now: time.Now,
	}
}

// AttemptPending performs the synchronous first attempt for all work that is
// currently due. It deliberately has no request context: once the domain write
// commits, client disconnects must not cancel artifact production.
func (s *Service) AttemptPending() Report {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	now := s.now()
	pending, err := s.store.ListDueSubscriptionUpdates(now)
	if err != nil {
		return Report{Error: err.Error()}
	}
	report := Report{Results: make([]Result, 0, len(pending))}
	for _, work := range pending {
		report.Results = append(report.Results, s.attempt(work, now))
	}
	return report
}

func (s *Service) attempt(work store.PendingSubscriptionUpdate, now time.Time) Result {
	id := work.Subscription.ID
	generation := work.Update.InputGeneration
	compiled, err := s.compile(work.Subscription)
	if err == nil {
		committed, commitErr := s.store.CommitSubscriptionArtifactIfCurrent(id, generation, store.SubscriptionArtifact{
			Body: compiled.Body, ContentType: compiled.ContentType, Revision: compiled.Revision,
			LastSuccess: now.UTC().Format(time.RFC3339),
		}, compiled.Warnings)
		if commitErr != nil {
			return Result{SubscriptionID: id, Outcome: "failed", Error: commitErr.Error()}
		}
		if !committed {
			return Result{SubscriptionID: id, Outcome: "stale"}
		}
		return Result{SubscriptionID: id, Outcome: "completed"}
	}

	failureClass := store.SubscriptionFailureConfig
	blockedReason := ""
	warnings := []string(nil)
	nextAttempt := time.Time{}
	var blocked *compiler.BlockedError
	var temporary *TemporaryError
	switch {
	case errors.As(err, &blocked):
		failureClass = store.SubscriptionFailureLifecycle
		blockedReason = blocked.Reason
		warnings = append(warnings, blocked.Warnings...)
	case errors.As(err, &temporary):
		failureClass = store.SubscriptionFailureTemporary
		nextAttempt = now.Add(retryDelay(work.Update.AttemptCount))
	}
	recorded, recordErr := s.store.RecordSubscriptionUpdateFailureIfCurrent(
		id, generation, failureClass, err.Error(), blockedReason, warnings, nextAttempt,
	)
	if recordErr != nil {
		return Result{SubscriptionID: id, Outcome: "failed", Error: recordErr.Error()}
	}
	if !recorded {
		return Result{SubscriptionID: id, Outcome: "stale"}
	}
	return Result{SubscriptionID: id, Outcome: "degraded", Error: err.Error()}
}

func retryDelay(attemptCount int) time.Duration {
	delay := baseRetryDelay
	for i := 0; i < attemptCount && delay < maxRetryDelay; i++ {
		delay *= 2
	}
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func (s *Service) Retry(subscriptionID int64) (Report, error) {
	if _, err := s.store.MarkOutputSubscriptionPending(subscriptionID); err != nil {
		return Report{}, err
	}
	return s.AttemptPending(), nil
}

func (s *Service) Run(ctx context.Context) {
	s.AttemptPending()
	timer := time.NewTicker(defaultPollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.AttemptPending()
		}
	}
}
