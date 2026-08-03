package runtimeapi

import "time"

const (
	TrafficHistoryRetention  = 15 * time.Minute
	TrafficHistoryMaxSamples = 1024
)

type TrafficStatus struct {
	Available         bool      `json:"available"`
	UploadSpeed       uint64    `json:"upload_speed"`
	DownloadSpeed     uint64    `json:"download_speed"`
	UploadTotal       uint64    `json:"upload_total"`
	DownloadTotal     uint64    `json:"download_total"`
	ActiveConnections int       `json:"active_connections"`
	ObservedAt        time.Time `json:"observed_at"`
}

type TrafficSample struct {
	Cursor            uint64    `json:"cursor"`
	ObservedAt        time.Time `json:"observed_at"`
	UploadSpeed       uint64    `json:"upload_speed"`
	DownloadSpeed     uint64    `json:"download_speed"`
	UploadTotal       uint64    `json:"upload_total"`
	DownloadTotal     uint64    `json:"download_total"`
	ActiveConnections int       `json:"active_connections"`
	Discontinuity     bool      `json:"discontinuity,omitempty"`
}

type TrafficHistoryRequest struct {
	After uint64
	Since time.Time
	Limit int
}

type TrafficHistory struct {
	Status         TrafficStatus   `json:"status"`
	Samples        []TrafficSample `json:"samples,omitempty"`
	EarliestCursor uint64          `json:"earliest_cursor,omitempty"`
	LatestCursor   uint64          `json:"latest_cursor,omitempty"`
	ResetRequired  bool            `json:"reset_required,omitempty"`
}
