package runtimenet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

type fakeSystem struct {
	mu         sync.Mutex
	discovery  Discovery
	prepared   int
	applied    int
	cleaned    int
	active     bool
	residuals  []runtimeapi.NetworkObject
	cleanupErr error
}

func (system *fakeSystem) Discover(context.Context, runtimeapi.TUNSettings) (Discovery, error) {
	system.mu.Lock()
	defer system.mu.Unlock()
	return system.discovery, nil
}

func (system *fakeSystem) PrepareTUN(context.Context, Ownership) (SystemPreparation, error) {
	system.mu.Lock()
	defer system.mu.Unlock()
	system.prepared++
	return SystemPreparation{
		Objects: []runtimeapi.NetworkObject{{
			Kind:  "tun",
			ID:    "tun:test",
			Name:  "smxtun0",
			State: runtimeapi.NetworkStatePrepared,
		}},
		RoutingMark:  20220,
		RouteTable:   20220,
		RulePriority: 12000,
	}, nil
}

func (system *fakeSystem) ApplyTUN(context.Context, Ownership) ([]runtimeapi.NetworkObject, error) {
	system.mu.Lock()
	defer system.mu.Unlock()
	system.applied++
	system.active = true
	return []runtimeapi.NetworkObject{{
		Kind:  "route",
		ID:    "route:test",
		Name:  "table-20220",
		State: runtimeapi.NetworkStateActive,
	}}, nil
}

func (system *fakeSystem) Cleanup(context.Context, Ownership) ([]runtimeapi.NetworkObject, error) {
	system.mu.Lock()
	defer system.mu.Unlock()
	system.cleaned++
	system.active = false
	return append([]runtimeapi.NetworkObject(nil), system.residuals...), system.cleanupErr
}

func (system *fakeSystem) Observe(context.Context, *Ownership) (runtimeapi.NetworkStatus, error) {
	system.mu.Lock()
	defer system.mu.Unlock()
	state := runtimeapi.NetworkStateInactive
	if system.active {
		state = runtimeapi.NetworkStateActive
	}
	return runtimeapi.NetworkStatus{
		Mode:  runtimeapi.RunModeExplicit,
		State: state,
	}, nil
}

func TestManagerPreviewPrepareCommitRenewReleaseAndReplayProtection(t *testing.T) {
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	system := &fakeSystem{discovery: Discovery{
		Device:        "smxtun0",
		IPv6Available: true,
		Routes: []runtimeapi.NetworkRoute{{
			ID:        "route_lan",
			Family:    "ipv4",
			CIDR:      "192.168.1.0/24",
			Interface: "eth0",
			Table:     "main",
			Source:    "kernel",
		}},
	}}
	manager, err := OpenManager(filepath.Join(t.TempDir(), "network"), 1001, system)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	manager.Now = func() time.Time { return now }
	session := openTestSession(t, manager, 1001)

	preview, err := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{
		Mode: runtimeapi.RunModeTUN,
	})
	if err != nil {
		t.Fatalf("preview Runtime TUN: %v", err)
	}
	if preview.Settings.IPv6Policy != runtimeapi.TUNIPv6Proxy ||
		preview.Settings.DNSPolicy != runtimeapi.TUNDNSHijack ||
		len(preview.Routes) != 1 || !preview.Routes[0].Bypass {
		t.Fatalf("Runtime TUN preview=%#v", preview)
	}

	operationID := "op_0123456789abcdef0123456789abcdef"
	prepared, err := manager.Prepare(t.Context(), requestMeta(session, operationID, 1, now), preview.PlanID)
	if err != nil {
		t.Fatalf("prepare Runtime TUN: %v", err)
	}
	if prepared.Device != "smxtun0" || prepared.OwnershipID == "" {
		t.Fatalf("prepared Runtime TUN=%#v", prepared)
	}
	status, err := manager.Commit(
		t.Context(),
		requestMeta(session, operationID, 2, now),
		prepared.OwnershipID,
	)
	if err != nil || status.State != runtimeapi.NetworkStateActive {
		t.Fatalf("commit Runtime TUN status=%#v err=%v", status, err)
	}
	now = now.Add(time.Second)
	if _, err := manager.Renew(
		t.Context(),
		requestMeta(session, operationID, 3, now),
		prepared.OwnershipID,
	); err != nil {
		t.Fatalf("renew Runtime TUN lease: %v", err)
	}
	if _, err := manager.Renew(
		t.Context(),
		requestMeta(session, operationID, 3, now),
		prepared.OwnershipID,
	); err == nil || !strings.Contains(err.Error(), "replayed") {
		t.Fatalf("replayed Runtime TUN lease error=%v", err)
	}
	status, err = manager.Release(
		t.Context(),
		requestMeta(session, "op_disable_0123456789abcdef", 4, now),
		prepared.OwnershipID,
		ReleaseOperator,
	)
	if err != nil || status.State != runtimeapi.NetworkStateInactive || system.cleaned != 1 {
		t.Fatalf("release Runtime TUN status=%#v cleaned=%d err=%v", status, system.cleaned, err)
	}
	reconnected := openTestSession(t, manager, 1001)
	result, err := manager.Result(reconnected.ID, operationID, OperationCommit)
	if err != nil || result.Sequence != 2 || result.OwnershipID != prepared.OwnershipID {
		t.Fatalf("query committed Runtime network result=%#v err=%v", result, err)
	}
}

