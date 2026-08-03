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
