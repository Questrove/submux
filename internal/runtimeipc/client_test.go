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

func TestClientReadsFilteredConnectionPage(t *testing.T) {
	client := &Client{
		clientType: "tui",
		version:    "test",
		http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			query := request.URL.Query()
			if request.URL.Path != "/v1/connections" || query.Get("target") != "api.example" ||
				query.Get("process") != "browser" || query.Get("rule") != "Domain" ||
				query.Get("node") != "Tokyo" || query.Get("page") != "2" || query.Get("page_size") != "25" ||
				request.Header.Get(HeaderClientType) != "tui" {
				t.Fatalf("request=%s headers=%v", request.URL.String(), request.Header)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"items":[{"id":"connection-1","target":"api.example.com:443","protocol":"tcp","started_at":"2026-08-03T12:00:00Z"}],"total":1,"page":2,"page_size":25,"available":true,"observed_at":"2026-08-03T12:00:10Z"}`,
				)),
				Request: request,
			}, nil
		})},
	}
	page, err := client.Connections(t.Context(), runtimeapi.ConnectionQuery{
		Target: "api.example", Process: "browser", Rule: "Domain", Node: "Tokyo", Page: 2, PageSize: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || page.Page != 2 || page.PageSize != 25 || len(page.Items) != 1 || page.Items[0].ID != "connection-1" {
		t.Fatalf("connection page=%#v", page)
	}
}

func TestClientReadsFilteredAppliedRules(t *testing.T) {
	client := &Client{
		clientType: "tui",
		version:    "test",
		http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			query := request.URL.Query()
			if request.URL.Path != "/v1/rules" || query.Get("content") != "api.example" ||
				query.Get("type") != "domain" || query.Get("target") != "proxy" {
				t.Fatalf("request=%s", request.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"view":"applied","configuration_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","items":[{"order":7,"type":"DOMAIN","condition":"api.example","target":"PROXY","origin":"source","content":"DOMAIN,api.example,PROXY"}],"total":1}`,
				)),
				Request: request,
			}, nil
		})},
	}
	rules, err := client.Rules(t.Context(), runtimeapi.RuleQuery{Content: "api.example", Type: "domain", Target: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if rules.View != runtimeapi.RuleViewApplied || rules.Total != 1 || len(rules.Items) != 1 || rules.Items[0].Order != 7 {
		t.Fatalf("rules=%#v", rules)
	}
}

func TestClientReadsProxyGroupsForSource(t *testing.T) {
	const sourceID = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	client := &Client{
		clientType: "tui",
		version:    "test",
		http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/proxy-groups" || request.URL.Query().Get("source_id") != sourceID {
				t.Fatalf("request=%s", request.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					`{"source_id":"` + sourceID + `","current_source":true,"available":true,"groups":[{"name":"PROXY","type":"select","main":true,"selectable":true,"current":"Tokyo","nodes":[]}],"observed_at":"2026-08-03T12:00:00Z"}`,
				)),
				Request: request,
			}, nil
		})},
	}
	groups, err := client.ProxyGroups(t.Context(), runtimeapi.ProxyGroupQuery{SourceID: sourceID})
	if err != nil {
		t.Fatal(err)
	}
	if groups.SourceID != sourceID || !groups.Available || len(groups.Groups) != 1 || groups.Groups[0].Current != "Tokyo" {
		t.Fatalf("groups=%#v", groups)
	}
}