func TestManagerRejectsUnauthorizedPeerAndConflictingPlan(t *testing.T) {
	system := &fakeSystem{discovery: Discovery{
		Device: "smxtun0",
		Conflicts: []runtimeapi.NetworkConflict{{
			Kind:   "full_tunnel",
			Owner:  "wg0",
			Detail: "another policy table owns a default route",
		}},
	}}
	manager, err := OpenManager(filepath.Join(t.TempDir(), "network"), 1001, system)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	if _, err := manager.OpenSession(1002, testSessionRequest()); err == nil {
		t.Fatal("privileged Runtime network manager accepted an unauthorized UID")
	}
	if _, err := manager.OpenSession(0, testSessionRequest()); err == nil {
		t.Fatal("privileged Runtime network manager accepted root instead of the Runtime service account")
	}
	session := openTestSession(t, manager, 1001)
	preview, err := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{})
	if err != nil {
		t.Fatalf("preview conflicting Runtime TUN: %v", err)
	}
	if _, err := manager.Prepare(
		t.Context(),
		requestMeta(session, "op_conflict", 1, time.Now().UTC()),
		preview.PlanID,
	); err == nil {
		t.Fatal("privileged Runtime network manager prepared a conflicting full tunnel")
	}
}

func TestManagerRejectsUnknownCapturedRouteAndSuppressesAbsentIPv6Warning(t *testing.T) {
	system := &fakeSystem{discovery: Discovery{
		Device: "smxtun0",
		Routes: []runtimeapi.NetworkRoute{{
			ID:        "route_lan",
			Family:    "ipv4",
			CIDR:      "192.168.1.0/24",
			Interface: "eth0",
		}},
	}}
	manager, err := OpenManager(filepath.Join(t.TempDir(), "network"), 1001, system)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	session := openTestSession(t, manager, 1001)
	if _, err := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{
		IPv6Policy:      runtimeapi.TUNIPv6Direct,
		CaptureRouteIDs: []string{"route_missing"},
	}); err == nil || !strings.Contains(err.Error(), "stale or unknown") {
		t.Fatalf("unknown captured route error=%v", err)
	}
	preview, err := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{
		IPv6Policy: runtimeapi.TUNIPv6Direct,
	})
	if err != nil {
		t.Fatalf("preview Runtime TUN without IPv6: %v", err)
	}
	if len(preview.Warnings) != 0 {
		t.Fatalf("absent IPv6 should not produce a meaningless warning: %#v", preview.Warnings)
	}
}

func TestManagerFailOpenCleansActiveOwnershipWithoutClientSession(t *testing.T) {
	system := &fakeSystem{discovery: Discovery{Device: "smxtun0"}}
	manager, err := OpenManager(filepath.Join(t.TempDir(), "network"), 1001, system)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	now := time.Now().UTC()
	manager.Now = func() time.Time { return now }
	session := openTestSession(t, manager, 1001)
	preview, err := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{})
	if err != nil {
		t.Fatalf("preview Runtime TUN: %v", err)
	}
	prepared, err := manager.Prepare(
		t.Context(),
		requestMeta(session, "op_failopen", 1, now),
		preview.PlanID,
	)
	if err != nil {
		t.Fatalf("prepare Runtime TUN: %v", err)
	}
	if _, err := manager.Commit(
		t.Context(),
		requestMeta(session, "op_failopen", 2, now),
		prepared.OwnershipID,
	); err != nil {
		t.Fatalf("commit Runtime TUN: %v", err)
	}
	status, err := manager.FailOpen(t.Context(), ReleaseServiceRestart)
	if err != nil || status.State != runtimeapi.NetworkStateInactive || system.cleaned != 1 {
		t.Fatalf("fail-open status=%#v cleaned=%d err=%v", status, system.cleaned, err)
	}
}

