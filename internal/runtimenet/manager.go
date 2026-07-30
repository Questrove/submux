package runtimenet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeapi"
)

// gatewayPreviewOnly remains true until a physical Linux gateway acceptance run
// supplements the namespace and equivalent dual-interface coverage.
const gatewayPreviewOnly = true

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
	mode := request.Mode
	if mode == "" {
		mode = runtimeapi.RunModeTUN
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.sessionLocked(sessionID); err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	var (
		settings        runtimeapi.TUNSettings
		gatewaySettings *runtimeapi.GatewaySettings
		discovery       Discovery
		err             error
	)
	switch mode {
	case runtimeapi.RunModeTUN:
		settings, err = normalizeTUNSettings(request)
		if err == nil {
			discovery, err = m.System.Discover(ctx, settings)
		}
	case runtimeapi.RunModeGateway:
		var normalized runtimeapi.GatewaySettings
		normalized, err = normalizeGatewaySettings(request)
		if err == nil {
			discovery, err = m.System.DiscoverGateway(ctx, normalized)
			gatewaySettings = &normalized
		}
	default:
		err = errors.New("Runtime network preview mode is invalid")
	}
	if err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	if discovery.Device == "" || len(discovery.Device) > 15 {
		return runtimeapi.NetworkPreview{}, errors.New("privileged Runtime network system returned an invalid TUN device")
	}
	selections := settings.CaptureRouteIDs
	if gatewaySettings != nil {
		selections = gatewaySettings.ExcludedRouteIDs
	}
	selected := make(map[string]struct{}, len(selections))
	for _, id := range selections {
		selected[id] = struct{}{}
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
		_, chosen := selected[routes[index].ID]
		if mode == runtimeapi.RunModeTUN {
			routes[index].Bypass = !chosen
			continue
		}
		switch routes[index].Role {
		case runtimeapi.NetworkRouteRoleGatewayLAN:
			routes[index].Bypass = chosen
		case runtimeapi.NetworkRouteRoleGatewayWAN, runtimeapi.NetworkRouteRoleDirect:
			routes[index].Bypass = true
		default:
			return runtimeapi.NetworkPreview{}, errors.New("privileged Runtime network system returned an invalid gateway route role")
		}
	}
	for id := range selected {
		route, ok := discovered[id]
		if !ok {
			return runtimeapi.NetworkPreview{}, errors.New("Runtime network route selection is stale or unknown")
		}
		_ = route
		if mode == runtimeapi.RunModeGateway {
			for _, candidate := range routes {
				if candidate.ID == id && candidate.Role != runtimeapi.NetworkRouteRoleGatewayLAN {
					return runtimeapi.NetworkPreview{}, errors.New("Runtime gateway can only exclude discovered LAN routes")
				}
			}
		}
	}
	planID, err := randomIdentifier("plan_")
	if err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	now := m.now()
	preview := runtimeapi.NetworkPreview{
		PlanID:          planID,
		Mode:            mode,
		Device:          discovery.Device,
		Settings:        settings,
		GatewaySettings: gatewaySettings,
		Routes:          routes,
		Conflicts:       append([]runtimeapi.NetworkConflict(nil), discovery.Conflicts...),
		Warnings:        append([]string(nil), discovery.Warnings...),
		PreviewOnly:     discovery.PreviewOnly || mode == runtimeapi.RunModeGateway && gatewayPreviewOnly,
		ExpiresAt:       now.Add(m.planTTL()),
		ObservedAt:      now,
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
) (PreparedNetwork, error) {
	if err := contextError(ctx); err != nil {
		return PreparedNetwork{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.validateMutationLocked(meta)
	if err != nil {
		return PreparedNetwork{}, err
	}
	plan, ok := m.plans[planID]
	if !ok || !plan.preview.ExpiresAt.After(m.now()) {
		return PreparedNetwork{}, errors.New("Runtime network plan is unavailable or expired")
	}
	if plan.sessionID != meta.SessionID {
		return PreparedNetwork{}, errors.New("Runtime network plan belongs to another connection")
	}
	if len(plan.preview.Conflicts) > 0 {
		return PreparedNetwork{}, errors.New("Runtime network plan contains a full-tunnel conflict")
	}
	if m.ownership != nil {
		return PreparedNetwork{}, errors.New("Runtime network takeover is already owned")
	}
	ownershipID, err := randomIdentifier("net_")
	if err != nil {
		return PreparedNetwork{}, err
	}
	token, err := randomHex(32)
	if err != nil {
		return PreparedNetwork{}, err
	}
	now := m.now()
	ownership := Ownership{
		ID:                ownershipID,
		Token:             token,
		RuntimeInstanceID: session.RuntimeInstanceID,
		OperationID:       meta.OperationID,
		Mode:              plan.preview.Mode,
		Device:            plan.preview.Device,
		IPv6Available:     plan.discovery.IPv6Available,
		Settings:          plan.preview.Settings,
		GatewaySettings:   cloneGatewaySettings(plan.preview.GatewaySettings),
		Routes:            append([]runtimeapi.NetworkRoute(nil), plan.preview.Routes...),
		Original:          cloneStringMap(plan.discovery.Original),
		PreviewOnly:       plan.preview.PreviewOnly,
		State:             runtimeapi.NetworkStatePrepared,
		Epoch:             m.epoch,
		LeaseExpiresAt:    now.Add(m.leaseTTL()),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	var preparation SystemPreparation
	switch ownership.Mode {
	case runtimeapi.RunModeTUN:
		preparation, err = m.System.PrepareTUN(ctx, ownership)
	case runtimeapi.RunModeGateway:
		preparation, err = m.System.PrepareGateway(ctx, ownership)
	default:
		err = errors.New("Runtime network plan mode is invalid")
	}
	if err != nil {
		ownership.Objects = append([]runtimeapi.NetworkObject(nil), preparation.Objects...)
		_, _ = m.System.Cleanup(context.Background(), ownership)
		return PreparedNetwork{}, err
	}
	ownership.Objects = append([]runtimeapi.NetworkObject(nil), preparation.Objects...)
	ownership.RoutingMark = preparation.RoutingMark
	ownership.CaptureMark = preparation.CaptureMark
	ownership.RouteTable = preparation.RouteTable
	ownership.RulePriority = preparation.RulePriority
	result := PreparedNetwork{
		OwnershipID:     ownership.ID,
		Mode:            ownership.Mode,
		Device:          ownership.Device,
		RoutingMark:     ownership.RoutingMark,
		Settings:        ownership.Settings,
		GatewaySettings: cloneGatewaySettings(ownership.GatewaySettings),
	}
	outcome, err := m.newResult(session, meta, OperationPrepare, ownership.ID, result, nil)
	if err != nil {
		_, _ = m.System.Cleanup(context.Background(), ownership)
		return PreparedNetwork{}, err
	}
	if err := m.persistLocked(&ownership, &outcome); err != nil {
		_, _ = m.System.Cleanup(context.Background(), ownership)
		return PreparedNetwork{}, err
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
	var objects []runtimeapi.NetworkObject
	switch ownership.Mode {
	case runtimeapi.RunModeTUN:
		objects, err = m.System.ApplyTUN(ctx, *ownership)
	case runtimeapi.RunModeGateway:
		objects, err = m.System.ApplyGateway(ctx, *ownership)
	default:
		err = errors.New("Runtime network ownership mode is invalid")
	}
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
		status.GatewaySettings = cloneGatewaySettings(m.ownership.GatewaySettings)
		status.OwnershipID = m.ownership.ID
		if len(status.Objects) == 0 {
			status.Objects = append([]runtimeapi.NetworkObject(nil), m.ownership.Objects...)
		}
		status.Routes = append([]runtimeapi.NetworkRoute(nil), m.ownership.Routes...)
		status.Residuals = mergeNetworkObjects(status.Residuals, m.ownership.Residuals)
		expiresAt := m.ownership.LeaseExpiresAt
		status.LeaseExpiresAt = &expiresAt
	}
	status.PreviewOnly = m.ownership != nil && m.ownership.PreviewOnly ||
		status.Mode == runtimeapi.RunModeGateway && gatewayPreviewOnly
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

func normalizeGatewaySettings(request runtimeapi.NetworkPreviewRequest) (runtimeapi.GatewaySettings, error) {
	if request.Mode != runtimeapi.RunModeGateway {
		return runtimeapi.GatewaySettings{}, errors.New("Runtime gateway preview mode is required")
	}
	ipv6Policy := request.IPv6Policy
	if ipv6Policy == "" {
		ipv6Policy = runtimeapi.TUNIPv6Direct
	}
	if ipv6Policy != runtimeapi.TUNIPv6Direct && ipv6Policy != runtimeapi.TUNIPv6Block {
		return runtimeapi.GatewaySettings{}, errors.New("Runtime gateway IPv6 policy must be direct or block")
	}
	dnsPolicy := request.DNSPolicy
	if dnsPolicy == "" {
		dnsPolicy = runtimeapi.TUNDNSHijack
	}
	if dnsPolicy != runtimeapi.TUNDNSHijack && dnsPolicy != runtimeapi.TUNDNSOff {
		return runtimeapi.GatewaySettings{}, errors.New("Runtime gateway DNS policy is invalid")
	}
	captureTCP := true
	if request.CaptureTCP != nil {
		captureTCP = *request.CaptureTCP
	}
	captureUDP := true
	if request.CaptureUDP != nil {
		captureUDP = *request.CaptureUDP
	}
	excluded := append([]string(nil), request.ExcludedRouteIDs...)
	sort.Strings(excluded)
	for index, id := range excluded {
		if !validOpaqueIdentifier(id, 3, 96) {
			return runtimeapi.GatewaySettings{}, errors.New("Runtime gateway route exclusion contains an invalid ID")
		}
		if index > 0 && excluded[index-1] == id {
			return runtimeapi.GatewaySettings{}, errors.New("Runtime gateway route exclusion contains a duplicate ID")
		}
	}
	udpExceptions, err := normalizeGatewayExceptions(request.UDPExceptions, false)
	if err != nil {
		return runtimeapi.GatewaySettings{}, fmt.Errorf("Runtime gateway UDP exceptions: %w", err)
	}
	hostExceptions, err := normalizeGatewayExceptions(request.HostExceptions, true)
	if err != nil {
		return runtimeapi.GatewaySettings{}, fmt.Errorf("Runtime gateway host exceptions: %w", err)
	}
	dnsDirect := append([]string(nil), request.DNSDirectCIDRs...)
	if len(dnsDirect) > 128 {
		return runtimeapi.GatewaySettings{}, errors.New("Runtime gateway DNS direct list exceeds 128 entries")
	}
	for index, cidr := range dnsDirect {
		normalized, err := normalizeGatewayCIDR(cidr)
		if err != nil {
			return runtimeapi.GatewaySettings{}, fmt.Errorf("Runtime gateway DNS direct CIDR: %w", err)
		}
		dnsDirect[index] = normalized
	}
	sort.Strings(dnsDirect)
	for index := 1; index < len(dnsDirect); index++ {
		if dnsDirect[index-1] == dnsDirect[index] {
			return runtimeapi.GatewaySettings{}, errors.New("Runtime gateway DNS direct list contains a duplicate CIDR")
		}
	}
	return runtimeapi.GatewaySettings{
		IPv6Policy:       ipv6Policy,
		DNSPolicy:        dnsPolicy,
		CaptureTCP:       captureTCP,
		CaptureUDP:       captureUDP,
		ProxyHostTraffic: request.ProxyHostTraffic,
		ExcludedRouteIDs: excluded,
		UDPExceptions:    udpExceptions,
		DNSDirectCIDRs:   dnsDirect,
		HostExceptions:   hostExceptions,
	}, nil
}

func normalizeGatewayExceptions(
	source []runtimeapi.GatewayTrafficException,
	allowUID bool,
) ([]runtimeapi.GatewayTrafficException, error) {
	if len(source) > 128 {
		return nil, errors.New("list exceeds 128 entries")
	}
	result := make([]runtimeapi.GatewayTrafficException, len(source))
	for index, exception := range source {
		if exception.UID != nil && !allowUID {
			return nil, errors.New("UID is only valid for host traffic")
		}
		normalized := runtimeapi.GatewayTrafficException{}
		if exception.SourceCIDR != "" {
			cidr, err := normalizeGatewayCIDR(exception.SourceCIDR)
			if err != nil {
				return nil, fmt.Errorf("source CIDR: %w", err)
			}
			normalized.SourceCIDR = cidr
		}
		if exception.DestinationCIDR != "" {
			cidr, err := normalizeGatewayCIDR(exception.DestinationCIDR)
			if err != nil {
				return nil, fmt.Errorf("destination CIDR: %w", err)
			}
			normalized.DestinationCIDR = cidr
		}
		if exception.UID != nil {
			uid := *exception.UID
			normalized.UID = &uid
		}
		normalized.DestinationPorts = append(
			[]runtimeapi.NetworkPortRange(nil),
			exception.DestinationPorts...,
		)
		for portIndex := range normalized.DestinationPorts {
			portRange := &normalized.DestinationPorts[portIndex]
			if portRange.Start == 0 {
				return nil, errors.New("destination port must be between 1 and 65535")
			}
			if portRange.End == 0 {
				portRange.End = portRange.Start
			}
			if portRange.End < portRange.Start {
				return nil, errors.New("destination port range is reversed")
			}
		}
		sort.Slice(normalized.DestinationPorts, func(left, right int) bool {
			if normalized.DestinationPorts[left].Start != normalized.DestinationPorts[right].Start {
				return normalized.DestinationPorts[left].Start < normalized.DestinationPorts[right].Start
			}
			return normalized.DestinationPorts[left].End < normalized.DestinationPorts[right].End
		})
		for portIndex := 1; portIndex < len(normalized.DestinationPorts); portIndex++ {
			previous := normalized.DestinationPorts[portIndex-1]
			current := normalized.DestinationPorts[portIndex]
			if previous == current {
				return nil, errors.New("destination port list contains a duplicate range")
			}
		}
		if normalized.SourceCIDR == "" &&
			normalized.DestinationCIDR == "" &&
			len(normalized.DestinationPorts) == 0 &&
			normalized.UID == nil {
			return nil, errors.New("entry must contain at least one typed condition")
		}
		result[index] = normalized
	}
	sort.Slice(result, func(left, right int) bool {
		return gatewayExceptionKey(result[left]) < gatewayExceptionKey(result[right])
	})
	for index := 1; index < len(result); index++ {
		if gatewayExceptionKey(result[index-1]) == gatewayExceptionKey(result[index]) {
			return nil, errors.New("list contains a duplicate entry")
		}
	}
	return result, nil
}

func normalizeGatewayCIDR(value string) (string, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		if !prefix.Addr().Is4() {
			return "", errors.New("only IPv4 CIDRs are supported")
		}
		return prefix.Masked().String(), nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() {
		return "", errors.New("only IPv4 addresses or CIDRs are supported")
	}
	return netip.PrefixFrom(address, 32).String(), nil
}

func gatewayExceptionKey(value runtimeapi.GatewayTrafficException) string {
	uid := ""
	if value.UID != nil {
		uid = fmt.Sprintf("%d", *value.UID)
	}
	ports := make([]string, 0, len(value.DestinationPorts))
	for _, portRange := range value.DestinationPorts {
		ports = append(ports, fmt.Sprintf("%d-%d", portRange.Start, portRange.End))
	}
	return strings.Join([]string{
		value.SourceCIDR,
		value.DestinationCIDR,
		strings.Join(ports, ","),
		uid,
	}, "\x00")
}

func cloneGatewaySettings(source *runtimeapi.GatewaySettings) *runtimeapi.GatewaySettings {
	if source == nil {
		return nil
	}
	clone := *source
	clone.ExcludedRouteIDs = append([]string(nil), source.ExcludedRouteIDs...)
	clone.DNSDirectCIDRs = append([]string(nil), source.DNSDirectCIDRs...)
	clone.UDPExceptions = cloneGatewayExceptions(source.UDPExceptions)
	clone.HostExceptions = cloneGatewayExceptions(source.HostExceptions)
	return &clone
}

func cloneGatewayExceptions(
	source []runtimeapi.GatewayTrafficException,
) []runtimeapi.GatewayTrafficException {
	result := make([]runtimeapi.GatewayTrafficException, len(source))
	for index, exception := range source {
		result[index] = exception
		result[index].DestinationPorts = append(
			[]runtimeapi.NetworkPortRange(nil),
			exception.DestinationPorts...,
		)
		if exception.UID != nil {
			uid := *exception.UID
			result[index].UID = &uid
		}
	}
	return result
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
