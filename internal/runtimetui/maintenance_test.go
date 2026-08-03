package runtimetui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestMaintenancePageShowsStructuredPlansRestoreDiagnosticsAndOperation(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(8, 5)})
	model.openPage(pageMaintenance)
	model.mihomoUpdate = runtimeapi.MihomoUpdatePlan{
		PlanID: "plan_mihomo", Source: "tuf", Trust: "tuf", Version: "1.19.29", CurrentVersion: "1.19.28",
		StaticConfigVerified: true, AssetSHA256: strings.Repeat("a", 64), Repository: "MetaCubeX/mihomo",
	}
	model.productUpdate = runtimeapi.ProductUpdatePlan{
		PlanID: "plan_runtime", Source: "tuf", Trust: "tuf", Version: "2.1.0", CurrentVersion: "2.0.2",
		Installable: true, AssetSHA256: strings.Repeat("b", 64), NetworkInterruption: "Runtime 将短暂重启",
		Migration: runtimeapi.ProductUpdateMigration{CurrentSchema: 1, TargetSchema: 1, Reversible: true},
	}
	model.backupRestore = runtimeapi.BackupRestorePreview{
		ContentID: "content_backup", FormatVersion: 1, Restorable: true, SourceCount: 2, ManagedResourceCount: 3,
		HasAdvancedOverride: true, RecentConfigurationCount: 4,
		Compatibility: runtimeapi.BackupRestoreCompatibility{Compatible: true, RuntimeProtocol: runtimeapi.ProtocolVersion, PortableStateSchema: 1},
		Sources:       []runtimeapi.BackupRestoreSource{{ID: "src_main", Name: "主订阅", Type: runtimeapi.SourceTypeRemoteHTTP, Current: true}},
		ManagedResources: []runtimeapi.BackupRestoreResource{{
			ID: "res_key", Name: "gateway.pem", Kind: runtimeapi.ResourceKindPrivateKey, Size: 128, SHA256: strings.Repeat("d", 64),
		}},
		RecentConfigurations: []runtimeapi.BackupRestoreEntry{{Name: "current/config.yaml", Size: 256, SHA256: strings.Repeat("e", 64)}},
		Covered:              []string{"configuration_sources", "managed_resources"},
		Excluded:             []string{"logs", "operations"},
		MachineSettings:      map[string]string{"run_mode": "tun"}, PendingSettings: []string{"run_mode"},
	}
	model.diagnostics = runtimeapi.DiagnosticsPreview{
		Items: []runtimeapi.DiagnosticItem{
			{Name: "snapshot.json", Included: true, Size: 10},
			{Name: "current-config.yaml", Included: true, Sensitive: true, Size: 20},
		},
		Warning: "完整配置需要额外确认",
	}
	model.lastOperation = runtimeapi.Operation{
		ID: "op_restore", State: runtimeapi.OperationFailed, Stage: "restoring", Progress: 70,
		Action:    runtimeapi.Action{Kind: runtimeapi.ActionRestoreBackup},
		Error:     &runtimeapi.ProtocolError{Code: runtimeapi.ErrorServiceUnavailable, Message: "恢复校验失败", Retryable: true},
		Result:    &runtimeapi.OperationResult{AutomaticBackupFile: "before-restore.zip", BackupSHA256: strings.Repeat("c", 64)},
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
	}

	view := model.View().Content
	for _, expected := range []string{
		"Mihomo 回滚 · 来源 本机保留的已验证版本", "校验 回滚后执行 Mihomo 运行状态和健康检查", "Runtime 回滚 · 来源 本机已验证的产品恢复点",
		"Mihomo 目标 1.19.28 → 1.19.29", "来源 tuf · 信任 tuf", "校验 静态配置通过",
		"Runtime 目标 2.0.2 → 2.1.0", "Runtime 将短暂重启", "恢复 可回滚程序与 Runtime 数据库",
		"格式 1 · 可恢复 是", "2 个来源 · 3 个托管资源 · 4 份近期配置", "高级覆盖 包含", "run_mode = tun",
		"兼容性 通过", "来源 主订阅 · remote_http · 当前来源", "托管资源 gateway.pem", "近期配置 current/config.yaml", "覆盖 configuration_sources", "不覆盖 logs、operations",
		"snapshot.json · 包含 · 已脱敏", "current-config.yaml · 包含 · 敏感", "完整配置需要额外确认",
		"op_restore · failed · restoring · 70%", "Enter 查看阶段、错误、恢复和产物详情",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("maintenance page missing %q:\n%s", expected, view)
		}
	}

	model.focusIndex = 3
	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if command != nil {
		t.Fatal("opening operation details returned a command")
	}
	model = updated.(Model)
	detail := model.View().Content
	for _, expected := range []string{"运行操作详情", "阶段 restoring · 70%", "错误 service_unavailable · 恢复校验失败", "恢复方式", "before-restore.zip", shortDigest(strings.Repeat("c", 64))} {
		if !strings.Contains(detail, expected) {
			t.Fatalf("operation detail missing %q:\n%s", expected, detail)
		}
	}
}

