package runtimeipc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestClientReadsTrafficHistoryWithCursor(t *testing.T) {
	client := &Client{
		clientType: "tui",
		version:    "test",
		http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/traffic/history" || request.URL.Query().Get("after") != "7" ||
				request.URL.Query().Get("limit") != "32" || request.Header.Get(HeaderClientType) != "tui" {
				t.Fatalf("request=%s headers=%v", request.URL.String(), request.Header)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"status":{"available":true,"upload_total":12,"observed_at":"2026-08-03T12:00:00Z"},"samples":[{"cursor":8,"observed_at":"2026-08-03T12:00:00Z","upload_total":12}],"latest_cursor":8}`,
				)),
				Request: request,
			}, nil
		})},
	}
	history, err := client.TrafficHistory(t.Context(), runtimeapi.TrafficHistoryRequest{After: 7, Limit: 32})
	if err != nil {
		t.Fatal(err)
	}
	if history.LatestCursor != 8 || history.Status.UploadTotal != 12 || len(history.Samples) != 1 {
		t.Fatalf("history=%#v", history)
	}
}

func TestClientTrafficHistoryEncodesSinceTime(t *testing.T) {
	since := time.Date(2026, 8, 3, 12, 0, 0, 123, time.UTC)
	client := &Client{
		clientType: "tui",
		version:    "test",
		http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Query().Get("since") != since.Format(time.RFC3339Nano) {
				t.Fatalf("since=%q", request.URL.Query().Get("since"))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"status":{"observed_at":"0001-01-01T00:00:00Z"}}`)),
				Request:    request,
			}, nil
		})},
	}
	if _, err := client.TrafficHistory(context.Background(), runtimeapi.TrafficHistoryRequest{Since: since}); err != nil {
		t.Fatal(err)
	}
}
