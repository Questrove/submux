package runtimetui

import (
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestRuntimeTUINetworkModeUsesStructuredForm(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		RunMode:         runtimeapi.RunModeExplicit,
		Network: runtimeapi.NetworkStatus{
			Available: true,
			Mode:      runtimeapi.RunModeExplicit,
			State:     runtimeapi.NetworkStateInactive,
		},
	}}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(ctrlKey('t'))
	model = updated.(Model)
	if model.networkForm == nil {
		t.Fatal("TUN shortcut did not open a structured network form")
	}
	view := model.View().Content
	for _, text := range []string{"运行方式", "普通 TUN", "IPv6 策略", "DNS 策略", "接管路由 ID"} {
		if !strings.Contains(view, text) {
			t.Fatalf("network form missing %q: %q", text, view)
		}
	}
	if strings.Contains(view, `"ipv6_policy"`) || strings.Contains(view, `"dns_policy"`) {
		t.Fatalf("network form still exposes JSON editing: %q", view)
	}

	model.networkForm.setValue(networkFieldIPv6Policy, runtimeapi.TUNIPv6Direct)
	model.networkForm.setValue(networkFieldDNSPolicy, runtimeapi.TUNDNSOff)
	model.networkForm.setValue(networkFieldCaptureRouteIDs, "route_lan, route_work")
	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil || model.networkForm != nil {
		t.Fatalf("network preview submit command=%v form=%#v", command != nil, model.networkForm)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.networkRequests) != 1 {
		t.Fatalf("network preview requests=%#v", client.networkRequests)
	}
	request := client.networkRequests[0]
	if request.Mode != runtimeapi.RunModeTUN || request.IPv6Policy != runtimeapi.TUNIPv6Direct ||
		request.DNSPolicy != runtimeapi.TUNDNSOff || len(request.CaptureRouteIDs) != 2 {
		t.Fatalf("TUN request=%#v", request)
	}
}

func TestRuntimeTUINetworkFormValidatesEnumsAndCIDRs(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	updated, _ := model.Update(ctrlKey('l'))
	model = updated.(Model)
	model.networkForm.setValue(networkFieldIPv6Policy, "sometimes")

	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command != nil || !strings.Contains(model.status, "IPv6 策略") {
		t.Fatalf("invalid enum command=%v status=%q", command != nil, model.status)
	}

	model.networkForm.setValue(networkFieldIPv6Policy, runtimeapi.TUNIPv6Block)
	model.networkForm.setValue(networkFieldDNSDirectCIDRs, "10.0.0.53/32, not-a-cidr")
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command != nil || !strings.Contains(model.status, "DNS 直连网段") || !strings.Contains(model.status, "有效 CIDR") {
		t.Fatalf("invalid CIDR command=%v status=%q", command != nil, model.status)
	}
}

func TestRuntimeTUINetworkFormRejectsPortsWithoutDestinationCIDR(t *testing.T) {
	form := newNetworkForm(runtimeapi.RunModeGateway)
	form.setValue(networkFieldUDPDirectPorts, "443,1000-2000")

	_, err := form.request()
	if err == nil || !strings.Contains(err.Error(), "必须同时填写 UDP 直连目的网段") {
		t.Fatalf("ports without destination CIDR error=%v", err)
	}
}

func TestRuntimeTUINetworkModeSelectionIsExclusive(t *testing.T) {
	form := newNetworkForm(runtimeapi.RunModeExplicit)
	for _, mode := range []string{runtimeapi.RunModeExplicit, runtimeapi.RunModeTUN, runtimeapi.RunModeGateway} {
		form.setMode(mode)
		view := renderNetworkModeChoice(form.mode)
		if strings.Count(view, "●") != 1 || !strings.Contains(view, networkModeLabel(mode)) {
			t.Fatalf("exclusive mode %q view=%q", mode, view)
		}
	}
}

