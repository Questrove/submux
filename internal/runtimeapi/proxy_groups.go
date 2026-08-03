package runtimeapi

import "time"

const (
	ProxyLatencyScopeNode   = "node"
	ProxyLatencyScopeGroup  = "group"
	ProxyLatencyScopeSource = "source"
)

const ProxyGroupNameMaxLength = 256

type ProxyGroupQuery struct {
	SourceID string `json:"source_id"`
}

type ProxyGroupList struct {
	SourceID      string             `json:"source_id"`
	CurrentSource bool               `json:"current_source"`
	Available     bool               `json:"available"`
	Message       string             `json:"message,omitempty"`
	Groups        []ProxyGroupStatus `json:"groups"`
	ObservedAt    time.Time          `json:"observed_at"`
}

type ProxyGroupStatus struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Main       bool              `json:"main"`
	Selectable bool              `json:"selectable"`
	Current    string            `json:"current,omitempty"`
	Nodes      []ProxyNodeStatus `json:"nodes"`
}

type ProxyNodeStatus struct {
	Name              string     `json:"name"`
	Type              string     `json:"type,omitempty"`
	Available         bool       `json:"available"`
	UnavailableReason string     `json:"unavailable_reason,omitempty"`
	DelayMillis       int        `json:"delay_millis,omitempty"`
	DelayTestedAt     *time.Time `json:"delay_tested_at,omitempty"`
	DelayFailure      string     `json:"delay_failure,omitempty"`
	DelayStale        bool       `json:"delay_stale,omitempty"`
}
