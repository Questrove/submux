package runtimetui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestRuntimeTUIMonitorLoadsFiveMinuteTrafficHistory(t *testing.T) {
	now := time.Now().UTC()
	client := &fakeClient{
		snapshot: shellSnapshot(4, 3),
		trafficHistories: []runtimeapi.TrafficHistory{{
			Status: runtimeapi.TrafficStatus{
				Available: true, UploadSpeed: 2048, DownloadSpeed: 4096,
				UploadTotal: 10 << 20, DownloadTotal: 20 << 20, ActiveConnections: 7,
				ObservedAt: now,
			},
			Samples: []runtimeapi.TrafficSample{
				{Cursor: 1, ObservedAt: now.Add(-time.Second), UploadSpeed: 1024, DownloadSpeed: 2048},
				{Cursor: 2, ObservedAt: now, UploadSpeed: 2048, DownloadSpeed: 4096},
			},
			LatestCursor: 2,
		}},
	}
	model, _ := initializeShellModel(t, client)

	updated, command := model.Update(commandKey("4"))
	model = updated.(Model)
	if command == nil || model.page != pageMonitor || model.trafficRange != 5*time.Minute {
		t.Fatalf("monitor open command=%v page=%s range=%s", command != nil, model.page, model.trafficRange)
	}
	updated, tick := model.Update(command())
	model = updated.(Model)
	if tick == nil || len(client.trafficRequests) != 1 || client.trafficRequests[0].Since.IsZero() {
		t.Fatalf("initial traffic request=%#v tick=%v", client.trafficRequests, tick != nil)
	}
	view := model.View().Content
	for _, expected := range []string{
		"当前速度", "2.0 KiB/s", "4.0 KiB/s", "本次运行累计", "10.0 MiB", "20.0 MiB",
		"活动连接         7", "范围 5 分钟", "上传 ", "下载 ",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("monitor missing %q: %q", expected, view)
		}
	}
}

func TestRuntimeTUIMonitorUsesCursorForIncrementalRefresh(t *testing.T) {
	now := time.Now().UTC()
	client := &fakeClient{
		snapshot: shellSnapshot(4, 3),
		trafficHistories: []runtimeapi.TrafficHistory{
			{Samples: []runtimeapi.TrafficSample{{Cursor: 4, ObservedAt: now}}, LatestCursor: 4},
			{Samples: []runtimeapi.TrafficSample{{Cursor: 5, ObservedAt: now.Add(time.Second)}}, LatestCursor: 5},
		},
	}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	updated, _ := model.Update(model.trafficHistoryCmd(true)())
	model = updated.(Model)

	updated, command := model.Update(trafficTickMsg{})
	model = updated.(Model)
	if command == nil {
		t.Fatal("traffic tick did not request increment")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.trafficRequests) != 2 || client.trafficRequests[1].After != 4 || len(model.trafficHistory) != 2 {
		t.Fatalf("increment requests=%#v history=%#v", client.trafficRequests, model.trafficHistory)
	}
}

func TestRuntimeTUIMonitorChangesRangeAndPreservesStaleData(t *testing.T) {
	now := time.Now().UTC()
	client := &fakeClient{
		snapshot: shellSnapshot(4, 3),
		trafficHistories: []runtimeapi.TrafficHistory{{
			Status:       runtimeapi.TrafficStatus{Available: true, UploadTotal: 100},
			Samples:      []runtimeapi.TrafficSample{{Cursor: 1, ObservedAt: now, UploadTotal: 100}},
			LatestCursor: 1,
		}},
	}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	updated, _ := model.Update(model.trafficHistoryCmd(true)())
	model = updated.(Model)

	updated, command := model.Update(commandKey("]"))
	model = updated.(Model)
	if command == nil || model.trafficRange != 15*time.Minute {
		t.Fatalf("range=%s command=%v", model.trafficRange, command != nil)
	}
	client.trafficError = errors.New("traffic IPC unavailable")
	updated, _ = model.Update(command())
	model = updated.(Model)
	if !model.trafficStale || len(model.trafficHistory) != 1 || !strings.Contains(model.View().Content, "流量数据已过期") {
		t.Fatalf("stale=%t history=%#v view=%q", model.trafficStale, model.trafficHistory, model.View().Content)
	}
}

func TestRuntimeTUIMonitorStopsPollingWhenPageIsHidden(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageStatus)
	updated, command := model.Update(trafficTickMsg{})
	model = updated.(Model)
	if command != nil || model.page != pageStatus {
		t.Fatalf("hidden monitor continued polling: command=%v page=%s", command != nil, model.page)
	}
}
