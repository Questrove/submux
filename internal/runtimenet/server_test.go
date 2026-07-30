package runtimenet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPrivilegedNetworkServerAcceptsOnlyRuntimeUIDAndTypedPayloads(t *testing.T) {
	manager, err := OpenManager(
		filepath.Join(t.TempDir(), "network"),
		1001,
		&fakeSystem{discovery: Discovery{Device: "smxtun0"}},
	)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	server, err := NewServer(manager)
	if err != nil {
		t.Fatalf("create privileged Runtime network server: %v", err)
	}
	handler := server.Handler()
	connectionID := "conn_test"
	validRequest := `{"request":{"protocol_version":` + strconv.Itoa(ProtocolVersion) +
		`,"runtime_instance_id":"runtime_0123456789abcdef0123456789abcdef","client_nonce":"` +
		strings.Repeat("a", 64) + `"}}`

	rootResponse := serveNetworkRequest(handler, connectionID, 0, "/v1/session", validRequest)
	if rootResponse.Code != http.StatusForbidden {
		t.Fatalf("root session status=%d body=%s", rootResponse.Code, rootResponse.Body.String())
	}

	for _, field := range []string{"url", "path", "command", "argv", "firewall_fragment"} {
		body := strings.TrimSuffix(validRequest, "}") + `,"` + field + `":"forbidden"}`
		response := serveNetworkRequest(handler, connectionID, 1001, "/v1/session", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("session payload field %q status=%d body=%s", field, response.Code, response.Body.String())
		}
	}

	sessionResponse := serveNetworkRequest(handler, connectionID, 1001, "/v1/session", validRequest)
	if sessionResponse.Code != http.StatusCreated {
		t.Fatalf("Runtime session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	var session Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode Runtime network session: %v", err)
	}
	for _, field := range []string{"url", "path", "command", "argv", "firewall_fragment"} {
		body := `{"session_id":"` + session.ID +
			`","request":{"mode":"tun","` + field + `":"forbidden"}}`
		response := serveNetworkRequest(handler, connectionID, 1001, "/v1/preview", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("preview payload field %q status=%d body=%s", field, response.Code, response.Body.String())
		}
	}
}

func TestPrivilegedNetworkServerRejectsQueriesMethodsAndUnknownEndpoints(t *testing.T) {
	manager, err := OpenManager(
		filepath.Join(t.TempDir(), "network"),
		1001,
		&fakeSystem{discovery: Discovery{Device: "smxtun0"}},
	)
	if err != nil {
		t.Fatalf("open privileged Runtime network manager: %v", err)
	}
	server, _ := NewServer(manager)
	handler := server.Handler()

	query := serveNetworkRequest(handler, "conn_test", 1001, "/v1/session?path=/tmp", `{}`)
	if query.Code != http.StatusBadRequest {
		t.Fatalf("query status=%d body=%s", query.Code, query.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	request = request.WithContext(context.WithValue(
		request.Context(),
		connectionContextKey{},
		connectionIdentity{id: "conn_test", uid: 1001},
	))
	method := httptest.NewRecorder()
	handler.ServeHTTP(method, request)
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d body=%s", method.Code, method.Body.String())
	}
	unknown := serveNetworkRequest(handler, "conn_test", 1001, "/v1/run-command", `{}`)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown endpoint status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func serveNetworkRequest(
	handler http.Handler,
	connectionID string,
	uid uint32,
	target string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
	request = request.WithContext(context.WithValue(
		request.Context(),
		connectionContextKey{},
		connectionIdentity{id: connectionID, uid: uid},
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
