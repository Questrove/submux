package runtimeapi

import "time"

const (
	RunModeUnconfigured = "unconfigured"
	RunModeExplicit     = "explicit"
	RunModeTUN          = "tun"
	RunModeGateway      = "gateway"

	NetworkStateUnavailable = "unavailable"
	NetworkStateInactive    = "inactive"
	NetworkStatePrepared    = "prepared"
	NetworkStateActive      = "active"
	NetworkStateReleasing   = "releasing"
	NetworkStateConflict    = "conflict"
	NetworkStateUnknown     = "unknown"

	TUNIPv6Proxy  = "proxy"
	TUNIPv6Direct = "direct"
	TUNIPv6Block  = "block"

	TUNDNSHijack = "hijack"
	TUNDNSOff    = "off"
)

type TUNSettings struct {
	IPv6Policy      string   `json:"ipv6_policy"`
	DNSPolicy       string   `json:"dns_policy"`
	CaptureRouteIDs []string `json:"capture_route_ids,omitempty"`
}

type NetworkPreviewRequest struct {
	Mode            string   `json:"mode"`
	IPv6Policy      string   `json:"ipv6_policy,omitempty"`
	DNSPolicy       string   `json:"dns_policy,omitempty"`
	CaptureRouteIDs []string `json:"capture_route_ids,omitempty"`
}

type NetworkPreview struct {
	PlanID     string            `json:"plan_id"`
	Mode       string            `json:"mode"`
	Device     string            `json:"device"`
	Settings   TUNSettings       `json:"settings"`
	Routes     []NetworkRoute    `json:"routes,omitempty"`
	Conflicts  []NetworkConflict `json:"conflicts,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
	ExpiresAt  time.Time         `json:"expires_at"`
	ObservedAt time.Time         `json:"observed_at"`
}

type NetworkRoute struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	CIDR      string `json:"cidr"`
	Interface string `json:"interface"`
	Table     string `json:"table"`
	Source    string `json:"source"`
	Bypass    bool   `json:"bypass"`
}

type NetworkConflict struct {
	Kind   string `json:"kind"`
	Owner  string `json:"owner,omitempty"`
	Detail string `json:"detail"`
}

type NetworkObject struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type NetworkStatus struct {
	Available      bool              `json:"available"`
	Mode           string            `json:"mode"`
	State          string            `json:"state"`
	Device         string            `json:"device,omitempty"`
	Settings       TUNSettings       `json:"settings"`
	OwnershipID    string            `json:"ownership_id,omitempty"`
	Objects        []NetworkObject   `json:"objects,omitempty"`
	Routes         []NetworkRoute    `json:"routes,omitempty"`
	Conflicts      []NetworkConflict `json:"conflicts,omitempty"`
	Residuals      []NetworkObject   `json:"residuals,omitempty"`
	LeaseExpiresAt *time.Time        `json:"lease_expires_at,omitempty"`
	ObservedAt     time.Time         `json:"observed_at"`
	Fault          *Fault            `json:"fault,omitempty"`
}
