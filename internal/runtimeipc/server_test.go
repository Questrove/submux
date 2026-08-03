package runtimeipc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

type observerFunc func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error)

func (function observerFunc) Observe(ctx context.Context, peer runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	return function(ctx, peer)
}

type operatorObserver struct {
	observerFunc
}

type privacyOperator struct {
	operatorObserver
	reveal  func(runtimeapi.PeerIdentity, string, string, string, runtimeapi.RevealSourceURLRequest) (runtimeapi.RevealSourceURLResponse, error)
	preview func(runtimeapi.PeerIdentity, string, string, string, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsPreview, error)
	create  func(runtimeapi.PeerIdentity, string, string, string, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsResult, error)
}

type eventObserver struct {
	observerFunc
	events func(context.Context, runtimeapi.PeerIdentity, uint64, int) ([]runtimeapi.Event, uint64, error)
}

type networkObserver struct {
	observerFunc
	preview func(context.Context, runtimeapi.PeerIdentity, runtimeapi.NetworkPreviewRequest) (runtimeapi.NetworkPreview, error)
}

type trafficObserver struct {
	observerFunc
	history func(context.Context, runtimeapi.PeerIdentity, runtimeapi.TrafficHistoryRequest) (runtimeapi.TrafficHistory, error)
}

func (observer trafficObserver) TrafficHistory(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.TrafficHistoryRequest,
) (runtimeapi.TrafficHistory, error) {
	return observer.history(ctx, peer, request)
}

type updateObserver struct {
	operatorObserver
	upload  func(runtimeapi.PeerIdentity, int64, string, io.Reader) (runtimeapi.MihomoUpdateBundle, error)
	preview func(runtimeapi.PeerIdentity, runtimeapi.MihomoUpdatePreviewRequest) (runtimeapi.MihomoUpdatePlan, error)
}

type productUpdateObserver struct {
	operatorObserver
	preview func(runtimeapi.PeerIdentity, string, string, string, runtimeapi.ProductUpdatePreviewRequest) (runtimeapi.ProductUpdatePlan, error)
}

type backupObserver struct {
	operatorObserver
	preview        func(runtimeapi.PeerIdentity, runtimeapi.BackupPreviewRequest) (runtimeapi.BackupPreview, error)
	export         func(runtimeapi.PeerIdentity, runtimeapi.BackupExportRequest) (runtimeapi.BackupArchive, error)
	restorePreview func(runtimeapi.PeerIdentity, runtimeapi.BackupRestorePreviewRequest) (runtimeapi.BackupRestorePreview, error)
}

func (observer backupObserver) PreviewBackup(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	_, _, _ string,
	request runtimeapi.BackupPreviewRequest,
) (runtimeapi.BackupPreview, error) {
	return observer.preview(peer, request)
}

func (observer backupObserver) ExportBackup(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	_, _, _ string,
	request runtimeapi.BackupExportRequest,
) (runtimeapi.BackupArchive, error) {
	return observer.export(peer, request)
}

func (observer backupObserver) PreviewBackupRestore(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	_, _, _ string,
	request runtimeapi.BackupRestorePreviewRequest,
) (runtimeapi.BackupRestorePreview, error) {
	return observer.restorePreview(peer, request)
}

func (observer updateObserver) UploadMihomoUpdateBundle(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	size int64,
	digest string,
	body io.Reader,
) (runtimeapi.MihomoUpdateBundle, error) {
	return observer.upload(peer, size, digest, body)
}

func (observer updateObserver) PreviewMihomoUpdate(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.MihomoUpdatePreviewRequest,
) (runtimeapi.MihomoUpdatePlan, error) {
	return observer.preview(peer, request)
}

func (observer productUpdateObserver) PreviewProductUpdate(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.ProductUpdatePreviewRequest,
) (runtimeapi.ProductUpdatePlan, error) {
	return observer.preview(peer, clientType, clientVersion, requestID, request)
}

func (observer networkObserver) PreviewNetwork(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.NetworkPreviewRequest,
) (runtimeapi.NetworkPreview, error) {
	return observer.preview(ctx, peer, request)
}

func (observer eventObserver) Events(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	after uint64,
	limit int,
) ([]runtimeapi.Event, uint64, error) {
	return observer.events(ctx, peer, after, limit)
}

func (operatorObserver) UploadImport(context.Context, runtimeapi.PeerIdentity, string, int64, string, []byte) (runtimeapi.ImportContent, error) {
	return runtimeapi.ImportContent{}, nil
}

func (operatorObserver) GetAdvancedOverride(
	context.Context,
	runtimeapi.PeerIdentity,
	string,
	string,
	string,
	bool,
) (runtimeapi.AdvancedOverrideDocument, error) {
	return runtimeapi.AdvancedOverrideDocument{YAML: "{}\n"}, nil
}

func (operatorObserver) PreviewCandidate(
	context.Context,
	runtimeapi.PeerIdentity,
	runtimeapi.PreviewCandidateRequest,
) (runtimeapi.CandidatePreview, error) {
	return runtimeapi.CandidatePreview{Validated: true}, nil
}

