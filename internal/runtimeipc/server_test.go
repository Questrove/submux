package runtimeipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

type observerFunc func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error)

func (function observerFunc) Observe(ctx context.Context, peer runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	return function(ctx, peer)
}

type operatorObserver struct {
	observerFunc
}

func (operatorObserver) UploadImport(context.Context, runtimeapi.PeerIdentity, string, int64, string, []byte) (runtimeapi.ImportContent, error) {
	return runtimeapi.ImportContent{}, nil
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

func (operatorObserver) Execute(context.Context, runtimeapi.PeerIdentity, string, runtimeapi.CreateOperationRequest) (runtimeapi.Operation, bool, error) {
	return runtimeapi.Operation{}, false, nil
}

func (operatorObserver) GetOperation(context.Context, string) (runtimeapi.Operation, error) {
	return runtimeapi.Operation{}, nil
}

func (operatorObserver) CancelOperation(context.Context, runtimeapi.PeerIdentity, string, runtimeapi.CancelOperationRequest) (runtimeapi.Operation, bool, error) {
	return runtimeapi.Operation{}, false, nil
}

func (operatorObserver) VerifyProxy(context.Context) (runtimeapi.ProxyVerification, error) {
	return runtimeapi.ProxyVerification{}, nil
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
	request = withPeerContext(request, runtimeapi.PeerIdentity{Platform: "test", UID: 1000}, nil)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"validated":true`) {
		t.Fatalf("response=%s", recorder.Body.String())
	}
}
