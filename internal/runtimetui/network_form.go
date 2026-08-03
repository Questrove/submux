package runtimetui

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

const (
	networkFieldMode              = "mode"
	networkFieldIPv6Policy        = "ipv6_policy"
	networkFieldDNSPolicy         = "dns_policy"
	networkFieldCaptureRouteIDs   = "capture_route_ids"
	networkFieldCaptureTCP        = "capture_tcp"
	networkFieldCaptureUDP        = "capture_udp"
	networkFieldProxyHostTraffic  = "proxy_host_traffic"
	networkFieldExcludedRouteIDs  = "excluded_route_ids"
	networkFieldDNSDirectCIDRs    = "dns_direct_cidrs"
	networkFieldUDPDirectCIDRs    = "udp_direct_cidrs"
	networkFieldUDPDirectPorts    = "udp_direct_ports"
	networkFieldHostExceptionUIDs = "host_exception_uids"
)

var networkModes = []string{
	runtimeapi.RunModeExplicit,
	runtimeapi.RunModeTUN,
	runtimeapi.RunModeGateway,
}

type networkForm struct {
	mode string
	*structuredForm
}

func newNetworkForm(mode string) *networkForm {
	if !validNetworkMode(mode) {
		mode = runtimeapi.RunModeExplicit
	}
	return &networkForm{mode: mode, structuredForm: newStructuredForm(networkFields(mode, nil))}
}

func validNetworkMode(mode string) bool {
	return mode == runtimeapi.RunModeExplicit || mode == runtimeapi.RunModeTUN || mode == runtimeapi.RunModeGateway
}

func networkFields(mode string, previous map[string]string) []structuredField {
	value := func(key string, fallback string) string {
		if previous != nil && previous[key] != "" {
			return previous[key]
		}
		return fallback
	}
	fields := []structuredField{{
		key: networkFieldMode, label: "运行方式", value: mode, options: networkModes,
	}}
	if mode == runtimeapi.RunModeExplicit {
		return fields
	}
	defaultIPv6 := runtimeapi.TUNIPv6Proxy
	if mode == runtimeapi.RunModeGateway {
		defaultIPv6 = runtimeapi.TUNIPv6Direct
	}
	fields = append(fields,
		structuredField{
			key: networkFieldIPv6Policy, label: "IPv6 策略", value: value(networkFieldIPv6Policy, defaultIPv6),
			options: []string{runtimeapi.TUNIPv6Proxy, runtimeapi.TUNIPv6Direct, runtimeapi.TUNIPv6Block},
		},
		structuredField{
			key: networkFieldDNSPolicy, label: "DNS 策略", value: value(networkFieldDNSPolicy, runtimeapi.TUNDNSHijack),
			options: []string{runtimeapi.TUNDNSHijack, runtimeapi.TUNDNSOff},
		},
	)
	if mode == runtimeapi.RunModeTUN {
		return append(fields, structuredField{
			key: networkFieldCaptureRouteIDs, label: "接管路由 ID", value: value(networkFieldCaptureRouteIDs, ""),
			placeholder: "逗号分隔；留空由 Runtime 根据真实路由预览",
		})
	}
	return append(fields,
		structuredField{key: networkFieldCaptureTCP, label: "接管 TCP", value: value(networkFieldCaptureTCP, "true"), toggle: true},
		structuredField{key: networkFieldCaptureUDP, label: "接管 UDP", value: value(networkFieldCaptureUDP, "true"), toggle: true},
		structuredField{key: networkFieldProxyHostTraffic, label: "代理本机流量", value: value(networkFieldProxyHostTraffic, "false"), toggle: true},
		structuredField{key: networkFieldExcludedRouteIDs, label: "排除 LAN 路由 ID", value: value(networkFieldExcludedRouteIDs, ""), placeholder: "逗号分隔"},
		structuredField{key: networkFieldDNSDirectCIDRs, label: "DNS 直连网段", value: value(networkFieldDNSDirectCIDRs, ""), placeholder: "逗号分隔 CIDR"},
		structuredField{key: networkFieldUDPDirectCIDRs, label: "UDP 直连目的网段", value: value(networkFieldUDPDirectCIDRs, ""), placeholder: "逗号分隔 CIDR"},
		structuredField{key: networkFieldUDPDirectPorts, label: "UDP 直连端口", value: value(networkFieldUDPDirectPorts, ""), placeholder: "例如 53,443,1000-2000"},
		structuredField{key: networkFieldHostExceptionUIDs, label: "本机直连 UID", value: value(networkFieldHostExceptionUIDs, ""), placeholder: "逗号分隔数字 UID"},
	)
}

func (form *networkForm) setMode(mode string) {
	if !validNetworkMode(mode) || mode == form.mode {
		return
	}
	form.commitActiveField()
	previous := make(map[string]string, len(form.fields))
	for _, field := range form.fields {
		previous[field.key] = field.value
	}
	form.input.Blur()
	form.mode = mode
	form.structuredForm = newStructuredForm(networkFields(mode, previous))
}

func (form *networkForm) syncModeField() {
	mode := form.value(networkFieldMode)
	if mode != form.mode {
		form.setMode(mode)
	}
}

