package runtimeipc

import (
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
