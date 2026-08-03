package runtimeapi

import "time"

const (
	ConnectionPageDefaultSize  = 20
	ConnectionPageMaxSize      = 100
	ConnectionResponseMaxBytes = 1 << 20
)

type Connection struct {
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	Target          string    `json:"target"`
	Protocol        string    `json:"protocol"`
	Inbound         string    `json:"inbound,omitempty"`
	Process         string    `json:"process,omitempty"`
	Rule            string    `json:"rule,omitempty"`
	RulePayload     string    `json:"rule_payload,omitempty"`
	OutboundChain   []string  `json:"outbound_chain,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	DurationSeconds int64     `json:"duration_seconds"`
	Upload          uint64    `json:"upload"`
	Download        uint64    `json:"download"`
}

type ConnectionQuery struct {
	Target   string
	Process  string
	Rule     string
	Node     string
	Page     int
	PageSize int
}

type ConnectionPage struct {
	Items      []Connection `json:"items,omitempty"`
	Total      int          `json:"total"`
	Page       int          `json:"page"`
	PageSize   int          `json:"page_size"`
	Available  bool         `json:"available"`
	ObservedAt time.Time    `json:"observed_at"`
}
