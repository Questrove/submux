package runtimenet

import (
	"context"
	"encoding/json"
	"time"

	"submux/internal/runtimeapi"
)

const (
	ProtocolVersion = 3

	DefaultPlanTTL      = 5 * time.Minute
	DefaultLeaseTTL     = 10 * time.Second
	DefaultRequestLimit = 30 * time.Second
	DefaultSessionLimit = 1024

	ReleaseOperator       = "operator"
	ReleaseRuntimeExit    = "runtime_exit"
	ReleaseMihomoFailure  = "mihomo_failure"
	ReleaseLeaseExpired   = "lease_expired"
	ReleaseServiceRestart = "service_restart"
	ReleaseUpdate         = "update"

	OperationPrepare   = "prepare_network"
	OperationCommit    = "commit_network"
	OperationRenew     = "renew_lease"
	OperationRelease   = "release"
	OperationCoreStage = "core_stage"
	OperationCoreStart = "core_start"
	OperationCoreStop  = "core_stop"

	PrivilegedCoreObjectMihomo = "mihomo"
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

type PreparedNetwork struct {
	OwnershipID     string                      `json:"ownership_id"`
	Mode            string                      `json:"mode"`
	Device          string                      `json:"device"`
	RoutingMark     int                         `json:"routing_mark"`
	Settings        runtimeapi.TUNSettings      `json:"settings"`
	GatewaySettings *runtimeapi.GatewaySettings `json:"gateway_settings,omitempty"`
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
	PreviewOnly   bool
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
	CaptureMark       int
	RouteTable        int
	RulePriority      int
	Settings          runtimeapi.TUNSettings
	GatewaySettings   *runtimeapi.GatewaySettings
	Routes            []runtimeapi.NetworkRoute
	Original          map[string]string
	Objects           []runtimeapi.NetworkObject
	Residuals         []runtimeapi.NetworkObject
	PreviewOnly       bool
	State             string
	Epoch             string
	LeaseExpiresAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type SystemPreparation struct {
	Objects      []runtimeapi.NetworkObject
	RoutingMark  int
	CaptureMark  int
	RouteTable   int
	RulePriority int
}

type System interface {
	Discover(context.Context, runtimeapi.TUNSettings) (Discovery, error)
	DiscoverGateway(context.Context, runtimeapi.GatewaySettings) (Discovery, error)
	PrepareTUN(context.Context, Ownership) (SystemPreparation, error)
	PrepareGateway(context.Context, Ownership) (SystemPreparation, error)
	ApplyTUN(context.Context, Ownership) ([]runtimeapi.NetworkObject, error)
	ApplyGateway(context.Context, Ownership) ([]runtimeapi.NetworkObject, error)
	Cleanup(context.Context, Ownership) ([]runtimeapi.NetworkObject, error)
	Observe(context.Context, *Ownership) (runtimeapi.NetworkStatus, error)
}

type PrivilegedCoreStage struct {
	ObjectID     string `json:"object_id"`
	CoreSHA256   string `json:"core_sha256"`
	ConfigSHA256 string `json:"config_sha256"`
	DataObjectID string `json:"data_object_id,omitempty"`
}

type PrivilegedCoreStatus struct {
	ObjectID     string    `json:"object_id"`
	State        string    `json:"state"`
	PID          int       `json:"pid,omitempty"`
	CoreSHA256   string    `json:"core_sha256,omitempty"`
	ConfigSHA256 string    `json:"config_sha256,omitempty"`
	DataObjectID string    `json:"data_object_id,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
	PreviewOnly  bool      `json:"preview_only,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type PrivilegedCoreSystem interface {
	StageCore(context.Context, PrivilegedCoreStage) (PrivilegedCoreStatus, error)
	StartCore(context.Context, string) (PrivilegedCoreStatus, error)
	StopCore(context.Context, string) (PrivilegedCoreStatus, error)
	ObserveCore(context.Context, string) (PrivilegedCoreStatus, error)
}
