package runtimetraffic

import (
	"context"
	"errors"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

type scriptedReader struct {
	readings []Reading
	errors   []error
	index    int
}

func (r *scriptedReader) ReadTraffic(context.Context) (Reading, error) {
	index := r.index
	r.index++
	if index < len(r.errors) && r.errors[index] != nil {
		return Reading{}, r.errors[index]
	}
	return r.readings[index], nil
}

func TestCollectorTracksCurrentRunAndMarksRestartGap(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	reader := &scriptedReader{
		readings: []Reading{
			{UploadTotal: 100, DownloadTotal: 200, ActiveConnections: 2},
			{UploadTotal: 300, DownloadTotal: 700, ActiveConnections: 4},
			{},
			{UploadTotal: 20, DownloadTotal: 40, ActiveConnections: 1},
		},
		errors: []error{nil, nil, errors.New("Mihomo stopped"), nil},
	}
	collector := &Collector{Reader: reader, Now: func() time.Time { return now }}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := collector.Status()
	if status.UploadSpeed != 200 || status.DownloadSpeed != 500 || status.ActiveConnections != 4 ||
		status.UploadTotal != 300 || status.DownloadTotal != 700 {
		t.Fatalf("traffic status=%#v", status)
	}
	now = now.Add(time.Second)
	if err := collector.collect(t.Context()); err == nil {
		t.Fatal("collector accepted unavailable Mihomo")
	}
	if collector.Status().Available || collector.Status().UploadTotal != 300 || collector.Status().DownloadTotal != 700 {
		t.Fatalf("unavailable status=%#v", collector.Status())
	}
	now = now.Add(time.Second)
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	history := collector.History(runtimeapi.TrafficHistoryRequest{})
	if history.Status.UploadTotal != 20 || history.Status.DownloadTotal != 40 || history.Status.UploadSpeed != 0 {
		t.Fatalf("new run status=%#v", history.Status)
	}
	foundGap := false
	for _, sample := range history.Samples {
		foundGap = foundGap || sample.Discontinuity
	}
	if !foundGap {
		t.Fatalf("restart history has no discontinuity: %#v", history.Samples)
	}
}

func TestCollectorHistorySupportsTimeAndCursorRanges(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	reader := &scriptedReader{readings: []Reading{{}, {UploadTotal: 10}, {UploadTotal: 30}}}
	collector := &Collector{Reader: reader, Now: func() time.Time { return now }}
	for range 3 {
		if err := collector.collect(t.Context()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}

	recent := collector.History(runtimeapi.TrafficHistoryRequest{Since: now.Add(-90 * time.Second)})
	if len(recent.Samples) != 1 || recent.Samples[0].UploadTotal != 30 {
		t.Fatalf("time range history=%#v", recent)
	}
	increment := collector.History(runtimeapi.TrafficHistoryRequest{After: 1})
	if len(increment.Samples) != 2 || increment.LatestCursor != 3 {
		t.Fatalf("cursor history=%#v", increment)
	}
}

func TestCollectorPrunesHistoryToFifteenMinutes(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	reader := &scriptedReader{readings: []Reading{{}, {UploadTotal: 10}}}
	collector := &Collector{Reader: reader, Now: func() time.Time { return now }}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(runtimeapi.TrafficHistoryRetention + time.Second)
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	history := collector.History(runtimeapi.TrafficHistoryRequest{})
	if len(history.Samples) != 1 || history.Samples[0].UploadTotal != 10 {
		t.Fatalf("pruned history=%#v", history)
	}
}

func TestCollectorFiltersAndPagesConnectionDetailsAndRetainsStaleData(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 10, 0, time.UTC)
	observedAt := now
	connections := []runtimeapi.Connection{
		{ID: "1", Target: "api.example.com:443", Process: "browser", Rule: "DomainSuffix", RulePayload: "example.com", OutboundChain: []string{"Proxy", "Tokyo"}, StartedAt: now.Add(-10 * time.Second)},
		{ID: "2", Target: "dns.example:53", Process: "resolver", Rule: "Match", OutboundChain: []string{"DIRECT"}, StartedAt: now.Add(-5 * time.Second)},
		{ID: "3", Target: "updates.test:443", Process: "updater", Rule: "Domain", OutboundChain: []string{"Proxy", "Osaka"}, StartedAt: now.Add(-2 * time.Second)},
	}
	reader := &scriptedReader{
		readings: []Reading{{ActiveConnections: len(connections), Connections: connections}},
		errors:   []error{nil, errors.New("Mihomo unavailable")},
	}
	collector := &Collector{Reader: reader, Now: func() time.Time { return now }}
	if err := collector.collect(t.Context()); err != nil {
		t.Fatal(err)
	}

	queries := []runtimeapi.ConnectionQuery{
		{Target: "API.EXAMPLE"},
		{Process: "BROWSER"},
		{Rule: "example.com"},
		{Node: "tokyo"},
	}
	for _, query := range queries {
		page := collector.Connections(query)
		if !page.Available || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "1" || page.Items[0].DurationSeconds != 10 {
			t.Fatalf("connection query=%#v page=%#v", query, page)
		}
	}
	page := collector.Connections(runtimeapi.ConnectionQuery{Page: 2, PageSize: 1})
	if page.Total != 3 || len(page.Items) != 1 || page.Items[0].ID != "2" || page.Page != 2 || page.PageSize != 1 {
		t.Fatalf("paged connections=%#v", page)
	}

	now = now.Add(time.Second)
	if err := collector.collect(t.Context()); err == nil {
		t.Fatal("collector accepted unavailable Mihomo")
	}
	stale := collector.Connections(runtimeapi.ConnectionQuery{Target: "api.example"})
	if stale.Available || !stale.ObservedAt.Equal(observedAt) || len(stale.Items) != 1 || stale.Items[0].DurationSeconds != 10 {
		t.Fatalf("stale connections=%#v", stale)
	}
}
