package runtimetraffic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestMihomoControllerTreatsMissingConnectionAsIdempotentSuccess(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan string, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- request.Method + " " + request.URL.EscapedPath()
		if request.URL.Path == "/connections/missing-connection" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	controller := MihomoController{Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}}
	alreadyClosed, err := controller.CloseConnection(t.Context(), "missing-connection")
	if err != nil || !alreadyClosed {
		t.Fatalf("missing connection alreadyClosed=%t err=%v", alreadyClosed, err)
	}
	alreadyClosed, err = controller.CloseConnection(t.Context(), "active/connection")
	if err != nil || alreadyClosed {
		t.Fatalf("active connection alreadyClosed=%t err=%v", alreadyClosed, err)
	}
	if first, second := <-requests, <-requests; first != "DELETE /connections/missing-connection" || second != "DELETE /connections/active%2Fconnection" {
		t.Fatalf("requests=%q, %q", first, second)
	}
}

func TestMihomoControllerReturnsTypedErrorForRejectedClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "internal details must not escape", http.StatusInternalServerError)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	controller := MihomoController{Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}}
	if _, err := controller.CloseConnection(t.Context(), "connection-1"); !errors.Is(err, ErrMihomoConnectionControl) {
		t.Fatalf("close error=%v", err)
	}
}
