package runtimetui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

var trafficRanges = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

func (m Model) trafficHistoryCmd(initial bool) tea.Cmd {
	client, ok := m.client.(TrafficClient)
	rangeAt := m.trafficRange
	if !ok {
		return func() tea.Msg {
			return trafficHistoryErrorMsg{err: errors.New("Runtime 不支持流量历史接口"), rangeAt: rangeAt}
		}
	}
	request := runtimeapi.TrafficHistoryRequest{Limit: runtimeapi.TrafficHistoryMaxSamples}
	if initial || m.trafficCursor == 0 {
		request.Since = time.Now().UTC().Add(-rangeAt)
		initial = true
	} else {
		request.After = m.trafficCursor
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		history, err := client.TrafficHistory(ctx, request)
		if err != nil {
			return trafficHistoryErrorMsg{err: err, rangeAt: rangeAt}
		}
		return trafficHistoryMsg{history: history, rangeAt: rangeAt, initial: initial}
	}
}

func trafficTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return trafficTickMsg{} })
}

func (m *Model) cycleTrafficRange(forward bool) {
	index := 0
	for current, value := range trafficRanges {
		if value == m.trafficRange {
			index = current
			break
		}
	}
	if forward {
		index = (index + 1) % len(trafficRanges)
	} else {
		index = (index + len(trafficRanges) - 1) % len(trafficRanges)
	}
	m.trafficRange = trafficRanges[index]
	m.trafficCursor = 0
	m.trafficStale = false
	m.trafficFault = nil
	m.status = "流量曲线范围已切换为 " + trafficRangeLabel(m.trafficRange)
}

func appendTrafficSamples(existing []runtimeapi.TrafficSample, incoming []runtimeapi.TrafficSample) []runtimeapi.TrafficSample {
	for _, sample := range incoming {
		if len(existing) > 0 && sample.Cursor <= existing[len(existing)-1].Cursor {
			continue
		}
		existing = append(existing, sample)
	}
	if len(existing) == 0 {
		return existing
	}
	cutoff := existing[len(existing)-1].ObservedAt.Add(-runtimeapi.TrafficHistoryRetention)
	first := 0
	for first < len(existing) && existing[first].ObservedAt.Before(cutoff) {
		first++
	}
	if first > 0 {
		existing = append([]runtimeapi.TrafficSample(nil), existing[first:]...)
	}
	if overflow := len(existing) - runtimeapi.TrafficHistoryMaxSamples; overflow > 0 {
		existing = append([]runtimeapi.TrafficSample(nil), existing[overflow:]...)
	}
	return existing
}

func trafficRangeLabel(value time.Duration) string {
	switch value {
	case time.Minute:
		return "1 分钟"
	case 5 * time.Minute:
		return "5 分钟"
	case 15 * time.Minute:
		return "15 分钟"
	default:
		return value.String()
	}
}