func TestManagerRecoversStaleOwnershipAndRejectsTampering(t *testing.T) {
	root := filepath.Join(t.TempDir(), "network")
	system := &fakeSystem{discovery: Discovery{Device: "smxtun0"}}
	manager, err := OpenManager(root, 1001, system)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	now := time.Now().UTC()
	manager.Now = func() time.Time { return now }
	session := openTestSession(t, manager, 1001)
	preview, err := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{})
	if err != nil {
		t.Fatalf("preview Runtime TUN: %v", err)
	}
	prepared, err := manager.Prepare(
		t.Context(),
		requestMeta(session, "op_recover", 1, now),
		preview.PlanID,
	)
	if err != nil {
		t.Fatalf("prepare Runtime TUN: %v", err)
	}
	if prepared.OwnershipID == "" {
		t.Fatal("prepared Runtime TUN has no ownership ID")
	}

	restarted, err := OpenManager(root, 1001, system)
	if err != nil {
		t.Fatalf("reopen privileged Runtime network manager: %v", err)
	}
	status, err := restarted.Recover(t.Context())
	if err != nil || status.State != runtimeapi.NetworkStateInactive || system.cleaned != 1 {
		t.Fatalf("recover stale Runtime TUN status=%#v cleaned=%d err=%v", status, system.cleaned, err)
	}

	manager, err = OpenManager(root, 1001, system)
	if err != nil {
		t.Fatalf("reopen clean privileged Runtime network manager: %v", err)
	}
	manager.Now = func() time.Time { return now }
	session = openTestSession(t, manager, 1001)
	preview, _ = manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{})
	if _, err := manager.Prepare(
		t.Context(),
		requestMeta(session, "op_tamper", 1, now),
		preview.PlanID,
	); err != nil {
		t.Fatalf("prepare Runtime TUN for tamper test: %v", err)
	}
	path := filepath.Join(root, ownershipFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Runtime network ownership: %v", err)
	}
	body[len(body)/2] ^= 1
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatalf("tamper Runtime network ownership: %v", err)
	}
	if _, err := OpenManager(root, 1001, system); err == nil {
		t.Fatal("privileged Runtime network manager accepted a tampered ownership record")
	}
}

func TestManagerReportsResidualsDuringFailOpen(t *testing.T) {
	system := &fakeSystem{
		discovery: Discovery{Device: "smxtun0"},
		residuals: []runtimeapi.NetworkObject{{
			Kind:  "route",
			ID:    "route:residual",
			Name:  "default-v4",
			State: runtimeapi.NetworkStateUnknown,
		}},
		cleanupErr: errors.New("route removal failed"),
	}
	manager, err := OpenManager(filepath.Join(t.TempDir(), "network"), 1001, system)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	now := time.Now().UTC()
	manager.Now = func() time.Time { return now }
	session := openTestSession(t, manager, 1001)
	preview, _ := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{})
	prepared, err := manager.Prepare(
		t.Context(),
		requestMeta(session, "op_residual", 1, now),
		preview.PlanID,
	)
	if err != nil {
		t.Fatalf("prepare Runtime TUN: %v", err)
	}
	status, err := manager.Release(
		t.Context(),
		requestMeta(session, "op_residual", 2, now),
		prepared.OwnershipID,
		ReleaseMihomoFailure,
	)
	if err == nil || status.State != runtimeapi.NetworkStateUnknown || len(status.Residuals) != 1 {
		t.Fatalf("residual Runtime TUN status=%#v err=%v", status, err)
	}
}

func openTestSession(t *testing.T, manager *Manager, peerUID uint32) Session {
	t.Helper()
	session, err := manager.OpenSession(peerUID, testSessionRequest())
	if err != nil {
		t.Fatalf("open privileged Runtime network session: %v", err)
	}
	return session
}

func testSessionRequest() SessionRequest {
	return SessionRequest{
		ProtocolVersion:   ProtocolVersion,
		RuntimeInstanceID: "installation_0123456789abcdef0123456789abcdef",
		ClientNonce:       strings.Repeat("a", 64),
	}
}

func requestMeta(session Session, operationID string, sequence uint64, now time.Time) RequestMeta {
	return RequestMeta{
		Epoch:           session.Epoch,
		SessionID:       session.ID,
		ConnectionNonce: session.ServerNonce,
		Sequence:        sequence,
		OperationID:     operationID,
		Deadline:        now.Add(10 * time.Second),
	}
}
