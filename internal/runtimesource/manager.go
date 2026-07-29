package runtimesource

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

type CandidateValidator interface {
	ValidateSourceCandidate(context.Context, []byte) (ValidatedCandidate, error)
}

type ValidatedCandidate struct {
	YAML   []byte
	SHA256 string
}

type SourceFetcher interface {
	Fetch(context.Context, SourceConfig, ConditionalRequest) (FetchResult, error)
}

type Reporter func(stage string, progress int, cancellable bool) error

type ManagerError struct {
	Code      string
	Message   string
	Retryable bool
	Cause     error
}

func (e *ManagerError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *ManagerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Manager struct {
	State     *runtimestate.Store
	Fetcher   SourceFetcher
	Validator CandidateValidator
	Now       func() time.Time
	Random    func() float64
}

func (m *Manager) Execute(
	ctx context.Context,
	operation runtimeapi.Operation,
	report Reporter,
) (*runtimeapi.OperationResult, error) {
	if m == nil || m.State == nil || m.Fetcher == nil || m.Validator == nil {
		return nil, errors.New("Runtime source manager is incomplete")
	}
	switch operation.Action.Kind {
	case runtimeapi.ActionAddRemoteSource:
		return m.addRemoteSource(ctx, operation, report)
	case runtimeapi.ActionRefreshSource:
		return m.refreshSource(ctx, operation, report)
	default:
		return nil, errors.New("Runtime source action is unsupported")
	}
}

func (m *Manager) DueActions(now time.Time) ([]runtimeapi.Action, error) {
	if m == nil || m.State == nil {
		return nil, errors.New("Runtime source manager is unavailable")
	}
	record, found, err := m.State.DueCurrentRemoteSource(now)
	if err != nil || !found {
		return nil, err
	}
	active, err := m.State.HasActiveSourceRefresh(record.ID)
	if err != nil || active {
		return nil, err
	}
	return []runtimeapi.Action{{
		Kind: runtimeapi.ActionRefreshSource,
		Params: runtimeapi.ActionParams{
			SourceID: record.ID,
		},
	}}, nil
}

func (m *Manager) addRemoteSource(
	ctx context.Context,
	operation runtimeapi.Operation,
	report Reporter,
) (*runtimeapi.OperationResult, error) {
	if err := report("reading_source_draft", 10, true); err != nil {
		return nil, err
	}
	body, content, err := m.State.ConsumeImport(
		operation.Action.Params.ContentID,
		operation.CallerIdentity,
		operation.ID,
		m.now(),
	)
	if err != nil {
		return nil, &ManagerError{
			Code:    sourceImportErrorCode(err),
			Message: "The remote source draft is unavailable or no longer usable",
			Cause:   err,
		}
	}
	if normalizedContentType(content.ContentType) != runtimeapi.SourceDraftContentType {
		return nil, &ManagerError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The uploaded content is not a remote source draft",
		}
	}
	draft, err := decodeSourceDraft(body)
	if err != nil {
		return nil, &ManagerError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The remote source draft is invalid",
			Cause:   err,
		}
	}
	config, err := NormalizeDraft(draft)
	if err != nil {
		return nil, &ManagerError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The remote source settings are outside the Runtime safety policy",
			Cause:   err,
		}
	}
	if err := report("downloading_source", 30, true); err != nil {
		return nil, err
	}
	fetched, err := m.Fetcher.Fetch(ctx, config, ConditionalRequest{})
	if err != nil {
		return nil, managerFetchError(err)
	}
	if fetched.NotModified || len(fetched.Body) == 0 {
		return nil, &ManagerError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "The remote source did not return a configuration",
		}
	}
	if err := report("validating_source_candidate", 60, true); err != nil {
		return nil, err
	}
	candidate, err := m.Validator.ValidateSourceCandidate(ctx, fetched.Body)
	if err != nil {
		return nil, &ManagerError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Mihomo rejected the downloaded source candidate",
			Cause:   err,
		}
	}
	if err := validateCandidateDigest(candidate); err != nil {
		return nil, err
	}
	now := m.now()
	next := nextRegularRefresh(
		now,
		time.Duration(config.RefreshIntervalSeconds)*time.Second,
		m.random(),
	)
	record := recordFromConfig(config)
	record.ETag = fetched.ETag
	record.LastModified = fetched.LastModified
	record.LastRefreshResult = "validated"
	record.LastRefreshRoute = config.Route
	record.LastRefreshAt = timePointer(now)
	record.LastManualRefreshAt = timePointer(now)
	record.NextRefreshAt = next
	if err := report("saving_source", 85, false); err != nil {
		return nil, err
	}
	record, err = m.State.CreateRemoteSource(record, fetched.Body, candidate.YAML, operation.ID, now)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		CandidateSHA256: candidate.SHA256,
		SourceID:        record.ID,
		RefreshResult:   record.LastRefreshResult,
		RefreshRoute:    record.LastRefreshRoute,
		NextRefreshAt:   cloneTime(record.NextRefreshAt),
	}, nil
}

