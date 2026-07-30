package runtimenet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeapi"
)

type Manager struct {
	Root       string
	RuntimeUID uint32
	System     System
	Now        func() time.Time
	PlanTTL    time.Duration
	LeaseTTL   time.Duration

	mu        sync.Mutex
	epoch     string
	sessions  map[string]*sessionState
	plans     map[string]planState
	ownership *Ownership
	results   []CommittedResult
	key       []byte
}

type sessionState struct {
	RuntimeInstanceID string
	ClientNonce       string
	ServerNonce       string
	ExpiresAt         time.Time
	LastSequence      uint64
}

type planState struct {
	sessionID string
	preview   runtimeapi.NetworkPreview
	discovery Discovery
}

func OpenManager(root string, runtimeUID uint32, system System) (*Manager, error) {
	if system == nil {
		return nil, errors.New("privileged Runtime network system is required")
	}
	key, err := openIntegrityKey(root)
	if err != nil {
		return nil, err
	}
	epoch, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("create privileged Runtime network epoch: %w", err)
	}
	manager := &Manager{
		Root:       root,
		RuntimeUID: runtimeUID,
		System:     system,
		epoch:      epoch,
		sessions:   make(map[string]*sessionState),
		plans:      make(map[string]planState),
		key:        key,
	}
	state, err := readState(root, key)
	if err != nil {
		return nil, err
	}
	manager.ownership = state.Ownership
	manager.results = append([]CommittedResult(nil), state.Results...)
	return manager, nil
}

func (m *Manager) Recover(ctx context.Context) (runtimeapi.NetworkStatus, error) {
	if err := contextError(ctx); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ownership == nil {
		return m.observeLocked(ctx)
	}
	ownership := *m.ownership
	residuals, err := m.System.Cleanup(ctx, ownership)
	now := m.now()
	if err != nil || len(residuals) > 0 {
		ownership.State = runtimeapi.NetworkStateUnknown
		ownership.Residuals = append([]runtimeapi.NetworkObject(nil), residuals...)
		ownership.UpdatedAt = now
		m.ownership = &ownership
		if writeErr := m.persistLocked(&ownership, nil); writeErr != nil {
			return runtimeapi.NetworkStatus{}, errors.Join(err, writeErr)
		}
		status, observeErr := m.observeLocked(ctx)
		return status, errors.Join(err, observeErr)
	}
	if err := m.persistLocked(nil, nil); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	m.ownership = nil
	return m.observeLocked(ctx)
}

func (m *Manager) OpenSession(peerUID uint32, request SessionRequest) (Session, error) {
	if m == nil {
		return Session{}, errors.New("privileged Runtime network manager is unavailable")
	}
	if peerUID != m.RuntimeUID {
		return Session{}, errors.New("privileged Runtime network IPC denied this peer")
	}
	if request.ProtocolVersion != ProtocolVersion {
		return Session{}, errors.New("privileged Runtime network protocol is unsupported")
	}
	if !validOpaqueIdentifier(request.RuntimeInstanceID, 32, 128) {
		return Session{}, errors.New("Runtime installation ID is invalid")
	}
	if !validHex(request.ClientNonce, 64) {
		return Session{}, errors.New("Runtime network client nonce is invalid")
	}
	serverNonce, err := randomHex(32)
	if err != nil {
		return Session{}, err
	}
	hash := sha256.Sum256([]byte(m.epoch + "\x00" + request.ClientNonce + "\x00" + serverNonce))
	sessionID := hex.EncodeToString(hash[:])
	now := m.now()
	expiresAt := now.Add(24 * time.Hour)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionID] = &sessionState{
		RuntimeInstanceID: request.RuntimeInstanceID,
		ClientNonce:       request.ClientNonce,
		ServerNonce:       serverNonce,
		ExpiresAt:         expiresAt,
	}
	return Session{
		ProtocolVersion: ProtocolVersion,
		Epoch:           m.epoch,
		ID:              sessionID,
		ServerNonce:     serverNonce,
		ExpiresAt:       expiresAt,
	}, nil
}

