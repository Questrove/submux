package runtimetraffic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestConnectionManagerClosesCurrentFilteredScope(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	collector := &Collector{
		Reader: &scriptedReader{readings: []Reading{{Connections: []runtimeapi.Connection{
			{ID: "connection-1", Target: "api.example.com:443", Process: "browser", Rule: "Domain", OutboundChain: []string{"Proxy", "Tokyo"}},
			{ID: "connection-2", Target: "updates.example.com:443", Process: "updater", Rule: "Domain", OutboundChain: []string{"Proxy", "Osaka"}},
		}}}},
		Now: func() time.Time { return now },
	}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan string, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		closed <- request.URL.Path
		writer.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	manager := &ConnectionManager{
		Collector: collector,
		Controller: MihomoController{Dial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		}},
	}
	query := runtimeapi.ConnectionQuery{Node: "tokyo"}
	result, err := manager.CloseScope(t.Context(), query, 1, collector.Connections(query).ScopeToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 1 || result.Closed != 1 || result.AlreadyClosed != 0 {
		t.Fatalf("close result=%#v", result)
	}
	if path := <-closed; path != "/connections/connection-1" {
		t.Fatalf("closed path=%q", path)
	}
}

func TestConnectionManagerClosesOnlySnapshotMembersInUnfilteredScope(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	collector := &Collector{
		Reader: &scriptedReader{readings: []Reading{{Connections: []runtimeapi.Connection{
			{ID: "connection-1", Target: "one.example:443"},
			{ID: "connection-2", Target: "two.example:443"},
		}}}},
		Now: func() time.Time { return now },
	}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan string, 3)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- request.Method + " " + request.URL.Path
		writer.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	manager := &ConnectionManager{
		Collector: collector,
		Controller: MihomoController{Dial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		}},
	}
	query := runtimeapi.ConnectionQuery{}
	result, err := manager.CloseScope(t.Context(), query, 2, collector.Connections(query).ScopeToken)
	if err != nil || result.Matched != 2 || result.Closed != 2 {
		t.Fatalf("close all result=%#v err=%v", result, err)
	}
	if request := <-requests; request != "DELETE /connections/connection-1" {
		t.Fatalf("first close request=%q", request)
	}
	if request := <-requests; request != "DELETE /connections/connection-2" {
		t.Fatalf("second close request=%q", request)
	}
	select {
	case extra := <-requests:
		t.Fatalf("scope close sent extra request %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestConnectionManagerReturnsClearResultWhenSingleConnectionDisappeared(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.NotFound(writer, request)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	manager := &ConnectionManager{Controller: MihomoController{Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}}}
	result, err := manager.CloseOne(t.Context(), "connection-missing")
	if err != nil || result.Matched != 1 || result.Closed != 0 || result.AlreadyClosed != 1 {
		t.Fatalf("close missing result=%#v err=%v", result, err)
	}
}

func TestConnectionManagerRefusesScopeThatGrewAfterConfirmation(t *testing.T) {
	collector := &Collector{
		Reader: &scriptedReader{readings: []Reading{{Connections: []runtimeapi.Connection{
			{ID: "connection-1"}, {ID: "connection-2"},
		}}}},
		Now: time.Now,
	}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager := &ConnectionManager{Collector: collector}
	query := runtimeapi.ConnectionQuery{}
	if _, err := manager.CloseScope(t.Context(), query, 1, collector.Connections(query).ScopeToken); !errors.Is(err, ErrConnectionScopeChanged) {
		t.Fatalf("scope growth error=%v", err)
	}
}

func TestConnectionManagerRefusesEqualCountScopeWithDifferentMembers(t *testing.T) {
	reader := &scriptedReader{readings: []Reading{
		{Connections: []runtimeapi.Connection{{ID: "connection-old", OutboundChain: []string{"Proxy", "Tokyo"}}}},
		{Connections: []runtimeapi.Connection{{ID: "connection-new", OutboundChain: []string{"Proxy", "Tokyo"}}}},
	}}
	collector := &Collector{Reader: reader, Now: time.Now}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	query := runtimeapi.ConnectionQuery{Node: "Tokyo"}
	confirmed := collector.Connections(query)
	if confirmed.ScopeToken == "" {
		t.Fatal("connection page did not bind the filtered stable ID set")
	}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}

	manager := &ConnectionManager{Collector: collector}
	if _, err := manager.CloseScope(t.Context(), query, confirmed.Total, confirmed.ScopeToken); !errors.Is(err, ErrConnectionScopeChanged) {
		t.Fatalf("equal-count replacement error=%v", err)
	}
}
