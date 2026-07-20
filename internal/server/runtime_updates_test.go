package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"submux/internal/store"
)

func TestAuthenticatedBrowserReceivesRuntimeChangeEvents(t *testing.T) {
	st := newTestStore(t)
	instance, err := st.CreateRuntimeInstance(store.RuntimeInstance{
		Name: "event-test", DeviceKey: "device-key", OS: "linux", Arch: "amd64",
		Capabilities: []string{"agent.runtime.observe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	app := New(st, nil)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	admin := initAndClient(t, srv)
	baseURL, _ := url.Parse(srv.URL)
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/runtime/instances/1/events", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range admin.Jar.Cookies(baseURL) {
		config.Header.Add("Cookie", cookie.String())
	}
	connection, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				app.events.notify(instance.ID, "heartbeat")
			case <-done:
				return
			}
		}
	}()
	var message struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}
	if err := websocket.JSON.Receive(connection, &message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "runtime_changed" || message.Reason != "heartbeat" {
		t.Fatalf("unexpected runtime event: %#v", message)
	}
}