func (m *Manager) refreshSource(
	ctx context.Context,
	operation runtimeapi.Operation,
	report Reporter,
) (*runtimeapi.OperationResult, error) {
	record, err := m.State.GetRemoteSource(operation.Action.Params.SourceID)
	if err != nil {
		code := runtimeapi.ErrorServiceUnavailable
		if errors.Is(err, runtimestate.ErrSourceNotFound) {
			code = runtimeapi.ErrorNotFound
		}
		return nil, &ManagerError{Code: code, Message: "The remote source is unavailable", Cause: err}
	}
	manual := !strings.HasPrefix(operation.CallerIdentity, "runtime:")
	now := m.now()
	if manual && record.LastManualRefreshAt != nil &&
		now.Sub(record.LastManualRefreshAt.UTC()) < ManualRefreshDebounce {
		return nil, &ManagerError{
			Code:      runtimeapi.ErrorBusy,
			Message:   "The remote source was refreshed too recently",
			Retryable: true,
		}
	}
	config, err := configFromRecord(record)
	if err != nil {
		return nil, err
	}
	route := operation.Action.Params.Route
	if route == "" {
		route = record.Route
	}
	if route != runtimeapi.SourceRouteDirect && route != runtimeapi.SourceRouteMihomo {
		return nil, &ManagerError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The selected remote source route is invalid",
		}
	}
	config.Route = route
	if err := report("downloading_source", 30, true); err != nil {
		return nil, err
	}
	fetched, fetchErr := m.Fetcher.Fetch(ctx, config, ConditionalRequest{
		ETag:         record.ETag,
		LastModified: record.LastModified,
	})
	if fetchErr != nil {
		return nil, m.recordRefreshFailure(record, route, manual, operation.ID, fetchErr, now)
	}
	next := nextRegularRefresh(
		now,
		time.Duration(record.RefreshIntervalSeconds)*time.Second,
		m.random(),
	)
	if fetched.NotModified {
		if record.RawSHA256 == "" || record.CandidateSHA256 == "" {
			err := errors.New("remote source returned not-modified without a validated revision")
			return nil, m.recordRefreshFailure(record, route, manual, operation.ID, err, now)
		}
		if fetched.ETag == "" {
			fetched.ETag = record.ETag
		}
		if fetched.LastModified == "" {
			fetched.LastModified = record.LastModified
		}
		updated, err := m.State.CommitRemoteSourceRefresh(record.ID, runtimestate.SourceRefreshSuccess{
			ETag:          fetched.ETag,
			LastModified:  fetched.LastModified,
			Result:        "not_modified",
			Route:         route,
			NextRefreshAt: next,
			Manual:        manual,
			NotModified:   true,
		}, operation.ID, now)
		if err != nil {
			return nil, err
		}
		return &runtimeapi.OperationResult{
			CandidateSHA256: updated.CandidateSHA256,
			SourceID:        updated.ID,
			RefreshResult:   updated.LastRefreshResult,
			RefreshRoute:    updated.LastRefreshRoute,
			NextRefreshAt:   cloneTime(updated.NextRefreshAt),
			NotModified:     true,
		}, nil
	}
	if err := report("validating_source_candidate", 60, true); err != nil {
		return nil, err
	}
	candidate, validationErr := m.Validator.ValidateSourceCandidate(ctx, fetched.Body)
	if validationErr != nil {
		return nil, m.recordRefreshFailure(record, route, manual, operation.ID, &ManagerError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Mihomo rejected the downloaded source candidate",
			Cause:   validationErr,
		}, now)
	}
	if err := validateCandidateDigest(candidate); err != nil {
		return nil, err
	}
	if err := report("saving_source", 85, false); err != nil {
		return nil, err
	}
	updated, err := m.State.CommitRemoteSourceRefresh(record.ID, runtimestate.SourceRefreshSuccess{
		ETag:            fetched.ETag,
		LastModified:    fetched.LastModified,
		Result:          "validated",
		Route:           route,
		NextRefreshAt:   next,
		Manual:          manual,
		Raw:             fetched.Body,
		Candidate:       candidate.YAML,
		RawSHA256:       fetched.SHA256,
		CandidateSHA256: candidate.SHA256,
	}, operation.ID, now)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		CandidateSHA256: updated.CandidateSHA256,
		SourceID:        updated.ID,
		RefreshResult:   updated.LastRefreshResult,
		RefreshRoute:    updated.LastRefreshRoute,
		NextRefreshAt:   cloneTime(updated.NextRefreshAt),
	}, nil
}