func TestRuntimeTUIExplicitModeUsesUnifiedOperationWithoutNetworkTakeover(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		RunMode:         runtimeapi.RunModeExplicit,
		Network: runtimeapi.NetworkStatus{
			Available: true,
			Mode:      runtimeapi.RunModeExplicit,
			State:     runtimeapi.NetworkStateInactive,
		},
	}}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(ctrlKey('p'))
	model = updated.(Model)
	if model.networkForm == nil || model.networkForm.mode != runtimeapi.RunModeExplicit {
		t.Fatalf("explicit form=%#v", model.networkForm)
	}
	if view := model.View().Content; !strings.Contains(view, "不能覆盖地址或路径") {
		t.Fatalf("explicit Runtime-owned field guidance missing: %q", view)
	}
	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionStartProxy {
		t.Fatalf("explicit selection command=%v confirmation=%#v", command != nil, model.confirmation)
	}
	if len(client.networkRequests) != 0 || len(client.actions) != 0 {
		t.Fatalf("explicit selection changed state before confirmation: previews=%d actions=%d", len(client.networkRequests), len(client.actions))
	}
}

func TestRuntimeTUIExplicitModeStopDoesNotSubmitTUNDisable(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		RunMode:         runtimeapi.RunModeExplicit,
		Network: runtimeapi.NetworkStatus{
			Mode:  runtimeapi.RunModeExplicit,
			State: runtimeapi.NetworkStateInactive,
		},
	}}
	model, _ := initializeShellModel(t, client)

	updated, command := model.Update(ctrlKey('x'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionStopProxy {
		t.Fatalf("explicit stop command=%v confirmation=%#v", command != nil, model.confirmation)
	}
}

func TestRuntimeTUIPreviewShowsActualExpectedDNSResidualsAndRecovery(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		RunMode:         runtimeapi.RunModeExplicit,
		Network: runtimeapi.NetworkStatus{
			Available: true,
			Mode:      runtimeapi.RunModeExplicit,
			State:     runtimeapi.NetworkStateInactive,
			Residuals: []runtimeapi.NetworkObject{{Kind: "route", Name: "stale-route", State: runtimeapi.NetworkStateUnknown}},
		},
	}}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageNetwork)
	model.networkPreview = runtimeapi.NetworkPreview{
		PlanID:     "plan_test",
		Mode:       runtimeapi.RunModeTUN,
		Device:     "smxtun0",
		Settings:   runtimeapi.TUNSettings{IPv6Policy: runtimeapi.TUNIPv6Proxy, DNSPolicy: runtimeapi.TUNDNSHijack},
		ObservedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Minute),
		GatewaySettings: &runtimeapi.GatewaySettings{
			DNSPolicy:      runtimeapi.TUNDNSHijack,
			DNSDirectCIDRs: []string{"10.0.0.53/32"},
			UDPExceptions: []runtimeapi.GatewayTrafficException{{
				SourceCIDR:      "10.0.0.0/24",
				DestinationCIDR: "203.0.113.0/24",
				DestinationPorts: []runtimeapi.NetworkPortRange{
					{Start: 443, End: 443},
					{Start: 1000, End: 2000},
				},
			}},
			HostExceptions: []runtimeapi.GatewayTrafficException{{UID: uint32Pointer(1000)}},
		},
	}

	view := model.View().Content
	for _, text := range []string{
		"当前实际  explicit · inactive",
		"预期结果  tun · active · smxtun0",
		"DNS 变化  hijack",
		"DNS 直连网段 · 10.0.0.53/32",
		"UDP 直连例外 1 · 来源 10.0.0.0/24 · 目标 203.0.113.0/24 · 端口 443,1000-2000",
		"本机直连例外 1 · UID 1000",
		"stale-route",
		"恢复动作",
		"失败时撤销 Runtime 创建的网络对象并恢复直连",
	} {
		if !strings.Contains(view, text) {
			t.Fatalf("network preview missing %q: %q", text, view)
		}
	}
}

func uint32Pointer(value uint32) *uint32 {
	return &value
}

func TestRuntimeTUIPreviewOnlyPlanStillRequiresUnifiedConfirmation(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.networkPreview = runtimeapi.NetworkPreview{
		PlanID:      "plan_preview_only",
		Mode:        runtimeapi.RunModeGateway,
		PreviewOnly: true,
		ExpiresAt:   time.Now().Add(time.Minute),
	}

	updated, command := model.Update(ctrlKey('e'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil || model.confirmation.Action.Params.PlanID != "plan_preview_only" {
		t.Fatalf("preview-only apply command=%v confirmation=%#v status=%q", command != nil, model.confirmation, model.status)
	}
}
