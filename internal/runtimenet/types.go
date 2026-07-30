package runtimenet

import (
	"context"
	"encoding/json"
	"time"

	"submux/internal/runtimeapi"
)

const (
	ProtocolVersion = 1

	DefaultPlanTTL      = 5 * time.Minute
	DefaultLeaseTTL     = 10 * time.Second
	DefaultRequestLimit = 30 * time.Second

	ReleaseOperator       = "operator"
	ReleaseRuntimeExit    = "runtime_exit"
	ReleaseMihomoFailure  = "mihomo_failure"
	ReleaseLeaseExpired   = "lease_expired"
	ReleaseServiceRestart = "service_restart"
	ReleaseUpdate         = "update"

	OperationPrepare = "prepare_tun"
	OperationCommit  = "commit_tun"
	OperationRenew   = "renew_lease"
	OperationRelease = "release"
)

type SessionRequest struct {
	ProtocolVersion   int    `json:"protocol_version"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	ClientNonce       string `json:"client_nonce"`
}

type Session struct {
	ProtocolVersion int       `json:"protocol_version"`
	Epoch           string    `json:"epoch"`
	ID              string    `json:"session_id"`
	ServerNonce     string    `json:"server_nonce"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type RequestMeta struct {
	Epoch           string    `json:"epoch"`
	SessionID       string    `json:"session_id"`
	ConnectionNonce string    `json:"connection_nonce"`
	Sequence        uint64    `json:"sequence"`
	OperationID     string    `json:"operation_id"`
	Deadline        time.Time `json:"deadline"`
}

type PreparedTUN struct {
	OwnershipID string                 `json:"ownership_id"`
	Device      string                 `json:"device"`
	RoutingMark int                    `json:"routing_mark"`
	Settings    runtimeapi.TUNSettings `json:"settings"`
}

type CommittedResult struct {
	Epoch             string          `json:"epoch"`
	SessionID         string          `json:"session_id"`
	RuntimeInstanceID string          `json:"runtime_instance_id"`
	Sequence          uint64          `json:"sequence"`
	OperationID       string          `json:"operation_id"`
	Operation         string          `json:"operation"`
	OwnershipID       string          `json:"ownership_id,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Error             string          `json:"error,omitempty"`
	CommittedAt       time.Time       `json:"committed_at"`
}

type Discovery struct {
	Device        string
	IPv6Available bool
	Routes        []runtimeapi.NetworkRoute
	Conflicts     []runtimeapi.NetworkConflict
	Warnings      []string
	Original      map[string]string
}

type Ownership struct {
	ID                string
	Token             string
	RuntimeInstanceID string
	OperationID       string
	Mode              string
	Device            string
	IPv6Available     bool
	RoutingMark       int
	RouteTable        int
	RulePriority      int
	Settings          runtimeapi.TUNSettings
	Routes            []runtimeapi.NetworkRoute
	Original          map[string]string
	Objects           []runtimeapi.NetworkObject
	Residuals         []runtimeapi.NetworkObject
	State             string
	Epoch             string
	LeaseExpiresAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type SystemPreparation struct {
	Objects      []runtimeapi.NetworkObject
	RoutingMark  int
	RouteTable   int
	RulePriority int
}

type System interface {
	Discover(context.Context, runtimeapi.TUNSettings) (Discovery, error)
	PrepareTUN(context.Context, Ownership) (SystemPreparation, error)
	ApplyTUN(context.Context, Ownership) ([]runtimeapi.NetworkObject, error)
	Cleanup(context.Context, Ownership) ([]runtimeapi.NetworkObject, error)
	Observe(context.Context, *Ownership) (runtimeapi.NetworkStatus, error)
}
