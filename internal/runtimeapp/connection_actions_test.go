package runtimeapp

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimecore"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimestate"
	"submux/internal/runtimetraffic"
)

type trafficReaderFunc func(context.Context) (runtimetraffic.Reading, error)

func (function trafficReaderFunc) ReadTraffic(ctx context.Context) (runtimetraffic.Reading, error) {
	return function(ctx)
}

type runtimeVerifierFunc func(context.Context, string) error

func (function runtimeVerifierFunc) VerifyRuntime(ctx context.Context, proxyAddress string) error {
	return function(ctx, proxyAddress)
}

func TestMihomoExecutorClosesConfirmedFilteredConnectionScope(t *testing.T) {
	collector := &runtimetraffic.Collector{Reader: trafficReaderFunc(func(context.Context) (runtimetraffic.Reading, error) {
		return runtimetraffic.Reading{Connections: []runtimeapi.Connection{
			{ID: "connection-1", Target: "api.example.com:443", OutboundChain: []string{"Proxy", "Tokyo"}},
			{ID: "connection-2", Target: "updates.example.com:443", OutboundChain: []string{"Proxy", "Osaka"}},
		}}, nil
	})}
	collectorContext, cancelCollector := context.WithCancel(t.Context())
	collectorDone := make(chan error, 1)
	go func() { collectorDone <- collector.Run(collectorContext) }()
	t.Cleanup(func() {
		cancelCollector()
		if err := <-collectorDone; err != nil {
			t.Errorf("stop collector: %v", err)
		}
	})
	deadline := time.Now().Add(time.Second)
	for !collector.Connections(runtimeapi.ConnectionQuery{}).Available && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !collector.Connections(runtimeapi.ConnectionQuery{}).Available {
		t.Fatal("collector did not publish connection snapshot")
	}
	query := runtimeapi.ConnectionQuery{Node: "Tokyo"}
	confirmed := collector.Connections(query)

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

	executor := &MihomoExecutor{
		State:    &runtimestate.Store{},
		Core:     &runtimecore.Store{},
		Process:  &runtimeprocess.Process{},
		Verifier: runtimeVerifierFunc(func(context.Context, string) error { return nil }),
		Connections: &runtimetraffic.ConnectionManager{
			Collector: collector,
			Controller: runtimetraffic.MihomoController{Dial: func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			}},
		},
	}
	result, err := executor.Execute(t.Context(), runtimeapi.Operation{Action: runtimeapi.Action{
		Kind: runtimeapi.ActionCloseConnections,
		Params: runtimeapi.ActionParams{
			ConnectionScope:      &query,
			ConnectionScopeToken: confirmed.ScopeToken,
			ConnectionCount:      1,
			Confirm:              true,
		},
	}}, func(string, int, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.MatchedConnections != 1 || result.ClosedConnections != 1 || result.AlreadyClosedConnections != 0 {
		t.Fatalf("operation result=%#v", result)
	}
	if path := <-closed; path != "/connections/connection-1" {
		t.Fatalf("closed path=%q", path)
	}
}

func TestMihomoExecutorRecordsIdempotentSingleConnectionClose(t *testing.T) {
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

	executor := &MihomoExecutor{
		State:    &runtimestate.Store{},
		Core:     &runtimecore.Store{},
		Process:  &runtimeprocess.Process{},
		Verifier: runtimeVerifierFunc(func(context.Context, string) error { return nil }),
		Connections: &runtimetraffic.ConnectionManager{Controller: runtimetraffic.MihomoController{
			Dial: func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			},
		}},
	}
	result, err := executor.Execute(t.Context(), runtimeapi.Operation{Action: runtimeapi.Action{
		Kind: runtimeapi.ActionCloseConnection,
		Params: runtimeapi.ActionParams{
			ConnectionID: "connection-missing",
		},
	}}, func(string, int, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.ConnectionID != "connection-missing" || result.ClosedConnections != 0 || !result.ConnectionAlreadyClosed {
		t.Fatalf("operation result=%#v", result)
	}
}