func (m *Manager) recordRefreshFailure(
	record runtimestate.RemoteSourceRecord,
	route string,
	manual bool,
	operationID string,
	cause error,
	now time.Time,
) error {
	class := FailureConfig
	result := "candidate_invalid"
	retryable := false
	var retryAfter time.Duration
	message := "The downloaded source did not produce a usable candidate"
	var fetchError *FetchError
	var managerError *ManagerError
	switch {
	case errors.As(cause, &fetchError):
		class = fetchError.Class
		result = fetchError.Result
		retryable = fetchError.Retryable
		retryAfter = fetchError.RetryAfter
		message = fetchError.Message
	case errors.As(cause, &managerError):
		result = "candidate_invalid"
		retryable = managerError.Retryable
		message = managerError.Message
	}
	normalInterval := time.Duration(record.RefreshIntervalSeconds) * time.Second
	var next *time.Time
	if class == FailureTemporary {
		next = nextFailureRefresh(now, record.AttemptCount, normalInterval, retryAfter, m.random())
	} else {
		next = nextRegularRefresh(now, normalInterval, m.random())
	}
	if _, err := m.State.RecordRemoteSourceFailure(record.ID, runtimestate.SourceRefreshFailure{
		Result:        result,
		Route:         route,
		FailureClass:  class,
		NextRefreshAt: next,
		Manual:        manual,
	}, operationID, now); err != nil {
		return errors.Join(cause, err)
	}
	return &ManagerError{
		Code:      managerErrorCode(cause),
		Message:   message,
		Retryable: retryable,
		Cause:     cause,
	}
}

func recordFromConfig(config SourceConfig) runtimestate.RemoteSourceRecord {
	return runtimestate.RemoteSourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   config.Name,
		URL:                    config.URL.String(),
		RedactedTarget:         config.RedactedTarget,
		Route:                  config.Route,
		UserAgent:              config.UserAgent,
		Username:               config.Username,
		Password:               config.Password,
		AuthorizedTarget:       config.AuthorizedTarget,
		AllowPrivate:           config.AllowPrivate,
		AllowHTTP:              config.AllowHTTP,
		CustomCAPEM:            config.CustomCAPEM,
		SkipTLSVerify:          config.SkipTLSVerify,
		RefreshIntervalSeconds: config.RefreshIntervalSeconds,
		TimeoutSeconds:         int(config.Timeout / time.Second),
		MaxResponseBytes:       config.MaxResponseBytes,
	}
}

