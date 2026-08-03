package runtimeapi

import "time"

const (
	LogPageDefaultSize = 200
	LogPageMaxSize     = 1000

	LogComponentRuntime = "runtime"
	LogComponentMihomo  = "mihomo"
	LogComponentNetwork = "network"

	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

type LogQuery struct {
	Before    uint64    `json:"before,omitempty"`
	After     uint64    `json:"after,omitempty"`
	Limit     int       `json:"limit,omitempty"`
	Text      string    `json:"text,omitempty"`
	Level     string    `json:"level,omitempty"`
	Component string    `json:"component,omitempty"`
	Since     time.Time `json:"since,omitempty"`
	Until     time.Time `json:"until,omitempty"`
}

type LogEntry struct {
	Cursor    uint64    `json:"cursor"`
	At        time.Time `json:"at"`
	Component string    `json:"component"`
	Stream    string    `json:"stream"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

type LogPage struct {
	Items          []LogEntry `json:"items"`
	EarliestCursor uint64     `json:"earliest_cursor,omitempty"`
	LatestCursor   uint64     `json:"latest_cursor,omitempty"`
	HasOlder       bool       `json:"has_older"`
	HasNewer       bool       `json:"has_newer"`
	ResetRequired  bool       `json:"reset_required,omitempty"`
	ObservedAt     time.Time  `json:"observed_at"`
}
