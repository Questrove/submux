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

	NetworkRouteRoleTUN        = "tun_specific"
	NetworkRouteRoleGatewayLAN = "gateway_lan"
	NetworkRouteRoleGatewayWAN = "gateway_wan"
	NetworkRouteRoleDirect     = "direct"
)

type TUNSettings struct {
	IPv6Policy      string   `json:"ipv6_policy"`
	DNSPolicy       string   `json:"dns_policy"`
	CaptureRouteIDs []string `json:"capture_route_ids,omitempty"`
}

type NetworkPreviewRequest struct {
	Mode             string                    `json:"mode"`
	IPv6Policy       string                    `json:"ipv6_policy,omitempty"`
	DNSPolicy        string                    `json:"dns_policy,omitempty"`
	CaptureRouteIDs  []string                  `json:"capture_route_ids,omitempty"`
	CaptureTCP       *bool                     `json:"capture_tcp,omitempty"`
	CaptureUDP       *bool                     `json:"capture_udp,omitempty"`
	ProxyHostTraffic bool                      `json:"proxy_host_traffic,omitempty"`
	ExcludedRouteIDs []string                  `json:"excluded_route_ids,omitempty"`
	UDPExceptions    []GatewayTrafficException `json:"udp_exceptions,omitempty"`
	DNSDirectCIDRs   []string                  `json:"dns_direct_cidrs,omitempty"`
	HostExceptions   []GatewayTrafficException `json:"host_exceptions,omitempty"`
}

type NetworkPortRange struct {
	Start uint16 `json:"start"`
	End   uint16 `json:"end"`
}

type GatewayTrafficException struct {
	SourceCIDR       string             `json:"source_cidr,omitempty"`
	DestinationCIDR  string             `json:"destination_cidr,omitempty"`
	DestinationPorts []NetworkPortRange `json:"destination_ports,omitempty"`
	UID              *uint32            `json:"uid,omitempty"`
}

type GatewaySettings struct {
	IPv6Policy       string                    `json:"ipv6_policy"`
	DNSPolicy        string                    `json:"dns_policy"`
	CaptureTCP       bool                      `json:"capture_tcp"`
	CaptureUDP       bool                      `json:"capture_udp"`
	ProxyHostTraffic bool                      `json:"proxy_host_traffic"`
	ExcludedRouteIDs []string                  `json:"excluded_route_ids,omitempty"`
	UDPExceptions    []GatewayTrafficException `json:"udp_exceptions,omitempty"`
	DNSDirectCIDRs   []string                  `json:"dns_direct_cidrs,omitempty"`
	HostExceptions   []GatewayTrafficException `json:"host_exceptions,omitempty"`
}

type NetworkPreview struct {
	PlanID          string            `json:"plan_id"`
	Mode            string            `json:"mode"`
	Device          string            `json:"device"`
	Settings        TUNSettings       `json:"settings"`
	GatewaySettings *GatewaySettings  `json:"gateway_settings,omitempty"`
	Routes          []NetworkRoute    `json:"routes,omitempty"`
	Conflicts       []NetworkConflict `json:"conflicts,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
	PreviewOnly     bool              `json:"preview_only,omitempty"`
	ExpiresAt       time.Time         `json:"expires_at"`
	ObservedAt      time.Time         `json:"observed_at"`
}

type NetworkRoute struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	CIDR      string `json:"cidr"`
	Interface string `json:"interface"`
	Table     string `json:"table"`
	Source    string `json:"source"`
	Role      string `json:"role,omitempty"`
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
	Available       bool              `json:"available"`
	Mode            string            `json:"mode"`
	State           string            `json:"state"`
	Device          string            `json:"device,omitempty"`
	Settings        TUNSettings       `json:"settings"`
	GatewaySettings *GatewaySettings  `json:"gateway_settings,omitempty"`
	OwnershipID     string            `json:"ownership_id,omitempty"`
	Objects         []NetworkObject   `json:"objects,omitempty"`
	Routes          []NetworkRoute    `json:"routes,omitempty"`
	Conflicts       []NetworkConflict `json:"conflicts,omitempty"`
	Residuals       []NetworkObject   `json:"residuals,omitempty"`
	LeaseExpiresAt  *time.Time        `json:"lease_expires_at,omitempty"`
	ObservedAt      time.Time         `json:"observed_at"`
	PreviewOnly     bool              `json:"preview_only,omitempty"`
	Fault           *Fault            `json:"fault,omitempty"`
}