func TestSensitiveDiagnosticsUsesSeparatePreviewAndExplicitConfirmation(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(8, 5)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMaintenance)
	model.focusIndex = 2

	updated, command := model.Update(keyPress('G'))
	model = updated.(Model)
	if command == nil || len(client.diagnosticCreateRequests) != 0 {
		t.Fatal("sensitive diagnostics did not start with preview")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.diagnosticPreviewRequests) != 1 {
		t.Fatalf("preview requests=%#v", client.diagnosticPreviewRequests)
	}
	preview := client.diagnosticPreviewRequests[0]
	if !preview.IncludeRawConfig || !preview.IncludeFullLogs || !preview.IncludeNetworkInfo || preview.ConfirmSensitive {
		t.Fatalf("preview request=%#v", preview)
	}
	if model.sensitiveConfirm != "diagnostics-full" {
		t.Fatalf("sensitive confirmation=%q", model.sensitiveConfirm)
	}

	updated, command = model.Update(keyPress('G'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed sensitive diagnostics did not start creation")
	}
	updated, _ = model.Update(command())
	_ = updated.(Model)
	if len(client.diagnosticCreateRequests) != 1 || !client.diagnosticCreateRequests[0].ConfirmSensitive ||
		!client.diagnosticCreateRequests[0].IncludeRawConfig || !client.diagnosticCreateRequests[0].IncludeFullLogs ||
		!client.diagnosticCreateRequests[0].IncludeNetworkInfo {
		t.Fatalf("create requests=%#v", client.diagnosticCreateRequests)
	}
}

func TestFailedMaintenanceOperationDoesNotBlockBrowsingOrSafeRecoveryPreview(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(8, 5)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMaintenance)
	model.lastOperation = runtimeapi.Operation{
		ID: "op_failed", State: runtimeapi.OperationFailed, Stage: "restoring", Progress: 60,
		Action: runtimeapi.Action{Kind: runtimeapi.ActionRestoreBackup},
		Error:  &runtimeapi.ProtocolError{Code: runtimeapi.ErrorServiceUnavailable, Message: "恢复失败", Retryable: true},
	}

	updated, command := model.Update(keyPress('2'))
	model = updated.(Model)
	if command != nil || model.page != pageConfig {
		t.Fatalf("failed maintenance operation blocked browsing: page=%s command=%v", model.page, command != nil)
	}
	updated, _ = model.Update(keyPress('5'))
	model = updated.(Model)
	model.focusIndex = 1
	updated, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command == nil {
		t.Fatal("failed maintenance operation blocked non-conflicting backup preview")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.backupPreviews != 1 || len(model.backupPreview.Items) == 0 {
		t.Fatalf("safe recovery preview did not complete: calls=%d preview=%#v", client.backupPreviews, model.backupPreview)
	}
}