func (operatorObserver) RuntimeVersion() string {
	return "test"
}

func (operatorObserver) Execute(context.Context, runtimeapi.PeerIdentity, string, string, runtimeapi.CreateOperationRequest) (runtimeapi.Operation, bool, error) {
	return runtimeapi.Operation{}, false, nil
}

func (operatorObserver) GetOperation(context.Context, string) (runtimeapi.Operation, error) {
	return runtimeapi.Operation{}, nil
}

func (operatorObserver) CancelOperation(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.CancelOperationRequest) (runtimeapi.Operation, bool, error) {
	return runtimeapi.Operation{}, false, nil
}

func (operatorObserver) VerifyProxy(context.Context) (runtimeapi.ProxyVerification, error) {
	return runtimeapi.ProxyVerification{}, nil
}

func (operatorObserver) RevealSourceURL(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.RevealSourceURLRequest) (runtimeapi.RevealSourceURLResponse, error) {
	return runtimeapi.RevealSourceURLResponse{}, nil
}

func (operatorObserver) PreviewDiagnostics(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsPreview, error) {
	return runtimeapi.DiagnosticsPreview{}, nil
}

func (operatorObserver) CreateDiagnostics(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsResult, error) {
	return runtimeapi.DiagnosticsResult{}, nil
}

func (operator privacyOperator) RevealSourceURL(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.RevealSourceURLRequest,
) (runtimeapi.RevealSourceURLResponse, error) {
	return operator.reveal(peer, clientType, clientVersion, requestID, request)
}

func (operator privacyOperator) PreviewDiagnostics(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsPreview, error) {
	return operator.preview(peer, clientType, clientVersion, requestID, request)
}

func (operator privacyOperator) CreateDiagnostics(
	_ context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsResult, error) {
	return operator.create(peer, clientType, clientVersion, requestID, request)
}

