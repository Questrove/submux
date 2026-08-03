package runtimetraffic

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeapi"
)

const DefaultPollInterval = time.Second

type Reading struct {
	UploadTotal       uint64
	DownloadTotal     uint64
	ActiveConnections int
	Connections       []runtimeapi.Connection
}

type Reader interface {
	ReadTraffic(context.Context) (Reading, error)
}

type Collector struct {
	Reader   Reader
	Now      func() time.Time
	Interval time.Duration

	mu              sync.RWMutex
	status          runtimeapi.TrafficStatus
	samples         []runtimeapi.TrafficSample
	latestCursor    uint64
	previous        Reading
	previousAt      time.Time
	hasPrevious     bool
	discontinuityUp bool
	connections     []runtimeapi.Connection
	connectionsAt   time.Time
}

func (c *Collector) Connections(query runtimeapi.ConnectionQuery) runtimeapi.ConnectionPage {
	if c == nil {
		return runtimeapi.ConnectionPage{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	page := query.Page
	if page <= 0 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize <= 0 || pageSize > runtimeapi.ConnectionPageMaxSize {
		pageSize = runtimeapi.ConnectionPageDefaultSize
	}
	items := make([]runtimeapi.Connection, 0, len(c.connections))
	for _, connection := range c.connections {
		if !connectionMatches(connection, query) {
			continue
		}
		clone := connection
		clone.OutboundChain = append([]string(nil), connection.OutboundChain...)
		if !clone.StartedAt.IsZero() && !c.connectionsAt.IsZero() {
			clone.DurationSeconds = max(0, int64(c.connectionsAt.Sub(clone.StartedAt).Seconds()))
		}
		items = append(items, clone)
	}
	response := runtimeapi.ConnectionPage{
		Total:      len(items),
		Page:       page,
		PageSize:   pageSize,
		Available:  c.status.Available,
		ObservedAt: c.connectionsAt,
	}
	start := (page - 1) * pageSize
	if start >= len(items) {
		return response
	}
	end := min(len(items), start+pageSize)
	response.Items = items[start:end]
	return response
}

func (c *Collector) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Runtime traffic collector context is required")
	}
	if c == nil || c.Reader == nil {
		return errors.New("Runtime traffic reader is required")
	}
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if err := c.collect(ctx); err != nil && ctx.Err() != nil {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = c.collect(ctx)
		}
	}
}