func (form *networkForm) request() (runtimeapi.NetworkPreviewRequest, error) {
	form.commitActiveField()
	if !validNetworkMode(form.mode) {
		return runtimeapi.NetworkPreviewRequest{}, errors.New("运行方式必须是显式代理、普通 TUN 或 Linux 网关")
	}
	if form.mode == runtimeapi.RunModeExplicit {
		return runtimeapi.NetworkPreviewRequest{Mode: runtimeapi.RunModeExplicit}, nil
	}
	ipv6 := form.value(networkFieldIPv6Policy)
	if ipv6 != runtimeapi.TUNIPv6Proxy && ipv6 != runtimeapi.TUNIPv6Direct && ipv6 != runtimeapi.TUNIPv6Block {
		return runtimeapi.NetworkPreviewRequest{}, errors.New("IPv6 策略必须是 proxy、direct 或 block")
	}
	dns := form.value(networkFieldDNSPolicy)
	if dns != runtimeapi.TUNDNSHijack && dns != runtimeapi.TUNDNSOff {
		return runtimeapi.NetworkPreviewRequest{}, errors.New("DNS 策略必须是 hijack 或 off")
	}
	request := runtimeapi.NetworkPreviewRequest{
		Mode:       form.mode,
		IPv6Policy: ipv6,
		DNSPolicy:  dns,
	}
	if form.mode == runtimeapi.RunModeTUN {
		request.CaptureRouteIDs = splitList(form.value(networkFieldCaptureRouteIDs))
		return request, nil
	}
	captureTCP := form.value(networkFieldCaptureTCP) == "true"
	captureUDP := form.value(networkFieldCaptureUDP) == "true"
	request.CaptureTCP = &captureTCP
	request.CaptureUDP = &captureUDP
	request.ProxyHostTraffic = form.value(networkFieldProxyHostTraffic) == "true"
	request.ExcludedRouteIDs = splitList(form.value(networkFieldExcludedRouteIDs))
	var err error
	request.DNSDirectCIDRs, err = parseCIDRList(form.value(networkFieldDNSDirectCIDRs), "DNS 直连网段")
	if err != nil {
		return runtimeapi.NetworkPreviewRequest{}, err
	}
	udpCIDRs, err := parseCIDRList(form.value(networkFieldUDPDirectCIDRs), "UDP 直连目的网段")
	if err != nil {
		return runtimeapi.NetworkPreviewRequest{}, err
	}
	udpPorts, err := parsePortRanges(form.value(networkFieldUDPDirectPorts))
	if err != nil {
		return runtimeapi.NetworkPreviewRequest{}, err
	}
	if len(udpPorts) > 0 && len(udpCIDRs) == 0 {
		return runtimeapi.NetworkPreviewRequest{}, errors.New("填写 UDP 直连端口时必须同时填写 UDP 直连目的网段")
	}
	for _, cidr := range udpCIDRs {
		request.UDPExceptions = append(request.UDPExceptions, runtimeapi.GatewayTrafficException{
			DestinationCIDR:  cidr,
			DestinationPorts: append([]runtimeapi.NetworkPortRange(nil), udpPorts...),
		})
	}
	uids, err := parseUIDs(form.value(networkFieldHostExceptionUIDs))
	if err != nil {
		return runtimeapi.NetworkPreviewRequest{}, err
	}
	for _, uid := range uids {
		uid := uid
		request.HostExceptions = append(request.HostExceptions, runtimeapi.GatewayTrafficException{UID: &uid})
	}
	return request, nil
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		if _, duplicate := seen[item]; duplicate {
			continue
		}
		seen[item] = struct{}{}
		items = append(items, item)
	}
	return items
}

func parseCIDRList(value string, label string) ([]string, error) {
	items := splitList(value)
	for index, item := range items {
		prefix, err := netip.ParsePrefix(item)
		if err != nil || prefix.String() != item {
			return nil, fmt.Errorf("%s第 %d 项不是有效 CIDR: %s", label, index+1, item)
		}
	}
	return items, nil
}

func parsePortRanges(value string) ([]runtimeapi.NetworkPortRange, error) {
	items := splitList(value)
	ranges := make([]runtimeapi.NetworkPortRange, 0, len(items))
	for index, item := range items {
		parts := strings.Split(item, "-")
		if len(parts) > 2 {
			return nil, fmt.Errorf("UDP 直连端口第 %d 项不是端口或端口范围: %s", index+1, item)
		}
		start, err := strconv.ParseUint(parts[0], 10, 16)
		if err != nil || start == 0 {
			return nil, fmt.Errorf("UDP 直连端口第 %d 项无效: %s", index+1, item)
		}
		end := start
		if len(parts) == 2 {
			end, err = strconv.ParseUint(parts[1], 10, 16)
			if err != nil || end < start {
				return nil, fmt.Errorf("UDP 直连端口第 %d 项范围无效: %s", index+1, item)
			}
		}
		ranges = append(ranges, runtimeapi.NetworkPortRange{Start: uint16(start), End: uint16(end)})
	}
	return ranges, nil
}