func (m *Manager) Preview(
	ctx context.Context,
	sessionID string,
	request runtimeapi.NetworkPreviewRequest,
) (runtimeapi.NetworkPreview, error) {
	if err := contextError(ctx); err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	settings, err := normalizeTUNSettings(request)
	if err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.sessionLocked(sessionID); err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	discovery, err := m.System.Discover(ctx, settings)
	if err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	if discovery.Device == "" || len(discovery.Device) > 15 {
		return runtimeapi.NetworkPreview{}, errors.New("privileged Runtime network system returned an invalid TUN device")
	}
	capture := make(map[string]struct{}, len(settings.CaptureRouteIDs))
	for _, id := range settings.CaptureRouteIDs {
		capture[id] = struct{}{}
	}
	routes := append([]runtimeapi.NetworkRoute(nil), discovery.Routes...)
	discovered := make(map[string]struct{}, len(routes))
	for index := range routes {
		if !validOpaqueIdentifier(routes[index].ID, 3, 96) {
			return runtimeapi.NetworkPreview{}, errors.New("privileged Runtime network system returned an invalid route ID")
		}
		if _, duplicate := discovered[routes[index].ID]; duplicate {
			return runtimeapi.NetworkPreview{}, errors.New("privileged Runtime network system returned a duplicate route ID")
		}
		discovered[routes[index].ID] = struct{}{}
		_, captured := capture[routes[index].ID]
		routes[index].Bypass = !captured
	}
	for id := range capture {
		if _, ok := discovered[id]; !ok {
			return runtimeapi.NetworkPreview{}, errors.New("Runtime TUN route selection is stale or unknown")
		}
	}
	planID, err := randomIdentifier("plan_")
	if err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	now := m.now()
	preview := runtimeapi.NetworkPreview{
		PlanID:     planID,
		Mode:       runtimeapi.RunModeTUN,
		Device:     discovery.Device,
		Settings:   settings,
		Routes:     routes,
		Conflicts:  append([]runtimeapi.NetworkConflict(nil), discovery.Conflicts...),
		Warnings:   append([]string(nil), discovery.Warnings...),
		ExpiresAt:  now.Add(m.planTTL()),
		ObservedAt: now,
	}
	discovery.Routes = routes
	m.plans[planID] = planState{sessionID: sessionID, preview: preview, discovery: discovery}
	m.expirePlansLocked(now)
	return preview, nil
}