func TestSnapshotHandlerValidatesProtocolAndPeer(t *testing.T) {
	server, err := NewServer(
		observerFunc(func(_ context.Context, peer runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{
				ProtocolVersion: runtimeapi.ProtocolVersion,
				Revision:        7,
				Runtime: runtimeapi.RuntimeStatus{
					Version:      "v1.0.0",
					ServiceState: "running",
				},
				LatestEventCursor: 9,
				ObservedAt:        time.Unix(1, 0).UTC(),
			}, nil
		}),
		AuthorizeFunc(func(peer runtimeapi.PeerIdentity) error {
			if peer.UID != 1000 {
				return errors.New("not authorized")
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/snapshot", nil)
	request.Header.Set(HeaderRequestID, "request-1")
	request.Header.Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	request.Header.Set(HeaderClientVersion, "test")
	request.Header.Set(HeaderClientType, "test")
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var snapshot runtimeapi.Snapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.Revision != 7 || snapshot.LatestEventCursor != 9 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", recorder.Header().Get("Cache-Control"))
	}
}

func TestTrafficHistoryHandlerUsesBoundedTimeOrCursorQuery(t *testing.T) {
	var received runtimeapi.TrafficHistoryRequest
	service := trafficObserver{
		observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{}, nil
		},
		history: func(_ context.Context, peer runtimeapi.PeerIdentity, request runtimeapi.TrafficHistoryRequest) (runtimeapi.TrafficHistory, error) {
			if peer.UID != 1000 {
				t.Fatalf("peer=%#v", peer)
			}
			received = request
			return runtimeapi.TrafficHistory{
				Status:       runtimeapi.TrafficStatus{Available: true, UploadTotal: 12},
				Samples:      []runtimeapi.TrafficSample{{Cursor: 8, UploadTotal: 12}},
				LatestCursor: 8,
			}, nil
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	serve := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set(HeaderRequestID, "request-traffic")
		request.Header.Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
		request.Header.Set(HeaderClientType, "cli")
		request.Header.Set(HeaderClientVersion, "test")
		request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}

	since := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := serve("/v1/traffic/history?since=" + since.Format(time.RFC3339Nano) + "&limit=900")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" ||
		!received.Since.Equal(since) || received.Limit != 900 ||
		!strings.Contains(recorder.Body.String(), `"latest_cursor":8`) {
		t.Fatalf("traffic history status=%d request=%#v body=%s", recorder.Code, received, recorder.Body.String())
	}
	recorder = serve("/v1/traffic/history?after=8&since=" + since.Format(time.RFC3339Nano))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mixed traffic range status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = serve("/v1/traffic/history?limit=1025")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unbounded traffic limit status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestEventHandlerStreamsMonotonicNDJSON(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	service := eventObserver{
		observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{}, nil
		},
		events: func(_ context.Context, peer runtimeapi.PeerIdentity, after uint64, limit int) ([]runtimeapi.Event, uint64, error) {
			if peer.UID != 1000 || limit <= 0 {
				t.Fatalf("event request peer=%#v limit=%d", peer, limit)
			}
			calls++
			if calls == 1 {
				if after != 0 {
					t.Fatalf("initial event cursor=%d", after)
				}
				return []runtimeapi.Event{{
					Cursor:           1,
					Type:             "operation.queued",
					At:               time.Unix(1, 0).UTC(),
					OperationID:      "op_test",
					SnapshotRevision: 2,
				}}, 1, nil
			}
			cancel()
			return nil, 1, nil
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/events?after=0", nil).WithContext(ctx)
	request.Header.Set(HeaderRequestID, "request-events")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientVersion, "test")
	request.Header.Set(HeaderClientType, "test")
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("Content-Type=%q", recorder.Header().Get("Content-Type"))
	}
	var event runtimeapi.Event
	if err := json.NewDecoder(recorder.Body).Decode(&event); err != nil {
		t.Fatalf("decode Runtime event: %v", err)
	}
	if event.Cursor != 1 || event.OperationID != "op_test" || calls < 2 {
		t.Fatalf("streamed event=%#v calls=%d", event, calls)
	}
}

func TestEventHandlerReturnsCursorExpiredWithEarliestCursor(t *testing.T) {
	service := eventObserver{
		observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{}, nil
		},
		events: func(context.Context, runtimeapi.PeerIdentity, uint64, int) ([]runtimeapi.Event, uint64, error) {
			return nil, 42, &runtimestate.CursorExpiredError{Earliest: 42}
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/events?after=1", nil)
	request.Header.Set(HeaderRequestID, "request-events")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientVersion, "test")
	request.Header.Set(HeaderClientType, "test")
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope runtimeapi.ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode Runtime event error: %v", err)
	}
	if envelope.Error.Code != runtimeapi.ErrorCursorExpired || envelope.EarliestCursor != 42 {
		t.Fatalf("event error=%#v", envelope)
	}
}

func TestSnapshotHandlerErrorsAreStable(t *testing.T) {
	server, err := NewServer(
		observerFunc(func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{}, nil
		}),
		AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }),
	)
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(*http.Request)
		peer    bool
		status  int
		errCode string
	}{
		{
			name:    "missing request ID",
			mutate:  func(*http.Request) {},
			peer:    true,
			status:  http.StatusBadRequest,
			errCode: runtimeapi.ErrorInvalidRequest,
		},
		{
			name: "unsupported protocol",
			mutate: func(request *http.Request) {
				request.Header.Set(HeaderRequestID, "request-1")
				request.Header.Set(HeaderProtocolVersion, "999")
				request.Header.Set(HeaderClientVersion, "test")
				request.Header.Set(HeaderClientType, "test")
			},
			peer:    true,
			status:  http.StatusUpgradeRequired,
			errCode: runtimeapi.ErrorProtocolUnsupported,
		},
		{
			name: "unverified peer",
			mutate: func(request *http.Request) {
				request.Header.Set(HeaderRequestID, "request-1")
				request.Header.Set(HeaderProtocolVersion, "1")
				request.Header.Set(HeaderClientVersion, "test")
				request.Header.Set(HeaderClientType, "test")
			},
			status:  http.StatusForbidden,
			errCode: runtimeapi.ErrorUnauthorized,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/snapshot", nil)
			test.mutate(request)
			if test.peer {
				request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}, nil)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.status, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), `"code":"`+test.errCode+`"`) {
				t.Fatalf("body = %s, want error code %q", recorder.Body.String(), test.errCode)
			}
		})
	}
}

func TestOperationHandlerRejectsUnknownAndDuplicateJSONFields(t *testing.T) {
	service := operatorObserver{observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
		return runtimeapi.Snapshot{}, nil
	}}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	tests := []string{
		`{"request_id":"request-1","request_id":"request-2","if_revision":1,"action":{"kind":"proxy.stop","params":{}}}`,
		`{"request_id":"request-1","if_revision":1,"action":{"kind":"proxy.stop","params":{"unknown":true}}}`,
	}
	for _, body := range tests {
		request := httptest.NewRequest(http.MethodPost, "/v1/operations", bytes.NewBufferString(body))
		request.Header.Set(HeaderRequestID, "request-1")
		request.Header.Set(HeaderProtocolVersion, "1")
		request.Header.Set(HeaderClientVersion, "test")
		request.Header.Set(HeaderClientType, "test")
		request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("body %s: response=%s", body, recorder.Body.String())
		}
	}
}

func TestMutationRejectsIncompatibleClientVersion(t *testing.T) {
	service := operatorObserver{observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
		return runtimeapi.Snapshot{}, nil
	}}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	body := `{"request_id":"request-1","if_revision":1,"action":{"kind":"proxy.stop","params":{}}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/operations", bytes.NewBufferString(body))
	request.Header.Set(HeaderRequestID, "request-1")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientVersion, "old")
	request.Header.Set(HeaderClientType, "test")
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"protocol_unsupported"`) {
		t.Fatalf("response=%s", recorder.Body.String())
	}
}

