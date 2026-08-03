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
		_, _ = fmt.Fprint(writer, `{"uploadTotal":123,"downloadTotal":456,"connections":[{},{}]}`)
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
}