func (m *Manager) Prepare(
	ctx context.Context,
	meta RequestMeta,
	planID string,
) (PreparedTUN, error) {
	if err := contextError(ctx); err != nil {
		return PreparedTUN{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.validateMutationLocked(meta)
	if err != nil {
		return PreparedTUN{}, err
	}
	plan, ok := m.plans[planID]
	if !ok || !plan.preview.ExpiresAt.After(m.now()) {
		return PreparedTUN{}, errors.New("Runtime network plan is unavailable or expired")
	}
	if plan.sessionID != meta.SessionID {
		return PreparedTUN{}, errors.New("Runtime network plan belongs to another connection")
	}
	if len(plan.preview.Conflicts) > 0 {
		return PreparedTUN{}, errors.New("Runtime network plan contains a full-tunnel conflict")
	}
	if m.ownership != nil {
		return PreparedTUN{}, errors.New("Runtime network takeover is already owned")
	}
	ownershipID, err := randomIdentifier("net_")
	if err != nil {
		return PreparedTUN{}, err
	}
	token, err := randomHex(32)
	if err != nil {
		return PreparedTUN{}, err
	}
	now := m.now()
	ownership := Ownership{
		ID:                ownershipID,
		Token:             token,
		RuntimeInstanceID: session.RuntimeInstanceID,
		OperationID:       meta.OperationID,
		Mode:              runtimeapi.RunModeTUN,
		Device:            plan.preview.Device,
		IPv6Available:     plan.discovery.IPv6Available,
		Settings:          plan.preview.Settings,
		Routes:            append([]runtimeapi.NetworkRoute(nil), plan.preview.Routes...),
		Original:          cloneStringMap(plan.discovery.Original),
		State:             runtimeapi.NetworkStatePrepared,
		Epoch:             m.epoch,
		LeaseExpiresAt:    now.Add(m.leaseTTL()),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	preparation, err := m.System.PrepareTUN(ctx, ownership)
	if err != nil {
		ownership.Objects = append([]runtimeapi.NetworkObject(nil), preparation.Objects...)
		_, _ = m.System.Cleanup(context.Background(), ownership)
		return PreparedTUN{}, err
	}
	ownership.Objects = append([]runtimeapi.NetworkObject(nil), preparation.Objects...)
	ownership.RoutingMark = preparation.RoutingMark
	ownership.RouteTable = preparation.RouteTable
	ownership.RulePriority = preparation.RulePriority
	result := PreparedTUN{
		OwnershipID: ownership.ID,
		Device:      ownership.Device,
		RoutingMark: ownership.RoutingMark,
		Settings:    ownership.Settings,
	}
	outcome, err := m.newResult(session, meta, OperationPrepare, ownership.ID, result, nil)
	if err != nil {
		_, _ = m.System.Cleanup(context.Background(), ownership)
		return PreparedTUN{}, err
	}
	if err := m.persistLocked(&ownership, &outcome); err != nil {
		_, _ = m.System.Cleanup(context.Background(), ownership)
		return PreparedTUN{}, err
	}
	session.LastSequence = meta.Sequence
	delete(m.plans, planID)
	return result, nil
}

func (m *Manager) Commit(
	ctx context.Context,
	meta RequestMeta,
	ownershipID string,
) (runtimeapi.NetworkStatus, error) {
	if err := contextError(ctx); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ownership, err := m.mutableOwnershipLocked(meta, ownershipID)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	if ownership.State != runtimeapi.NetworkStatePrepared {
		return runtimeapi.NetworkStatus{}, errors.New("Runtime network ownership is not prepared")
	}
	objects, err := m.System.ApplyTUN(ctx, *ownership)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	ownership.Objects = mergeNetworkObjects(ownership.Objects, objects)
	ownership.State = runtimeapi.NetworkStateActive
	ownership.LeaseExpiresAt = m.now().Add(m.leaseTTL())
	ownership.UpdatedAt = m.now()
	status, observeErr := m.observeLocked(ctx)
	outcome, resultErr := m.newResult(session, meta, OperationCommit, ownership.ID, status, observeErr)
	if resultErr != nil {
		_, _ = m.System.Cleanup(context.Background(), *ownership)
		return runtimeapi.NetworkStatus{}, resultErr
	}
	if persistErr := m.persistLocked(ownership, &outcome); persistErr != nil {
		residuals, cleanupErr := m.System.Cleanup(context.Background(), *ownership)
		var recoveryPersistErr error
		if cleanupErr != nil || len(residuals) > 0 {
			ownership.State = runtimeapi.NetworkStateUnknown
			ownership.Residuals = append([]runtimeapi.NetworkObject(nil), residuals...)
			recoveryPersistErr = m.persistLocked(ownership, nil)
		} else {
			recoveryPersistErr = m.persistLocked(nil, nil)
			if recoveryPersistErr != nil {
				ownership.State = runtimeapi.NetworkStateUnknown
				m.ownership = ownership
			}
		}
		return runtimeapi.NetworkStatus{}, errors.Join(persistErr, cleanupErr, recoveryPersistErr)
	}
	session.LastSequence = meta.Sequence
	return status, observeErr
}

func (m *Manager) Renew(
	ctx context.Context,
	meta RequestMeta,
	ownershipID string,
) (runtimeapi.NetworkStatus, error) {
	if err := contextError(ctx); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ownership, err := m.mutableOwnershipLocked(meta, ownershipID)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	if ownership.State != runtimeapi.NetworkStateActive {
		return runtimeapi.NetworkStatus{}, errors.New("Runtime network ownership is not active")
	}
	ownership.LeaseExpiresAt = m.now().Add(m.leaseTTL())
	ownership.UpdatedAt = m.now()
	status, observeErr := m.observeLocked(ctx)
	outcome, resultErr := m.newResult(session, meta, OperationRenew, ownership.ID, status, observeErr)
	if resultErr != nil {
		return runtimeapi.NetworkStatus{}, resultErr
	}
	if persistErr := m.persistLocked(ownership, &outcome); persistErr != nil {
		return runtimeapi.NetworkStatus{}, persistErr
	}
	session.LastSequence = meta.Sequence
	return status, observeErr
}

func (m *Manager) Release(
	ctx context.Context,
	meta RequestMeta,
	ownershipID string,
	reason string,
) (runtimeapi.NetworkStatus, error) {
	if !validReleaseReason(reason) {
		return runtimeapi.NetworkStatus{}, errors.New("Runtime network release reason is invalid")
	}
	if err := contextError(ctx); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.validateMutationLocked(meta)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	if m.ownership == nil || m.ownership.ID != ownershipID {
		return runtimeapi.NetworkStatus{}, errors.New("Runtime network ownership does not match")
	}
	if m.ownership.RuntimeInstanceID != session.RuntimeInstanceID || m.ownership.Epoch != meta.Epoch {
		return runtimeapi.NetworkStatus{}, errors.New("Runtime network ownership identity does not match")
	}
	ownership := m.ownership
	status, err := m.releaseLocked(ctx, *ownership, reason)
	outcome, resultErr := m.newResult(session, meta, OperationRelease, ownership.ID, status, err)
	if resultErr != nil {
		return status, errors.Join(err, resultErr)
	}
	if persistErr := m.persistLocked(m.ownership, &outcome); persistErr != nil {
		return status, errors.Join(err, persistErr)
	}
	session.LastSequence = meta.Sequence
	return status, err
}

func (m *Manager) Result(
	sessionID string,
	operationID string,
	operation string,
) (CommittedResult, error) {
	if !validOpaqueIdentifier(operationID, 3, 96) || !validOperation(operation) {
		return CommittedResult{}, errors.New("Runtime network result query is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.sessionLocked(sessionID)
	if err != nil {
		return CommittedResult{}, err
	}
	for index := len(m.results) - 1; index >= 0; index-- {
		result := m.results[index]
		if result.RuntimeInstanceID == session.RuntimeInstanceID &&
			result.OperationID == operationID &&
			result.Operation == operation {
			return result, nil
		}
	}
	return CommittedResult{}, errors.New("Runtime network result was not found")
}

func (m *Manager) Observe(ctx context.Context, sessionID string) (runtimeapi.NetworkStatus, error) {
	if err := contextError(ctx); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.sessionLocked(sessionID); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	return m.observeLocked(ctx)
}

func (m *Manager) FailOpen(ctx context.Context, reason string) (runtimeapi.NetworkStatus, error) {
	if err := contextError(ctx); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	if !validReleaseReason(reason) {
		return runtimeapi.NetworkStatus{}, errors.New("Runtime network release reason is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ownership == nil {
		return m.observeLocked(ctx)
	}
	return m.releaseLocked(ctx, *m.ownership, reason)
}

func (m *Manager) Run(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.mu.Lock()
			if m.ownership != nil &&
				m.ownership.State == runtimeapi.NetworkStateActive &&
				!m.ownership.LeaseExpiresAt.After(m.now()) {
				_, err := m.releaseLocked(context.Background(), *m.ownership, ReleaseLeaseExpired)
				m.mu.Unlock()
				if err != nil {
					return err
				}
				continue
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) releaseLocked(
	ctx context.Context,
	ownership Ownership,
	_ string,
) (runtimeapi.NetworkStatus, error) {
	ownership.State = runtimeapi.NetworkStateReleasing
	ownership.UpdatedAt = m.now()
	if err := m.persistLocked(&ownership, nil); err != nil {
		status, observeErr := m.observeLocked(ctx)
		return status, errors.Join(err, observeErr)
	}
	residuals, cleanupErr := m.System.Cleanup(ctx, ownership)
	if cleanupErr != nil || len(residuals) > 0 {
		ownership.State = runtimeapi.NetworkStateUnknown
		ownership.Residuals = append([]runtimeapi.NetworkObject(nil), residuals...)
		ownership.UpdatedAt = m.now()
		writeErr := m.persistLocked(&ownership, nil)
		status, observeErr := m.observeLocked(ctx)
		return status, errors.Join(cleanupErr, writeErr, observeErr)
	}
	if err := m.persistLocked(nil, nil); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	return m.observeLocked(ctx)
}

func (m *Manager) mutableOwnershipLocked(
	meta RequestMeta,
	ownershipID string,
) (*sessionState, *Ownership, error) {
	session, err := m.validateMutationLocked(meta)
	if err != nil {
		return nil, nil, err
	}
	if m.ownership == nil || m.ownership.ID != ownershipID {
		return nil, nil, errors.New("Runtime network ownership does not match")
	}
	if m.ownership.RuntimeInstanceID != session.RuntimeInstanceID ||
		m.ownership.OperationID != meta.OperationID ||
		m.ownership.Epoch != meta.Epoch {
		return nil, nil, errors.New("Runtime network ownership identity does not match")
	}
	return session, m.ownership, nil
}

func (m *Manager) validateMutationLocked(meta RequestMeta) (*sessionState, error) {
	session, err := m.sessionLocked(meta.SessionID)
	if err != nil {
		return nil, err
	}
	if meta.Epoch != m.epoch {
		return nil, errors.New("Runtime network request used an old epoch")
	}
	if meta.ConnectionNonce != session.ServerNonce {
		return nil, errors.New("Runtime network connection nonce does not match")
	}
	now := m.now()
	if !meta.Deadline.After(now) || meta.Deadline.After(now.Add(DefaultRequestLimit)) {
		return nil, errors.New("Runtime network request deadline is invalid or expired")
	}
	if !validOpaqueIdentifier(meta.OperationID, 3, 96) {
		return nil, errors.New("Runtime network Operation ID is invalid")
	}
	if meta.Sequence == 0 || meta.Sequence <= session.LastSequence {
		return nil, errors.New("Runtime network request sequence was replayed")
	}
	return session, nil
}

func (m *Manager) sessionLocked(sessionID string) (*sessionState, error) {
	session := m.sessions[sessionID]
	if session == nil || !session.ExpiresAt.After(m.now()) {
		return nil, errors.New("Runtime network session is unavailable or expired")
	}
	return session, nil
}

func (m *Manager) observeLocked(ctx context.Context) (runtimeapi.NetworkStatus, error) {
	status, err := m.System.Observe(ctx, m.ownership)
	status.Available = true
	status.ObservedAt = m.now()
	if status.Mode == "" {
		status.Mode = runtimeapi.RunModeExplicit
	}
	if status.State == "" {
		if m.ownership != nil {
			status.State = m.ownership.State
		} else {
			status.State = runtimeapi.NetworkStateInactive
		}
	}
	if m.ownership != nil {
		status.Mode = m.ownership.Mode
		if m.ownership.State == runtimeapi.NetworkStateUnknown ||
			m.ownership.State == runtimeapi.NetworkStateReleasing ||
			m.ownership.State == runtimeapi.NetworkStateConflict {
			status.State = m.ownership.State
		}
		status.Device = m.ownership.Device
		status.Settings = m.ownership.Settings
		status.OwnershipID = m.ownership.ID
		if len(status.Objects) == 0 {
			status.Objects = append([]runtimeapi.NetworkObject(nil), m.ownership.Objects...)
		}
		status.Routes = append([]runtimeapi.NetworkRoute(nil), m.ownership.Routes...)
		status.Residuals = mergeNetworkObjects(status.Residuals, m.ownership.Residuals)
		expiresAt := m.ownership.LeaseExpiresAt
		status.LeaseExpiresAt = &expiresAt
	}
	return status, err
}

func normalizeTUNSettings(request runtimeapi.NetworkPreviewRequest) (runtimeapi.TUNSettings, error) {
	if request.Mode != "" && request.Mode != runtimeapi.RunModeTUN {
		return runtimeapi.TUNSettings{}, errors.New("Runtime network preview only supports ordinary TUN mode")
	}
	ipv6Policy := request.IPv6Policy
	if ipv6Policy == "" {
		ipv6Policy = runtimeapi.TUNIPv6Proxy
	}
	switch ipv6Policy {
	case runtimeapi.TUNIPv6Proxy, runtimeapi.TUNIPv6Direct, runtimeapi.TUNIPv6Block:
	default:
		return runtimeapi.TUNSettings{}, errors.New("Runtime TUN IPv6 policy is invalid")
	}
	dnsPolicy := request.DNSPolicy
	if dnsPolicy == "" {
		dnsPolicy = runtimeapi.TUNDNSHijack
	}
	if dnsPolicy != runtimeapi.TUNDNSHijack && dnsPolicy != runtimeapi.TUNDNSOff {
		return runtimeapi.TUNSettings{}, errors.New("Runtime TUN DNS policy is invalid")
	}
	capture := append([]string(nil), request.CaptureRouteIDs...)
	sort.Strings(capture)
	for index, id := range capture {
		if !validOpaqueIdentifier(id, 3, 96) {
			return runtimeapi.TUNSettings{}, errors.New("Runtime TUN route selection contains an invalid ID")
		}
		if index > 0 && capture[index-1] == id {
			return runtimeapi.TUNSettings{}, errors.New("Runtime TUN route selection contains a duplicate ID")
		}
	}
	return runtimeapi.TUNSettings{
		IPv6Policy:      ipv6Policy,
		DNSPolicy:       dnsPolicy,
		CaptureRouteIDs: capture,
	}, nil
}

func (m *Manager) expirePlansLocked(now time.Time) {
	for id, plan := range m.plans {
		if !plan.preview.ExpiresAt.After(now) {
			delete(m.plans, id)
		}
	}
}

func (m *Manager) now() time.Time {
	if m != nil && m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) planTTL() time.Duration {
	if m.PlanTTL > 0 {
		return m.PlanTTL
	}
	return DefaultPlanTTL
}

func (m *Manager) leaseTTL() time.Duration {
	if m.LeaseTTL > 0 {
		return m.LeaseTTL
	}
	return DefaultLeaseTTL
}

func randomIdentifier(prefix string) (string, error) {
	value, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return prefix + value, nil
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validOpaqueIdentifier(value string, minLength, maxLength int) bool {
	if len(value) < minLength || len(value) > maxLength {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

func validReleaseReason(value string) bool {
	switch value {
	case ReleaseOperator,
		ReleaseRuntimeExit,
		ReleaseMihomoFailure,
		ReleaseLeaseExpired,
		ReleaseServiceRestart,
		ReleaseUpdate:
		return true
	default:
		return false
	}
}

func validOperation(value string) bool {
	switch value {
	case OperationPrepare, OperationCommit, OperationRenew, OperationRelease:
		return true
	default:
		return false
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Runtime network context is required")
	}
	return ctx.Err()
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mergeNetworkObjects(groups ...[]runtimeapi.NetworkObject) []runtimeapi.NetworkObject {
	byID := make(map[string]runtimeapi.NetworkObject)
	for _, group := range groups {
		for _, object := range group {
			byID[object.ID] = object
		}
	}
	result := make([]runtimeapi.NetworkObject, 0, len(byID))
	for _, object := range byID {
		result = append(result, object)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})
	return result
}

func (m *Manager) newResult(
	session *sessionState,
	meta RequestMeta,
	operation string,
	ownershipID string,
	payload any,
	resultErr error,
) (CommittedResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return CommittedResult{}, err
	}
	result := CommittedResult{
		Epoch:             meta.Epoch,
		SessionID:         meta.SessionID,
		RuntimeInstanceID: session.RuntimeInstanceID,
		Sequence:          meta.Sequence,
		OperationID:       meta.OperationID,
		Operation:         operation,
		OwnershipID:       ownershipID,
		Payload:           body,
		CommittedAt:       m.now(),
	}
	if resultErr != nil {
		result.Error = resultErr.Error()
	}
	return result, nil
}

func (m *Manager) persistLocked(ownership *Ownership, result *CommittedResult) error {
	results := append([]CommittedResult(nil), m.results...)
	if result != nil {
		results = append(results, *result)
		if len(results) > 128 {
			results = append([]CommittedResult(nil), results[len(results)-128:]...)
		}
	}
	if err := writeState(m.Root, m.key, durableState{
		Ownership: ownership,
		Results:   results,
	}); err != nil {
		return err
	}
	m.results = results
	m.ownership = ownership
	return nil
}