func TestCandidatePreviewUsesReadOnlyIPCEndpoint(t *testing.T) {
	service := operatorObserver{observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
		return runtimeapi.Snapshot{}, nil
	}}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/candidates/preview",
		bytes.NewBufferString(`{"content_id":"content_0123456789abcdef0123456789abcdef"}`),
	)
	request.Header.Set(HeaderRequestID, "request-1")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientVersion, "old")
	request.Header.Set(HeaderClientType, "test")
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"validated":true`) {
		t.Fatalf("response=%s", recorder.Body.String())
	}

	sourceRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/candidates/preview",
		bytes.NewBufferString(`{"source_id":"src_0123456789abcdef0123456789abcdef"}`),
	)
	sourceRequest.Header = request.Header.Clone()
	sourceRequest = withPeerContext(sourceRequest, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	sourceRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(sourceRecorder, sourceRequest)
	if sourceRecorder.Code != http.StatusOK {
		t.Fatalf("source preview status=%d body=%s", sourceRecorder.Code, sourceRecorder.Body.String())
	}

	bothRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/candidates/preview",
		bytes.NewBufferString(`{"content_id":"content_0123456789abcdef0123456789abcdef","source_id":"src_0123456789abcdef0123456789abcdef"}`),
	)
	bothRequest.Header = request.Header.Clone()
	bothRequest = withPeerContext(bothRequest, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	bothRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(bothRecorder, bothRequest)
	if bothRecorder.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous preview status=%d body=%s", bothRecorder.Code, bothRecorder.Body.String())
	}
}

func TestNetworkPreviewUsesTypedReadOnlyIPCEndpoint(t *testing.T) {
	called := 0
	service := networkObserver{
		observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{}, nil
		},
		preview: func(
			_ context.Context,
			peer runtimeapi.PeerIdentity,
			request runtimeapi.NetworkPreviewRequest,
		) (runtimeapi.NetworkPreview, error) {
			called++
			if peer.UID != 1000 ||
				request.Mode != runtimeapi.RunModeTUN ||
				request.IPv6Policy != runtimeapi.TUNIPv6Direct ||
				request.DNSPolicy != runtimeapi.TUNDNSOff ||
				len(request.CaptureRouteIDs) != 1 ||
				request.CaptureRouteIDs[0] != "route_lan" {
				t.Fatalf("network preview peer=%#v request=%#v", peer, request)
			}
			return runtimeapi.NetworkPreview{PlanID: "plan_0123456789abcdef0123456789abcdef"}, nil
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	serve := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/network/preview",
			bytes.NewBufferString(body),
		)
		request.Header.Set(HeaderRequestID, "request-network")
		request.Header.Set(HeaderProtocolVersion, "1")
		request.Header.Set(HeaderClientVersion, "old")
		request.Header.Set(HeaderClientType, "test")
		request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	recorder := serve(`{"mode":"tun","ipv6_policy":"direct","dns_policy":"off","capture_route_ids":["route_lan"]}`)
	if recorder.Code != http.StatusOK || called != 1 ||
		!strings.Contains(recorder.Body.String(), "plan_0123456789abcdef0123456789abcdef") {
		t.Fatalf("network preview status=%d calls=%d body=%s", recorder.Code, called, recorder.Body.String())
	}
	for _, body := range []string{
		`{"mode":"tun","url":"https://example.invalid"}`,
		`{"mode":"tun","command":"ip"}`,
		`{"mode":"tun","argv":["route"]}`,
		`{"mode":"tun","firewall_fragment":"drop"}`,
	} {
		recorder = serve(body)
		if recorder.Code != http.StatusBadRequest || called != 1 {
			t.Fatalf("untyped network body=%s status=%d calls=%d response=%s", body, recorder.Code, called, recorder.Body.String())
		}
	}
}

func TestAdvancedOverrideUsesAuthenticatedReadOnlyEndpoint(t *testing.T) {
	service := operatorObserver{observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
		return runtimeapi.Snapshot{}, nil
	}}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/advanced-override", nil)
	request.Header.Set(HeaderRequestID, "request-1")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientVersion, "old")
	request.Header.Set(HeaderClientType, "test")
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/advanced-override?reveal=1", nil)
	request.Header.Set(HeaderRequestID, "request-2")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientVersion, "old")
	request.Header.Set(HeaderClientType, "test")
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"yaml":"{}\n"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", recorder.Header().Get("Cache-Control"))
	}
}

func TestSensitiveEndpointsRequireConfirmationAndCarryClientIdentity(t *testing.T) {
	peer := runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-21-test"}
	revealCalls := 0
	previewCalls := 0
	createCalls := 0
	service := privacyOperator{
		operatorObserver: operatorObserver{observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{}, nil
		}},
		reveal: func(gotPeer runtimeapi.PeerIdentity, clientType, clientVersion, requestID string, request runtimeapi.RevealSourceURLRequest) (runtimeapi.RevealSourceURLResponse, error) {
			revealCalls++
			if gotPeer.Key() != peer.Key() || clientType != "gui" || clientVersion != "test" || requestID != "request-sensitive" || !request.Confirm {
				t.Fatalf("source reveal metadata peer=%#v type=%q version=%q request=%q body=%#v", gotPeer, clientType, clientVersion, requestID, request)
			}
			return runtimeapi.RevealSourceURLResponse{
				SourceID: request.SourceID,
				URL:      "https://user:pass@example.com/config?token=secret",
			}, nil
		},
		preview: func(gotPeer runtimeapi.PeerIdentity, clientType, clientVersion, requestID string, request runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsPreview, error) {
			previewCalls++
			if gotPeer.Key() != peer.Key() || clientType != "gui" || clientVersion != "test" || requestID != "request-sensitive" || !request.IncludeFullLogs {
				t.Fatalf("diagnostics preview metadata peer=%#v type=%q version=%q request=%q body=%#v", gotPeer, clientType, clientVersion, requestID, request)
			}
			return runtimeapi.DiagnosticsPreview{
				Warning: runtimeapi.SensitiveDataWarning,
				Items:   []runtimeapi.DiagnosticItem{{Name: "sensitive/logs/runtime.log", Included: true, Sensitive: true}},
			}, nil
		},
		create: func(gotPeer runtimeapi.PeerIdentity, clientType, clientVersion, requestID string, request runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsResult, error) {
			createCalls++
			if gotPeer.Key() != peer.Key() || clientType != "gui" || clientVersion != "test" || requestID != "request-sensitive" || !request.IncludeFullLogs || !request.ConfirmSensitive {
				t.Fatalf("diagnostics create metadata peer=%#v type=%q version=%q request=%q body=%#v", gotPeer, clientType, clientVersion, requestID, request)
			}
			return runtimeapi.DiagnosticsResult{FileName: "diagnostics.zip", Size: 10}, nil
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	serve := func(path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		request.Header.Set(HeaderRequestID, "request-sensitive")
		request.Header.Set(HeaderProtocolVersion, "1")
		request.Header.Set(HeaderClientType, "gui")
		request.Header.Set(HeaderClientVersion, "test")
		request = withPeerContext(request, peer, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}

	sourceID := "src_0123456789abcdef0123456789abcdef"
	recorder := serve("/v1/sources/reveal-url", `{"source_id":"`+sourceID+`","confirm":false}`)
	if recorder.Code != http.StatusBadRequest || revealCalls != 0 {
		t.Fatalf("unconfirmed reveal status=%d calls=%d body=%s", recorder.Code, revealCalls, recorder.Body.String())
	}
	recorder = serve("/v1/sources/reveal-url", `{"source_id":"`+sourceID+`","confirm":true}`)
	if recorder.Code != http.StatusOK || revealCalls != 1 || !strings.Contains(recorder.Body.String(), "token=secret") ||
		recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("confirmed reveal status=%d calls=%d headers=%v body=%s", recorder.Code, revealCalls, recorder.Header(), recorder.Body.String())
	}

	recorder = serve("/v1/diagnostics/preview", `{"include_full_logs":true}`)
	if recorder.Code != http.StatusOK || previewCalls != 1 || !strings.Contains(recorder.Body.String(), runtimeapi.SensitiveDataWarning) {
		t.Fatalf("diagnostics preview status=%d calls=%d body=%s", recorder.Code, previewCalls, recorder.Body.String())
	}
	recorder = serve("/v1/diagnostics/create", `{"include_full_logs":true}`)
	if recorder.Code != http.StatusBadRequest || createCalls != 0 {
		t.Fatalf("unconfirmed diagnostics status=%d calls=%d body=%s", recorder.Code, createCalls, recorder.Body.String())
	}
	recorder = serve("/v1/diagnostics/create", `{"include_full_logs":true,"confirm_sensitive":true}`)
	if recorder.Code != http.StatusCreated || createCalls != 1 || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("confirmed diagnostics status=%d calls=%d headers=%v body=%s", recorder.Code, createCalls, recorder.Header(), recorder.Body.String())
	}
}

func TestMihomoUpdateBundleAndPreviewUseAuthorizedLocalIPC(t *testing.T) {
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	bundleBody := []byte("offline-update-bundle")
	sum := sha256.Sum256(bundleBody)
	digest := hex.EncodeToString(sum[:])
	uploadCalls := 0
	previewCalls := 0
	service := updateObserver{
		operatorObserver: operatorObserver{observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{ProtocolVersion: runtimeapi.ProtocolVersion}, nil
		}},
		upload: func(gotPeer runtimeapi.PeerIdentity, size int64, gotDigest string, body io.Reader) (runtimeapi.MihomoUpdateBundle, error) {
			uploadCalls++
			read, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if gotPeer.Key() != peer.Key() || size != int64(len(bundleBody)) ||
				gotDigest != digest || !bytes.Equal(read, bundleBody) {
				t.Fatalf("unexpected update upload peer=%#v size=%d digest=%q body=%q", gotPeer, size, gotDigest, read)
			}
			return runtimeapi.MihomoUpdateBundle{
				ID:        "bundle_0123456789abcdef0123456789abcdef",
				Size:      size,
				SHA256:    gotDigest,
				ExpiresAt: time.Now().Add(time.Minute),
			}, nil
		},
		preview: func(gotPeer runtimeapi.PeerIdentity, request runtimeapi.MihomoUpdatePreviewRequest) (runtimeapi.MihomoUpdatePlan, error) {
			previewCalls++
			if gotPeer.Key() != peer.Key() ||
				request.Source != runtimeapi.MihomoUpdateSourceOfflineTUF ||
				request.BundleID != "bundle_0123456789abcdef0123456789abcdef" {
				t.Fatalf("unexpected update preview peer=%#v request=%#v", gotPeer, request)
			}
			return runtimeapi.MihomoUpdatePlan{
				PlanID:  "plan_0123456789abcdef0123456789abcdef",
				Source:  request.Source,
				Trust:   runtimeapi.MihomoUpdateTrustTUF,
				Version: "v1.2.3",
			}, nil
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/mihomo/update-bundles", bytes.NewReader(bundleBody))
	request.Header.Set(HeaderRequestID, "request-update")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientType, "cli")
	request.Header.Set(HeaderClientVersion, "test")
	request.Header.Set("Content-Type", runtimeapi.MihomoUpdateBundleContentType)
	request.Header.Set(HeaderContentSize, strconv.Itoa(len(bundleBody)))
	request.Header.Set(HeaderContentSHA256, digest)
	request = withPeerContext(request, peer, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || uploadCalls != 1 ||
		!strings.Contains(recorder.Body.String(), "bundle_0123456789abcdef0123456789abcdef") {
		t.Fatalf("update upload status=%d calls=%d body=%s", recorder.Code, uploadCalls, recorder.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"/v1/mihomo/updates/preview",
		strings.NewReader(`{"source":"offline_tuf","bundle_id":"bundle_0123456789abcdef0123456789abcdef"}`),
	)
	request.Header.Set(HeaderRequestID, "request-update-preview")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientType, "cli")
	request.Header.Set(HeaderClientVersion, "test")
	request = withPeerContext(request, peer, nil)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || previewCalls != 1 ||
		!strings.Contains(recorder.Body.String(), "plan_0123456789abcdef0123456789abcdef") {
		t.Fatalf("update preview status=%d calls=%d body=%s", recorder.Code, previewCalls, recorder.Body.String())
	}
}

func TestProductUpdatePreviewPreservesAuthenticatedCallerMetadata(t *testing.T) {
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	calls := 0
	service := productUpdateObserver{
		operatorObserver: operatorObserver{observerFunc: func(
			context.Context,
			runtimeapi.PeerIdentity,
		) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{ProtocolVersion: runtimeapi.ProtocolVersion}, nil
		}},
		preview: func(
			gotPeer runtimeapi.PeerIdentity,
			clientType string,
			clientVersion string,
			requestID string,
			request runtimeapi.ProductUpdatePreviewRequest,
		) (runtimeapi.ProductUpdatePlan, error) {
			calls++
			if gotPeer.Key() != peer.Key() ||
				clientType != "gui" ||
				clientVersion != "test" ||
				requestID != "request-product-preview" ||
				request.Source != runtimeapi.ProductUpdateSourceOnlineTUF ||
				request.Version != "v2.0.0" {
				t.Fatalf(
					"product preview peer=%#v client=%s/%s requestID=%s request=%#v",
					gotPeer,
					clientType,
					clientVersion,
					requestID,
					request,
				)
			}
			return runtimeapi.ProductUpdatePlan{
				PlanID:  "product_plan_0123456789abcdef0123456789abcdef",
				Version: "v2.0.0",
			}, nil
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/product/updates/preview",
		strings.NewReader(`{"source":"online_tuf","version":"v2.0.0"}`),
	)
	request.Header.Set(HeaderRequestID, "request-product-preview")
	request.Header.Set(HeaderProtocolVersion, "1")
	request.Header.Set(HeaderClientType, "gui")
	request.Header.Set(HeaderClientVersion, "test")
	request = withPeerContext(request, peer, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 1 ||
		!strings.Contains(recorder.Body.String(), "product_plan_0123456789abcdef0123456789abcdef") {
		t.Fatalf("product preview status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
}

func TestBackupPreviewExportAndRestorePreviewUseAuthorizedLocalIPC(t *testing.T) {
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	archiveBody := []byte("bounded-backup-archive")
	digest := sha256.Sum256(archiveBody)
	contentID := "content_0123456789abcdef0123456789abcdef"
	previewCalls := 0
	exportCalls := 0
	restorePreviewCalls := 0
	service := backupObserver{
		operatorObserver: operatorObserver{observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{ProtocolVersion: runtimeapi.ProtocolVersion}, nil
		}},
		preview: func(gotPeer runtimeapi.PeerIdentity, request runtimeapi.BackupPreviewRequest) (runtimeapi.BackupPreview, error) {
			previewCalls++
			if gotPeer.Key() != peer.Key() || !request.IncludeSecrets {
				t.Fatalf("unexpected backup preview peer=%#v request=%#v", gotPeer, request)
			}
			return runtimeapi.BackupPreview{FormatVersion: 1, Restorable: true, IncludeSecrets: true}, nil
		},
		export: func(gotPeer runtimeapi.PeerIdentity, request runtimeapi.BackupExportRequest) (runtimeapi.BackupArchive, error) {
			exportCalls++
			if gotPeer.Key() != peer.Key() || !request.IncludeSecrets || !request.ConfirmPlaintext {
				t.Fatalf("unexpected backup export peer=%#v request=%#v", gotPeer, request)
			}
			return runtimeapi.BackupArchive{
				FileName:       "submux-runtime-backup.zip",
				Size:           int64(len(archiveBody)),
				SHA256:         hex.EncodeToString(digest[:]),
				CreatedAt:      time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC),
				Restorable:     true,
				IncludeSecrets: true,
				Body:           archiveBody,
			}, nil
		},
		restorePreview: func(gotPeer runtimeapi.PeerIdentity, request runtimeapi.BackupRestorePreviewRequest) (runtimeapi.BackupRestorePreview, error) {
			restorePreviewCalls++
			if gotPeer.Key() != peer.Key() || request.ContentID != contentID {
				t.Fatalf("unexpected restore preview peer=%#v request=%#v", gotPeer, request)
			}
			return runtimeapi.BackupRestorePreview{
				ContentID:       request.ContentID,
				Restorable:      true,
				PendingSettings: []string{"listeners", "tun", "gateway"},
			}, nil
		},
	}
	server, err := NewServer(service, AuthorizeFunc(func(runtimeapi.PeerIdentity) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	serve := func(path, requestID, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set(HeaderRequestID, requestID)
		request.Header.Set(HeaderProtocolVersion, "1")
		request.Header.Set(HeaderClientType, "gui")
		request.Header.Set(HeaderClientVersion, "test")
		request = withPeerContext(request, peer, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}

	recorder := serve("/v1/backups/preview", "request-backup-preview", `{"include_secrets":true}`)
	if recorder.Code != http.StatusOK || previewCalls != 1 ||
		recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("backup preview status=%d calls=%d headers=%v body=%s", recorder.Code, previewCalls, recorder.Header(), recorder.Body.String())
	}
	recorder = serve("/v1/backups/export", "request-backup-export", `{"include_secrets":true,"confirm_plaintext":true}`)
	if recorder.Code != http.StatusOK || exportCalls != 1 ||
		!bytes.Equal(recorder.Body.Bytes(), archiveBody) ||
		recorder.Header().Get("Content-Type") != runtimeapi.RuntimeBackupContentType ||
		recorder.Header().Get(HeaderContentSHA256) != hex.EncodeToString(digest[:]) ||
		recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("backup export status=%d calls=%d headers=%v body=%q", recorder.Code, exportCalls, recorder.Header(), recorder.Body.Bytes())
	}
	recorder = serve("/v1/backups/restore/preview", "request-backup-restore", `{"content_id":"`+contentID+`"}`)
	if recorder.Code != http.StatusOK || restorePreviewCalls != 1 ||
		!strings.Contains(recorder.Body.String(), contentID) {
		t.Fatalf("restore preview status=%d calls=%d body=%s", recorder.Code, restorePreviewCalls, recorder.Body.String())
	}
}

func TestValidActionAcceptsOnlyWellFormedOperations(t *testing.T) {
	sourceID := "src_0123456789abcdef0123456789abcdef"
	for _, action := range []runtimeapi.Action{
		{
			Kind: runtimeapi.ActionApplyImportedConfig,
			Params: runtimeapi.ActionParams{
				ContentID: "content_0123456789abcdef0123456789abcdef",
			},
		},
		{
			Kind: runtimeapi.ActionAddRemoteSource,
			Params: runtimeapi.ActionParams{
				ContentID: "content_0123456789abcdef0123456789abcdef",
			},
		},
		{
			Kind: runtimeapi.ActionAddImportedSource,
			Params: runtimeapi.ActionParams{
				ContentID:  "content_0123456789abcdef0123456789abcdef",
				SourceName: "local-copy",
			},
		},
		{
			Kind:   runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{SourceID: sourceID},
		},
		{
			Kind: runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{
				SourceID: sourceID,
				Route:    runtimeapi.SourceRouteMihomo,
			},
		},
		{
			Kind:   runtimeapi.ActionApplySource,
			Params: runtimeapi.ActionParams{SourceID: sourceID},
		},
		{
			Kind: runtimeapi.ActionSwitchSource,
			Params: runtimeapi.ActionParams{
				SourceID:  sourceID,
				Route:     runtimeapi.SourceRouteDirect,
				UseCached: true,
			},
		},
		{
			Kind: runtimeapi.ActionDeleteSource,
			Params: runtimeapi.ActionParams{
				SourceID: sourceID,
				Confirm:  true,
			},
		},
		{
			Kind: runtimeapi.ActionAddManagedResource,
			Params: runtimeapi.ActionParams{
				ContentID:    "content_0123456789abcdef0123456789abcdef",
				ResourceKind: runtimeapi.ResourceKindProxyProvider,
				ResourceName: "provider.main",
			},
		},
		{
			Kind: runtimeapi.ActionSetAdvancedOverride,
			Params: runtimeapi.ActionParams{
				ContentID: "content_0123456789abcdef0123456789abcdef",
			},
		},
		{
			Kind: runtimeapi.ActionUpdateMihomo,
			Params: runtimeapi.ActionParams{
				PlanID:  "plan_0123456789abcdef0123456789abcdef",
				Trust:   runtimeapi.MihomoUpdateTrustTUF,
				Confirm: true,
			},
		},
		{
			Kind: runtimeapi.ActionRestoreBackup,
			Params: runtimeapi.ActionParams{
				ContentID: "content_0123456789abcdef0123456789abcdef",
				Confirm:   true,
			},
		},
		{
			Kind:   runtimeapi.ActionRollbackMihomo,
			Params: runtimeapi.ActionParams{Confirm: true},
		},
		{
			Kind: runtimeapi.ActionCheckProduct,
		},
		{
			Kind: runtimeapi.ActionUpdateProduct,
			Params: runtimeapi.ActionParams{
				PlanID:  "product_plan_0123456789abcdef0123456789abcdef",
				Trust:   runtimeapi.ProductUpdateTrustTUF,
				Confirm: true,
			},
		},
		{
			Kind:   runtimeapi.ActionRollbackProduct,
			Params: runtimeapi.ActionParams{Confirm: true},
		},
	} {
		if !validAction(action) {
			t.Fatalf("valid source action rejected: %#v", action)
		}
	}
	for _, action := range []runtimeapi.Action{
		{
			Kind: runtimeapi.ActionRestoreBackup,
			Params: runtimeapi.ActionParams{
				ContentID: "content_0123456789abcdef0123456789abcdef",
			},
		},
		{
			Kind: runtimeapi.ActionAddRemoteSource,
			Params: runtimeapi.ActionParams{
				ContentID: "content_0123456789abcdef0123456789abcdeg",
			},
		},
		{
			Kind: runtimeapi.ActionAddImportedSource,
			Params: runtimeapi.ActionParams{
				ContentID:  "content_0123456789abcdef0123456789abcdef",
				SourceName: "bad\nname",
			},
		},
		{
			Kind:   runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{SourceID: "src_0123456789abcdef0123456789abcdeg"},
		},
		{
			Kind: runtimeapi.ActionApplySource,
			Params: runtimeapi.ActionParams{
				SourceID: sourceID,
				Route:    runtimeapi.SourceRouteDirect,
			},
		},
		{
			Kind: runtimeapi.ActionApplySource,
			Params: runtimeapi.ActionParams{
				SourceID:  sourceID,
				ContentID: "content_forbidden",
			},
		},
		{
			Kind: runtimeapi.ActionSwitchSource,
			Params: runtimeapi.ActionParams{
				SourceID: sourceID,
				Confirm:  true,
			},
		},
		{
			Kind: runtimeapi.ActionDeleteSource,
			Params: runtimeapi.ActionParams{
				SourceID:  sourceID,
				UseCached: true,
			},
		},
		{
			Kind: runtimeapi.ActionAddManagedResource,
			Params: runtimeapi.ActionParams{
				ContentID:    "content_0123456789abcdef0123456789abcdef",
				ResourceKind: "arbitrary-file",
				ResourceName: "../provider",
			},
		},
		{
			Kind: runtimeapi.ActionSetAdvancedOverride,
			Params: runtimeapi.ActionParams{
				ContentID:    "content_0123456789abcdef0123456789abcdef",
				ResourceName: "forbidden",
			},
		},
		{
			Kind: runtimeapi.ActionUpdateMihomo,
			Params: runtimeapi.ActionParams{
				PlanID: "plan_0123456789abcdef0123456789abcdef",
				Trust:  runtimeapi.MihomoUpdateTrustTUF,
			},
		},
		{
			Kind: runtimeapi.ActionUpdateProduct,
			Params: runtimeapi.ActionParams{
				PlanID:  "plan_0123456789abcdef0123456789abcdef",
				Trust:   runtimeapi.ProductUpdateTrustTUF,
				Confirm: true,
			},
		},
		{
			Kind: runtimeapi.ActionUpdateProduct,
			Params: runtimeapi.ActionParams{
				PlanID: "product_plan_0123456789abcdef0123456789abcdef",
				Trust:  runtimeapi.ProductUpdateTrustTUF,
			},
		},
		{
			Kind:   runtimeapi.ActionCheckProduct,
			Params: runtimeapi.ActionParams{Confirm: true},
		},
		{
			Kind:   runtimeapi.ActionRollbackProduct,
			Params: runtimeapi.ActionParams{Trust: runtimeapi.ProductUpdateTrustTUF, Confirm: true},
		},
		{
			Kind:   runtimeapi.ActionStartProxy,
			Params: runtimeapi.ActionParams{Trust: runtimeapi.MihomoUpdateTrustTUF},
		},
	} {
		if validAction(action) {
			t.Fatalf("invalid source action accepted: %#v", action)
		}
	}
}