func (c *Collector) Status() runtimeapi.TrafficStatus {
	if c == nil {
		return runtimeapi.TrafficStatus{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *Collector) History(request runtimeapi.TrafficHistoryRequest) runtimeapi.TrafficHistory {
	if c == nil {
		return runtimeapi.TrafficHistory{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	limit := request.Limit
	if limit <= 0 || limit > runtimeapi.TrafficHistoryMaxSamples {
		limit = runtimeapi.TrafficHistoryMaxSamples
	}
	response := runtimeapi.TrafficHistory{Status: c.status, LatestCursor: c.latestCursor}
	if len(c.samples) == 0 {
		return response
	}
	response.EarliestCursor = c.samples[0].Cursor
	if request.After > 0 && request.After < response.EarliestCursor-1 {
		response.ResetRequired = true
		request.After = 0
	}
	for _, sample := range c.samples {
		if request.After > 0 && sample.Cursor <= request.After {
			continue
		}
		if !request.Since.IsZero() && sample.ObservedAt.Before(request.Since) {
			continue
		}
		response.Samples = append(response.Samples, sample)
	}
	if len(response.Samples) > limit {
		response.Samples = append([]runtimeapi.TrafficSample(nil), response.Samples[len(response.Samples)-limit:]...)
	}
	return response
}

func (c *Collector) collect(ctx context.Context) error {
	reading, err := c.Reader.ReadTraffic(ctx)
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.hasPrevious && !c.discontinuityUp {
			c.appendLocked(runtimeapi.TrafficSample{ObservedAt: now, Discontinuity: true})
		}
		c.status.Available = false
		c.status.UploadSpeed = 0
		c.status.DownloadSpeed = 0
		c.status.ActiveConnections = 0
		c.status.ObservedAt = now
		c.hasPrevious = false
		c.discontinuityUp = true
		c.pruneLocked(now)
		return err
	}
	uploadSpeed, downloadSpeed := uint64(0), uint64(0)
	discontinuity := false
	if c.hasPrevious {
		if reading.UploadTotal < c.previous.UploadTotal || reading.DownloadTotal < c.previous.DownloadTotal {
			discontinuity = true
		} else if elapsed := now.Sub(c.previousAt); elapsed > 0 {
			uploadSpeed = bytesPerSecond(reading.UploadTotal-c.previous.UploadTotal, elapsed)
			downloadSpeed = bytesPerSecond(reading.DownloadTotal-c.previous.DownloadTotal, elapsed)
		}
	} else if c.discontinuityUp && len(c.samples) > 0 && !c.samples[len(c.samples)-1].Discontinuity {
		discontinuity = true
	}
	if discontinuity {
		c.appendLocked(runtimeapi.TrafficSample{ObservedAt: now, Discontinuity: true})
	}
	c.status = runtimeapi.TrafficStatus{
		Available:         true,
		UploadSpeed:       uploadSpeed,
		DownloadSpeed:     downloadSpeed,
		UploadTotal:       reading.UploadTotal,
		DownloadTotal:     reading.DownloadTotal,
		ActiveConnections: reading.ActiveConnections,
		ObservedAt:        now,
	}
	c.connections = cloneConnections(reading.Connections)
	c.connectionsAt = now
	c.appendLocked(runtimeapi.TrafficSample{
		ObservedAt:        now,
		UploadSpeed:       uploadSpeed,
		DownloadSpeed:     downloadSpeed,
		UploadTotal:       reading.UploadTotal,
		DownloadTotal:     reading.DownloadTotal,
		ActiveConnections: reading.ActiveConnections,
	})
	c.previous = reading
	c.previousAt = now
	c.hasPrevious = true
	c.discontinuityUp = false
	c.pruneLocked(now)
	return nil
}

func connectionMatches(connection runtimeapi.Connection, query runtimeapi.ConnectionQuery) bool {
	contains := func(value string, filter string) bool {
		return filter == "" || strings.Contains(strings.ToLower(value), strings.ToLower(filter))
	}
	if !contains(connection.Target, query.Target) || !contains(connection.Process, query.Process) ||
		(!contains(connection.Rule, query.Rule) && !contains(connection.RulePayload, query.Rule)) {
		return false
	}
	if query.Node != "" {
		matched := false
		for _, node := range connection.OutboundChain {
			if contains(node, query.Node) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func cloneConnections(source []runtimeapi.Connection) []runtimeapi.Connection {
	clones := make([]runtimeapi.Connection, len(source))
	for index, connection := range source {
		clones[index] = connection
		clones[index].OutboundChain = append([]string(nil), connection.OutboundChain...)
	}
	return clones
}

func (c *Collector) appendLocked(sample runtimeapi.TrafficSample) {
	c.latestCursor++
	sample.Cursor = c.latestCursor
	c.samples = append(c.samples, sample)
}

func (c *Collector) pruneLocked(now time.Time) {
	cutoff := now.Add(-runtimeapi.TrafficHistoryRetention)
	first := 0
	for first < len(c.samples) && c.samples[first].ObservedAt.Before(cutoff) {
		first++
	}
	if first > 0 {
		c.samples = append([]runtimeapi.TrafficSample(nil), c.samples[first:]...)
	}
	if overflow := len(c.samples) - runtimeapi.TrafficHistoryMaxSamples; overflow > 0 {
		c.samples = append([]runtimeapi.TrafficSample(nil), c.samples[overflow:]...)
	}
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func bytesPerSecond(bytes uint64, elapsed time.Duration) uint64 {
	return uint64(float64(bytes) / elapsed.Seconds())
}