func configFromRecord(record runtimestate.RemoteSourceRecord) (SourceConfig, error) {
	interval := record.RefreshIntervalSeconds
	config, err := NormalizeDraft(runtimeapi.RemoteSourceDraft{
		Name:                   record.Name,
		URL:                    record.URL,
		Route:                  record.Route,
		UserAgent:              record.UserAgent,
		Username:               record.Username,
		Password:               record.Password,
		AuthorizedTarget:       record.AuthorizedTarget,
		AllowPrivate:           record.AllowPrivate,
		AllowHTTP:              record.AllowHTTP,
		CustomCAPEM:            record.CustomCAPEM,
		SkipTLSVerify:          record.SkipTLSVerify,
		RefreshIntervalSeconds: &interval,
		TimeoutSeconds:         record.TimeoutSeconds,
		MaxResponseBytes:       record.MaxResponseBytes,
	})
	if err != nil {
		return SourceConfig{}, &ManagerError{
			Code:    runtimeapi.ErrorInternal,
			Message: "The stored remote source settings are invalid",
			Cause:   err,
		}
	}
	return config, nil
}

func decodeSourceDraft(body []byte) (runtimeapi.RemoteSourceDraft, error) {
	var draft runtimeapi.RemoteSourceDraft
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&draft); err != nil {
		return runtimeapi.RemoteSourceDraft{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return runtimeapi.RemoteSourceDraft{}, errors.New("remote source draft contains trailing JSON")
	}
	return draft, nil
}

func validateCandidateDigest(candidate ValidatedCandidate) error {
	if len(candidate.YAML) == 0 {
		return errors.New("validated remote source candidate is empty")
	}
	digest := sha256.Sum256(candidate.YAML)
	actual := hex.EncodeToString(digest[:])
	if len(candidate.SHA256) != 64 || !strings.EqualFold(candidate.SHA256, actual) {
		return errors.New("validated remote source candidate digest is invalid")
	}
	return nil
}

func managerFetchError(err error) error {
	var fetchError *FetchError
	if !errors.As(err, &fetchError) {
		return &ManagerError{
			Code:      runtimeapi.ErrorServiceUnavailable,
			Message:   "Remote source refresh failed",
			Retryable: true,
			Cause:     err,
		}
	}
	return &ManagerError{
		Code:      managerErrorCode(fetchError),
		Message:   fetchError.Message,
		Retryable: fetchError.Retryable,
		Cause:     err,
	}
}

func managerErrorCode(err error) string {
	var fetchError *FetchError
	if errors.As(err, &fetchError) {
		switch fetchError.Class {
		case FailurePolicy, FailureConfig:
			return runtimeapi.ErrorInvalidRequest
		case FailureAuth:
			return runtimeapi.ErrorSourceAuthentication
		default:
			return runtimeapi.ErrorServiceUnavailable
		}
	}
	var managerError *ManagerError
	if errors.As(err, &managerError) && managerError.Code != "" {
		return managerError.Code
	}
	return runtimeapi.ErrorInvalidRequest
}

func sourceImportErrorCode(err error) string {
	switch {
	case errors.Is(err, runtimestate.ErrImportExpired):
		return runtimeapi.ErrorContentExpired
	case errors.Is(err, runtimestate.ErrImportConsumed):
		return runtimeapi.ErrorContentConsumed
	case errors.Is(err, runtimestate.ErrImportOwner):
		return runtimeapi.ErrorUnauthorized
	default:
		return runtimeapi.ErrorNotFound
	}
}

func normalizedContentType(contentType string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
}

func (m *Manager) now() time.Time {
	if m != nil && m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) random() float64 {
	if m != nil && m.Random != nil {
		return boundedRandom(m.Random())
	}
	var encoded [8]byte
	if _, err := rand.Read(encoded[:]); err != nil {
		return 0.5
	}
	return float64(binary.BigEndian.Uint64(encoded[:])>>11) / float64(uint64(1)<<53)
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}