func parseUIDs(value string) ([]uint32, error) {
	items := splitList(value)
	uids := make([]uint32, 0, len(items))
	for index, item := range items {
		uid, err := strconv.ParseUint(item, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("本机直连 UID 第 %d 项不是有效数字: %s", index+1, item)
		}
		uids = append(uids, uint32(uid))
	}
	return uids, nil
}

func (m Model) updateNetworkForm(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.networkForm == nil {
		return m, nil
	}
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			m.networkForm.close()
			m.networkForm = nil
			m.err = nil
			m.status = "已取消运行方式设置"
			return m, nil
		case "tab", "down", "enter":
			return m, m.networkForm.move(1)
		case "shift+tab", "up":
			return m, m.networkForm.move(-1)
		case "left":
			if m.networkForm.cycleActiveOption(-1) {
				m.networkForm.syncModeField()
				return m, nil
			}
		case "right":
			if m.networkForm.cycleActiveOption(1) {
				m.networkForm.syncModeField()
				return m, nil
			}
		case " ":
			if m.networkForm.toggleActiveField() {
				return m, nil
			}
			if m.networkForm.cycleActiveOption(1) {
				m.networkForm.syncModeField()
				return m, nil
			}
		case "ctrl+s":
			return m.submitNetworkForm()
		}
	}
	if len(m.networkForm.fields) == 0 || m.networkForm.fields[m.networkForm.index].toggle ||
		len(m.networkForm.fields[m.networkForm.index].options) > 0 {
		return m, nil
	}
	var command tea.Cmd
	m.networkForm.input, command = m.networkForm.input.Update(message)
	return m, command
}

func (m Model) submitNetworkForm() (tea.Model, tea.Cmd) {
	form := m.networkForm
	if form == nil || m.busy {
		return m, nil
	}
	request, err := form.request()
	if err != nil {
		m.err = err
		m.status = err.Error()
		return m, nil
	}
	form.close()
	m.networkForm = nil
	m.err = nil
	if request.Mode == runtimeapi.RunModeExplicit {
		action := runtimeapi.Action{Kind: runtimeapi.ActionStartProxy}
		if m.snapshot.Network.Mode == runtimeapi.RunModeTUN {
			action.Kind = runtimeapi.ActionDisableTUN
		} else if m.snapshot.Network.Mode == runtimeapi.RunModeGateway {
			action.Kind = runtimeapi.ActionDisableGateway
		}
		m.prepareAction(action)
		return m, nil
	}
	m.busy = true
	m.networkPreview = runtimeapi.NetworkPreview{}
	m.status = "正在由 Runtime 读取实际网络并生成预览…"
	return m, m.previewNetworkCmd(request)
}

func (m Model) renderNetworkForm() string {
	form := m.networkForm
	if form == nil {
		return ""
	}
	lines := []string{
		titleStyle.Render("运行方式设置"),
		mutedStyle.Render("Tab / Shift+Tab 切换字段 · ←/→ 选择枚举 · Space 切换布尔值 · Ctrl+S 生成预览 · Esc 取消"),
		"",
		renderNetworkModeChoice(form.mode),
		"",
	}
	if form.mode == runtimeapi.RunModeExplicit {
		lines = append(lines,
			"显式代理不会修改默认路由或系统 DNS。",
			"监听地址和认证来自候选配置，由 Runtime 保留设置管理；此表单不能覆盖地址或路径。",
		)
	}
	start, end := m.visibleFormRange(len(form.fields), form.index)
	if start > 0 {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("… 上方还有 %d 个字段", start)))
	}
	for index := start; index < end; index++ {
		field := form.fields[index]
		if field.key == networkFieldMode {
			continue
		}
		prefix := "  "
		if index == form.index {
			prefix = "▶ "
		}
		value := field.value
		switch {
		case index == form.index && !field.toggle && len(field.options) == 0:
			value = form.input.View()
		case field.toggle:
			value = "[ ]"
			if field.value == "true" {
				value = "[x]"
			}
		case len(field.options) > 0:
			value = "[" + field.value + "]  ←/→"
		case value == "":
			value = mutedStyle.Render("未填写")
		}
		lines = append(lines, fmt.Sprintf("%s%-22s %s", prefix, field.label, value))
	}
	if end < len(form.fields) {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("… 下方还有 %d 个字段", len(form.fields)-end)))
	}
	lines = append(lines, "", renderStatus(m.status, m.err, m.busy))
	return strings.Join(lines, "\n")
}

func renderNetworkModeChoice(selected string) string {
	labels := make([]string, 0, len(networkModes))
	for _, mode := range networkModes {
		marker := "○"
		if mode == selected {
			marker = "●"
		}
		labels = append(labels, marker+" "+networkModeLabel(mode))
	}
	return "运行方式  " + strings.Join(labels, "   ")
}

func networkModeLabel(mode string) string {
	switch mode {
	case runtimeapi.RunModeExplicit:
		return "显式代理"
	case runtimeapi.RunModeTUN:
		return "普通 TUN"
	case runtimeapi.RunModeGateway:
		return "Linux 网关"
	default:
		return mode
	}
}
