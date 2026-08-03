package runtimetraffic

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
)

func TestMihomoReaderReadsConnectionCounters(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/connections" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"uploadTotal":123,"downloadTotal":456,"connections":[{"id":"connection-1","metadata":{"network":"tcp","type":"Mixed","sourceIP":"127.0.0.1","sourcePort":"52100","destinationIP":"203.0.113.10","destinationPort":"443","host":"api.example.com","process":"browser","processPath":"C:/secret/browser.exe"},"upload":12,"download":34,"start":"2026-08-03T12:00:00Z","chains":["Proxy","Tokyo"],"rule":"DomainSuffix","rulePayload":"example.com"},{"id":"connection-2","metadata":{"network":"udp","destinationIP":"1.1.1.1","destinationPort":"53"}}]}`)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	reader := MihomoReader{Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}}
	reading, err := reader.ReadTraffic(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reading.UploadTotal != 123 || reading.DownloadTotal != 456 || reading.ActiveConnections != 2 {
		t.Fatalf("reading=%#v", reading)
	}
	connection := reading.Connections[0]
	if connection.ID != "connection-1" || connection.Source != "127.0.0.1:52100" ||
		connection.Target != "api.example.com:443" || connection.Protocol != "tcp" ||
		connection.Process != "browser" || connection.Rule != "DomainSuffix" ||
		connection.RulePayload != "example.com" || len(connection.OutboundChain) != 2 ||
		connection.Upload != 12 || connection.Download != 34 {
		t.Fatalf("connection=%#v", connection)
	}
}
