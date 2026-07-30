package runtimetui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"submux/internal/runtimeapi"
	"submux/internal/runtimebackupfile"
	"submux/internal/runtimeprivacy"
)

type Client interface {
	Observe(context.Context) (runtimeapi.Snapshot, error)
	UploadImport(context.Context, string, []byte) (runtimeapi.ImportContent, error)
	GetAdvancedOverride(context.Context, bool) (runtimeapi.AdvancedOverrideDocument, error)
	PreviewCandidate(context.Context, string) (runtimeapi.CandidatePreview, error)
	PreviewCandidateRequest(context.Context, runtimeapi.PreviewCandidateRequest) (runtimeapi.CandidatePreview, error)
	Execute(context.Context, runtimeapi.CreateOperationRequest) (runtimeapi.Operation, error)
	GetOperation(context.Context, string) (runtimeapi.Operation, error)
	WaitOperation(context.Context, string, time.Duration) (runtimeapi.Operation, error)
	CancelOperation(context.Context, string, runtimeapi.CancelOperationRequest) (runtimeapi.Operation, error)
	VerifyProxy(context.Context) (runtimeapi.ProxyVerification, error)
	RevealSourceURL(context.Context, string, bool) (runtimeapi.RevealSourceURLResponse, error)
	PreviewDiagnostics(context.Context, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsPreview, error)
	CreateDiagnostics(context.Context, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsResult, error)
}

type NetworkClient interface {
	PreviewNetwork(context.Context, runtimeapi.NetworkPreviewRequest) (runtimeapi.NetworkPreview, error)
}

type MihomoUpdateClient interface {
	PreviewMihomoUpdate(context.Context, runtimeapi.MihomoUpdatePreviewRequest) (runtimeapi.MihomoUpdatePlan, error)
}

type BackupClient interface {
	PreviewBackup(context.Context, runtimeapi.BackupPreviewRequest) (runtimeapi.BackupPreview, error)
	ExportBackup(context.Context, runtimeapi.BackupExportRequest) (runtimeapi.BackupArchive, error)
	PreviewBackupRestore(context.Context, runtimeapi.BackupRestorePreviewRequest) (runtimeapi.BackupRestorePreview, error)
}

type Model struct {
	ctx               context.Context
	client            Client
	editor            textarea.Model
	editing           bool
	editorMode        string
	busy              bool
	width             int
	height            int
	snapshot          runtimeapi.Snapshot
	selectedSourceID  string
	preview           runtimeapi.CandidatePreview
	networkPreview    runtimeapi.NetworkPreview
	mihomoUpdate      runtimeapi.MihomoUpdatePlan
	networkEditorMode string
	lastOperation     runtimeapi.Operation
	verification      runtimeapi.ProxyVerification
	status            string
	err               error
	sensitiveConfirm  string
	revealedURL       string
	diagnostics       runtimeapi.DiagnosticsPreview
	diagnosticsFile   runtimeapi.DiagnosticsResult
	backupPreview     runtimeapi.BackupPreview
	backupRestore     runtimeapi.BackupRestorePreview
	backupArchive     runtimeapi.BackupArchive
	backupFile        string
}

const (
	editorModeConfig         = "config"
	editorModeSource         = "source"
	editorModeImportedSource = "imported_source"
	editorModeResource       = "resource"
	editorModeOverride       = "override"
	editorModeNetwork        = "network"
	editorModeBackupExport   = "backup_export"
	editorModeBackupRestore  = "backup_restore"
)

type resourceDraft struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type importedSourceDraft struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type snapshotMsg struct {
	snapshot runtimeapi.Snapshot
}

type importPreviewMsg struct {
	content runtimeapi.ImportContent
	preview runtimeapi.CandidatePreview
}

type operationMsg struct {
	operation runtimeapi.Operation
}

type verificationMsg struct {
	verification runtimeapi.ProxyVerification
}

type overrideDocumentMsg struct {
	document runtimeapi.AdvancedOverrideDocument
}

type overridePreviewMsg struct {
	preview runtimeapi.CandidatePreview
}

type revealSourceURLMsg struct {
	response runtimeapi.RevealSourceURLResponse
}

type diagnosticsPreviewMsg struct {
	preview runtimeapi.DiagnosticsPreview
}

type diagnosticsResultMsg struct {
	result runtimeapi.DiagnosticsResult
}

type networkPreviewMsg struct {
	preview runtimeapi.NetworkPreview
}

type mihomoUpdateMsg struct {
	preview runtimeapi.MihomoUpdatePlan
}

type backupPreviewMsg struct {
	preview runtimeapi.BackupPreview
}

type backupExportMsg struct {
	archive runtimeapi.BackupArchive
	path    string
}

type backupRestorePreviewMsg struct {
	preview runtimeapi.BackupRestorePreview
}

type errMsg struct {
	err error
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7DD3FC"))
	labelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#CBD5E1"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#86EFAC"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FDE68A"))
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FCA5A5"))
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#94A3B8"))
)

func New(ctx context.Context, client Client) Model {
	if ctx == nil {
		ctx = context.Background()
	}
	editor := textarea.New()
	editor.Placeholder = "粘贴完整的 Mihomo YAML 配置"
	editor.SetWidth(80)
	editor.SetHeight(16)
	editor.ShowLineNumbers = true
	return Model{
		ctx:    ctx,
		client: client,
		editor: editor,
		status: "正在读取 Runtime 状态…",
		busy:   true,
	}
}

func Run(ctx context.Context, client Client, input io.Reader, output io.Writer) error {
	if client == nil {
		return errors.New("Runtime TUI client is required")
	}
	program := tea.NewProgram(
		New(ctx, client),
		tea.WithContext(ctx),
		tea.WithInput(input),
		tea.WithOutput(output),
	)
	_, err := program.Run()
	return err
}

func (m Model) Init() tea.Cmd {
	return m.observeCmd()
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		width := message.Width - 6
		if width < 40 {
			width = 40
		}
		if width > 100 {
			width = 100
		}
		m.editor.SetWidth(width)
		height := message.Height - 10
		if height < 8 {
			height = 8
		}
		if height > 24 {
			height = 24
		}
		m.editor.SetHeight(height)
	case snapshotMsg:
		m.snapshot = message.snapshot
		m.syncSelectedSource()
		m.busy = false
		m.err = nil
		m.status = "状态已更新"
	case importPreviewMsg:
		m.preview = message.preview
		m.busy = false
		m.editing = false
		m.err = nil
		m.status = fmt.Sprintf("候选配置已校验：%s", shortDigest(message.preview.CandidateSHA256))
		m.editor.Blur()
	case operationMsg:
		m.lastOperation = message.operation
		m.busy = false
		if m.editing {
			m.editing = false
			m.editor.Blur()
		}
		m.err = nil
		m.status = operationStatus(message.operation)
		if operationTerminal(message.operation.State) {
			return m, m.observeCmd()
		}
	case verificationMsg:
		m.verification = message.verification
		m.busy = false
		m.err = nil
		if message.verification.Available {
			m.status = "显式代理验证通过"
		} else {
			m.status = "显式代理当前不可用"
		}
	case overrideDocumentMsg:
		m.busy = false
		m.editing = true
		m.editorMode = editorModeOverride
		m.err = nil
		m.sensitiveConfirm = ""
		m.editor.SetValue(message.document.YAML)
		m.status = "编辑高级覆盖后按 Ctrl+S 校验并保存，Esc 取消"
		return m, m.editor.Focus()
	case overridePreviewMsg:
		m.preview = message.preview
		m.busy = false
		m.err = nil
		m.status = fmt.Sprintf("高级覆盖预览已校验：%s；Ctrl+S 保存", shortDigest(message.preview.CandidateSHA256))
	case revealSourceURLMsg:
		m.busy = false
		m.err = nil
		m.sensitiveConfirm = ""
		m.revealedURL = message.response.URL
		m.status = "已临时显示来源原始地址；离开当前界面后不会保存"
	case diagnosticsPreviewMsg:
		m.busy = false
		m.err = nil
		m.diagnostics = message.preview
		m.sensitiveConfirm = "diagnostics"
		m.status = "诊断包内容已预览；再次按 Ctrl+G 生成默认脱敏诊断包"
	case diagnosticsResultMsg:
		m.busy = false
		m.err = nil
		m.sensitiveConfirm = ""
		m.diagnosticsFile = message.result
		m.status = fmt.Sprintf("诊断包已保存：%s", message.result.FileName)
	case networkPreviewMsg:
		m.networkPreview = message.preview
		m.busy = false
		m.editing = false
		m.err = nil
		m.editor.Blur()
		m.status = fmt.Sprintf("%s 网络预览已生成：%s", message.preview.Mode, message.preview.PlanID)
	case mihomoUpdateMsg:
		m.mihomoUpdate = message.preview
		m.busy = false
		m.err = nil
		m.sensitiveConfirm = ""
		m.status = fmt.Sprintf("Mihomo %s 更新计划已验证；再按 U 查看确认提示", message.preview.Version)
	case backupPreviewMsg:
		m.backupPreview = message.preview
		m.busy = false
		m.err = nil
		if message.preview.IncludeSecrets {
			m.sensitiveConfirm = "backup-export-full"
			m.status = message.preview.Warning + " 再按一次 B 选择新输出文件。"
		} else {
			m.sensitiveConfirm = "backup-export-redacted"
			m.status = message.preview.Warning + " 再按一次 b 选择新输出文件。"
		}
	case backupExportMsg:
		m.backupArchive = message.archive
		m.backupFile = message.path
		m.busy = false
		m.editing = false
		m.editor.Blur()
		m.err = nil
		m.sensitiveConfirm = ""
		kind := "脱敏清单"
		if message.archive.IncludeSecrets {
			kind = "完整明文备份"
		}
		m.status = fmt.Sprintf("%s已保存：%s", kind, message.path)
	case backupRestorePreviewMsg:
		m.backupRestore = message.preview
		m.busy = false
		m.editing = false
		m.editor.Blur()
		m.err = nil
		m.sensitiveConfirm = "backup-restore:" + message.preview.ContentID
		m.status = message.preview.Warning + " 再按一次 L 明确确认恢复。"
	case errMsg:
		m.busy = false
		m.err = message.err
		m.status = publicErrorMessage(message.err)
	}

	if m.editing {
		if key, ok := message.(tea.KeyPressMsg); ok {
			switch key.String() {
			case "esc":
				m.editing = false
				m.editor.Blur()
				m.status = "已取消导入"
				return m, nil
			case "ctrl+s":
				if m.busy {
					return m, nil
				}
				body := []byte(m.editor.Value())
				if len(strings.TrimSpace(string(body))) == 0 {
					m.err = errors.New("配置不能为空")
					m.status = m.err.Error()
					return m, nil
				}
				m.busy = true
				if m.editorMode == editorModeBackupExport {
					client, ok := m.client.(BackupClient)
					if !ok {
						m.busy = false
						m.err = errors.New("当前 TUI 客户端不支持备份导出")
						m.status = m.err.Error()
						return m, nil
					}
					kind := "脱敏清单"
					if m.backupPreview.IncludeSecrets {
						kind = "完整明文备份"
					}
					m.status = "正在生成并保存" + kind + "…"
					return m, m.exportBackupCmd(
						client,
						strings.TrimSpace(string(body)),
						m.backupPreview.IncludeSecrets,
					)
				}
				if m.editorMode == editorModeBackupRestore {
					client, ok := m.client.(BackupClient)
					if !ok {
						m.busy = false
						m.err = errors.New("当前 TUI 客户端不支持备份恢复")
						m.status = m.err.Error()
						return m, nil
					}
					m.status = "正在读取、上传并检查备份…"
					return m, m.previewBackupRestoreCmd(client, strings.TrimSpace(string(body)))
				}
				if m.editorMode == editorModeNetwork {
					var request runtimeapi.NetworkPreviewRequest
					if err := json.Unmarshal(body, &request); err != nil {
						m.busy = false
						m.err = errors.New("网络设置必须是有效 JSON")
						m.status = m.err.Error()
						return m, nil
					}
					if request.Mode == "" {
						request.Mode = m.networkEditorMode
						if request.Mode == "" {
							request.Mode = runtimeapi.RunModeTUN
						}
					}
					if request.Mode != runtimeapi.RunModeTUN &&
						request.Mode != runtimeapi.RunModeGateway {
						m.busy = false
						m.err = errors.New("网络设置 mode 必须是 tun 或 gateway")
						m.status = m.err.Error()
						return m, nil
					}
					m.status = "正在生成网络预览…"
					return m, m.previewNetworkCmd(request)
				}
				if m.editorMode == editorModeSource {
					var draft runtimeapi.RemoteSourceDraft
					if err := json.Unmarshal(body, &draft); err != nil {
						m.busy = false
						m.err = errors.New("来源设置必须是有效 JSON")
						m.status = m.err.Error()
						return m, nil
					}
					m.status = "正在上传并添加远程来源…"
					return m, m.addSourceCmd(body)
				}
				if m.editorMode == editorModeImportedSource {
					var draft importedSourceDraft
					if err := json.Unmarshal(body, &draft); err != nil ||
						strings.TrimSpace(draft.Name) == "" ||
						strings.TrimSpace(draft.Content) == "" {
						m.busy = false
						m.err = errors.New("本机来源必须是包含 name 和 content 的有效 JSON")
						m.status = m.err.Error()
						return m, nil
					}
					m.status = "正在上传并添加本机配置来源…"
					return m, m.addImportedSourceCmd(draft)
				}
				if m.editorMode == editorModeResource {
					var draft resourceDraft
					if err := json.Unmarshal(body, &draft); err != nil ||
						strings.TrimSpace(draft.Name) == "" ||
						strings.TrimSpace(draft.Content) == "" {
						m.busy = false
						m.err = errors.New("托管资源必须是包含 name、kind 和 content 的有效 JSON")
						m.status = m.err.Error()
						return m, nil
					}
					m.status = "正在上传并添加托管资源…"
					return m, m.addResourceCmd(draft)
				}
				if m.editorMode == editorModeOverride {
					m.status = "正在校验并保存高级覆盖…"
					return m, m.setOverrideCmd(body)
				}
				m.status = "正在上传并校验候选配置…"
				return m, m.importPreviewCmd(body)
			case "ctrl+p":
				if m.editorMode != editorModeOverride || m.busy {
					return m, nil
				}
				sourceID := m.snapshot.Sources.CurrentSourceID
				if sourceID == "" {
					m.err = errors.New("当前没有可用于预览的远程来源")
					m.status = m.err.Error()
					return m, nil
				}
				body := []byte(m.editor.Value())
				if len(strings.TrimSpace(string(body))) == 0 {
					m.err = errors.New("高级覆盖不能为空")
					m.status = m.err.Error()
					return m, nil
				}
				m.busy = true
				m.status = "正在生成尚未保存的高级覆盖预览…"
				return m, m.previewOverrideCmd(sourceID, body)
			}
		}
		var command tea.Cmd
		m.editor, command = m.editor.Update(message)
		return m, command
	}

	if key, ok := message.(tea.KeyPressMsg); ok {
		if m.busy && key.String() != "q" && key.String() != "ctrl+c" {
			return m, nil
		}
		switch key.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.busy = true
			m.status = "正在读取 Runtime 状态…"
			return m, m.observeCmd()
		case "i":
			m.editing = true
			m.editorMode = editorModeConfig
			m.err = nil
			m.editor.SetValue("")
			m.status = "粘贴配置后按 Ctrl+S 上传并预览，Esc 取消"
			return m, m.editor.Focus()
		case "u":
			m.editing = true
			m.editorMode = editorModeSource
			m.err = nil
			m.editor.SetValue(defaultSourceDraft())
			m.status = "编辑来源 JSON 后按 Ctrl+S 添加，Esc 取消"
			return m, m.editor.Focus()
		case "n":
			m.editing = true
			m.editorMode = editorModeImportedSource
			m.err = nil
			m.editor.SetValue(defaultImportedSourceDraft())
			m.status = "编辑本机来源 JSON 后按 Ctrl+S 添加，Esc 取消"
			return m, m.editor.Focus()
		case "e":
			m.editing = true
			m.editorMode = editorModeResource
			m.err = nil
			m.editor.SetValue(defaultResourceDraft())
			m.status = "编辑资源 JSON 后按 Ctrl+S 添加，Esc 取消"
			return m, m.editor.Focus()
		case "o":
			if m.sensitiveConfirm != "override" {
				m.sensitiveConfirm = "override"
				m.err = nil
				m.status = runtimeapi.SensitiveDataWarning + " 再按一次 o 确认读取高级覆盖。"
				return m, nil
			}
			m.busy = true
			m.status = "正在读取高级覆盖…"
			return m, m.getOverrideCmd()
		case "a":
			if m.preview.ContentID == "" {
				m.err = errors.New("请先导入并预览配置")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在提交候选配置…"
			return m, m.executeCmd(runtimeapi.Action{
				Kind:   runtimeapi.ActionApplyImportedConfig,
				Params: runtimeapi.ActionParams{ContentID: m.preview.ContentID},
			})
		case "s":
			m.busy = true
			m.status = "正在提交启动操作…"
			return m, m.executeCmd(runtimeapi.Action{Kind: runtimeapi.ActionStartProxy})
		case "x":
			m.busy = true
			m.status = "正在提交停止操作…"
			return m, m.executeCmd(runtimeapi.Action{Kind: runtimeapi.ActionStopProxy})
		case "w":
			operationID := m.lastOperation.ID
			if operationID == "" {
				operationID = m.snapshot.Operations.CurrentOperationID
			}
			if operationID == "" {
				m.err = errors.New("当前没有可等待的运行操作")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在等待运行操作完成…"
			return m, m.waitCmd(operationID)
		case "g":
			operationID := m.lastOperation.ID
			if operationID == "" {
				operationID = m.snapshot.Operations.CurrentOperationID
			}
			if operationID == "" {
				m.err = errors.New("当前没有可查询的运行操作")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在查询运行操作…"
			return m, m.getCmd(operationID)
		case "c":
			operationID := m.lastOperation.ID
			if operationID == "" {
				operationID = m.snapshot.Operations.CurrentOperationID
			}
			if operationID == "" {
				m.err = errors.New("当前没有可取消的运行操作")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在取消运行操作…"
			return m, m.cancelCmd(operationID)
		case "v":
			m.busy = true
			m.status = "正在验证显式代理…"
			return m, m.verifyCmd()
		case "ctrl+u":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可显示原始地址的远程来源")
				m.status = m.err.Error()
				return m, nil
			}
			confirmation := "reveal:" + sourceID
			if m.sensitiveConfirm != confirmation {
				m.sensitiveConfirm = confirmation
				m.revealedURL = ""
				m.err = nil
				m.status = runtimeapi.SensitiveDataWarning + " 再按一次 Ctrl+U 确认显示。"
				return m, nil
			}
			m.busy = true
			m.status = "正在读取来源原始地址…"
			return m, m.revealSourceURLCmd(sourceID)
		case "ctrl+g":
			if m.sensitiveConfirm != "diagnostics" {
				m.busy = true
				m.status = runtimeapi.SensitiveDataWarning + " 正在生成默认脱敏预览…"
				return m, m.previewDiagnosticsCmd()
			}
			m.busy = true
			m.status = "正在生成默认脱敏诊断包…"
			return m, m.createDiagnosticsCmd()
		case "ctrl+t":
			m.editing = true
			m.editorMode = editorModeNetwork
			m.networkEditorMode = runtimeapi.RunModeTUN
			m.err = nil
			m.editor.SetValue(defaultNetworkDraft())
			m.status = "编辑普通 TUN 设置后按 Ctrl+S 预览，Esc 取消"
			return m, m.editor.Focus()
		case "ctrl+l":
			m.editing = true
			m.editorMode = editorModeNetwork
			m.networkEditorMode = runtimeapi.RunModeGateway
			m.err = nil
			m.editor.SetValue(defaultGatewayDraft())
			m.status = "编辑 Linux 网关设置后按 Ctrl+S 预览，Esc 取消"
			return m, m.editor.Focus()
		case "ctrl+e":
			if m.networkPreview.PlanID == "" {
				m.err = errors.New("请先生成网络预览")
				m.status = m.err.Error()
				return m, nil
			}
			if !m.networkPreview.ExpiresAt.IsZero() && !time.Now().Before(m.networkPreview.ExpiresAt) {
				m.networkPreview.PlanID = ""
				m.err = errors.New("网络预览已经过期，请重新预览")
				m.status = m.err.Error()
				return m, nil
			}
			if len(m.networkPreview.Conflicts) > 0 {
				m.err = errors.New("网络预览存在冲突，解决后请重新预览")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在提交网络接管启用操作…"
			actionKind := runtimeapi.ActionEnableTUN
			if m.networkPreview.Mode == runtimeapi.RunModeGateway {
				actionKind = runtimeapi.ActionEnableGateway
			}
			return m, m.executeCmd(runtimeapi.Action{
				Kind: actionKind,
				Params: runtimeapi.ActionParams{
					PlanID: m.networkPreview.PlanID,
				},
			})
		case "ctrl+x":
			m.busy = true
			m.status = "正在提交网络接管停用操作…"
			actionKind := runtimeapi.ActionDisableTUN
			if m.snapshot.Network.Mode == runtimeapi.RunModeGateway {
				actionKind = runtimeapi.ActionDisableGateway
			}
			return m, m.executeCmd(runtimeapi.Action{Kind: actionKind})
		case "U":
			if m.mihomoUpdate.PlanID == "" ||
				!m.mihomoUpdate.ExpiresAt.IsZero() && !time.Now().Before(m.mihomoUpdate.ExpiresAt) {
				client, ok := m.client.(MihomoUpdateClient)
				if !ok {
					m.err = errors.New("当前 TUI 客户端不支持 Mihomo 更新")
					m.status = m.err.Error()
					return m, nil
				}
				m.busy = true
				m.mihomoUpdate = runtimeapi.MihomoUpdatePlan{}
				m.status = "正在通过 TUF 检查官方 Mihomo 稳定更新…"
				return m, m.previewMihomoUpdateCmd(client)
			}
			confirmation := "mihomo-update:" + m.mihomoUpdate.PlanID
			if m.sensitiveConfirm != confirmation {
				m.sensitiveConfirm = confirmation
				m.err = nil
				m.status = fmt.Sprintf(
					"将把 Mihomo %s 更新为 %s，代理会短暂停止；再按一次 U 明确确认。",
					valueOr(m.mihomoUpdate.CurrentVersion, "未安装"),
					m.mihomoUpdate.Version,
				)
				return m, nil
			}
			m.sensitiveConfirm = ""
			m.busy = true
			m.status = "正在提交已确认的 Mihomo 更新…"
			return m, m.executeCmd(runtimeapi.Action{
				Kind: runtimeapi.ActionUpdateMihomo,
				Params: runtimeapi.ActionParams{
					PlanID:  m.mihomoUpdate.PlanID,
					Trust:   m.mihomoUpdate.Trust,
					Confirm: true,
				},
			})
		case "R":
			if m.snapshot.Updates.MihomoPreviousVersion == "" {
				m.err = errors.New("当前没有可回滚的上一版 Mihomo 核心")
				m.status = m.err.Error()
				return m, nil
			}
			if m.sensitiveConfirm != "mihomo-rollback" {
				m.sensitiveConfirm = "mihomo-rollback"
				m.err = nil
				m.status = fmt.Sprintf(
					"将从 Mihomo %s 回滚到 %s，代理会短暂停止；再按一次 R 明确确认。",
					valueOr(m.snapshot.Updates.MihomoCurrentVersion, "未知"),
					m.snapshot.Updates.MihomoPreviousVersion,
				)
				return m, nil
			}
			m.sensitiveConfirm = ""
			m.busy = true
			m.status = "正在提交已确认的 Mihomo 回滚…"
			return m, m.executeCmd(runtimeapi.Action{
				Kind:   runtimeapi.ActionRollbackMihomo,
				Params: runtimeapi.ActionParams{Confirm: true},
			})
		case "b", "B":
			client, ok := m.client.(BackupClient)
			if !ok {
				m.err = errors.New("当前 TUI 客户端不支持备份导出")
				m.status = m.err.Error()
				return m, nil
			}
			includeSecrets := key.String() == "B"
			confirmation := "backup-export-redacted"
			description := "不含秘密、不可恢复的脱敏清单"
			if includeSecrets {
				confirmation = "backup-export-full"
				description = "包含秘密内容的完整明文备份"
			}
			if m.sensitiveConfirm != confirmation {
				m.busy = true
				m.backupPreview = runtimeapi.BackupPreview{}
				m.status = "正在预览" + description + "…"
				return m, m.previewBackupCmd(client, includeSecrets)
			}
			m.editing = true
			m.editorMode = editorModeBackupExport
			m.err = nil
			m.editor.SetValue("submux-runtime-backup.zip")
			m.status = "输入尚不存在的输出文件后按 Ctrl+S 创建，Esc 取消"
			return m, m.editor.Focus()
		case "L":
			confirmation := "backup-restore:" + m.backupRestore.ContentID
			if m.backupRestore.ContentID != "" && m.sensitiveConfirm == confirmation {
				m.sensitiveConfirm = ""
				m.busy = true
				m.status = "正在提交已确认的整体恢复操作…"
				return m, m.executeCmd(runtimeapi.Action{
					Kind: runtimeapi.ActionRestoreBackup,
					Params: runtimeapi.ActionParams{
						ContentID: m.backupRestore.ContentID,
						Confirm:   true,
					},
				})
			}
			if _, ok := m.client.(BackupClient); !ok {
				m.err = errors.New("当前 TUI 客户端不支持备份恢复")
				m.status = m.err.Error()
				return m, nil
			}
			m.backupRestore = runtimeapi.BackupRestorePreview{}
			m.sensitiveConfirm = ""
			m.editing = true
			m.editorMode = editorModeBackupRestore
			m.err = nil
			m.editor.SetValue("")
			m.status = "输入备份文件后按 Ctrl+S 检查，Esc 取消；文件路径不会发送给 Runtime"
			return m, m.editor.Focus()
		case "y":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可预览的配置来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在生成所选来源的最终候选配置…"
			return m, m.previewSourceCmd(sourceID)
		case "f", "d", "m":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可刷新的配置来源")
				m.status = m.err.Error()
				return m, nil
			}
			route := ""
			if key.String() == "d" {
				route = runtimeapi.SourceRouteDirect
			}
			if key.String() == "m" {
				route = runtimeapi.SourceRouteMihomo
			}
			m.busy = true
			m.status = "正在提交来源刷新操作…"
			return m, m.executeCmd(runtimeapi.Action{
				Kind: runtimeapi.ActionRefreshSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
					Route:    route,
				},
			})
		case "[", "]":
			m.selectAdjacentSource(key.String() == "]")
			m.err = nil
			if source := m.selectedSourceSummary(); source != nil {
				m.status = fmt.Sprintf("已选择来源：%s（%s）", source.Name, source.Type)
			}
			return m, nil
		case "t", "k":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可切换的配置来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			useCached := key.String() == "k"
			if useCached {
				m.status = "正在刷新并切换来源；刷新失败时允许使用已验证缓存…"
			} else {
				m.status = "正在刷新并切换来源…"
			}
			return m, m.executeCmd(runtimeapi.Action{
				Kind: runtimeapi.ActionSwitchSource,
				Params: runtimeapi.ActionParams{
					SourceID:  sourceID,
					UseCached: useCached,
				},
			})
		case "z":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可删除的配置来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在删除所选来源…"
			return m, m.executeCmd(runtimeapi.Action{
				Kind: runtimeapi.ActionDeleteSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
				},
			})
		case "ctrl+d":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可删除的配置来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在确认删除所选来源…"
			return m, m.executeCmd(runtimeapi.Action{
				Kind: runtimeapi.ActionDeleteSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
					Confirm:  true,
				},
			})
		case "p":
			sourceID := m.snapshot.Sources.CurrentSourceID
			if sourceID == "" {
				m.err = errors.New("当前没有可应用的远程来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在提交来源应用操作…"
			return m, m.executeCmd(runtimeapi.Action{
				Kind: runtimeapi.ActionApplySource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
				},
			})
		}
	}
	return m, nil
}

func (m Model) View() tea.View {
	if m.editing {
		editorTitle := "Submux Runtime · 导入本机配置副本"
		editorHelp := "Ctrl+S 上传并预览 · Esc 取消"
		if m.editorMode == editorModeSource {
			editorTitle = "Submux Runtime · 添加远程配置来源"
			editorHelp = "Ctrl+S 添加来源 · Esc 取消；高风险设置必须填写精确 authorized_target"
		} else if m.editorMode == editorModeImportedSource {
			editorTitle = "Submux Runtime · 添加本机配置来源"
			editorHelp = "Ctrl+S 上传副本并添加来源 · Esc 取消；原文件路径不会保存"
		} else if m.editorMode == editorModeResource {
			editorTitle = "Submux Runtime · 添加托管资源"
			editorHelp = "Ctrl+S 上传内容并添加 · Esc 取消；只接受约定的资源类型"
		} else if m.editorMode == editorModeOverride {
			editorTitle = "Submux Runtime · 高级覆盖"
			editorHelp = "Ctrl+P 预览 · Ctrl+S 校验并保存 · Esc 取消；Runtime 保留字段不能覆盖"
		} else if m.editorMode == editorModeNetwork {
			editorTitle = "Submux Runtime · 普通 TUN 设置"
			if m.networkEditorMode == runtimeapi.RunModeGateway {
				editorTitle = "Submux Runtime · Linux 网关设置"
			}
			editorHelp = "Ctrl+S 生成真实网络预览 · Esc 取消；IPv6 必须明确选择 proxy、direct 或 block"
		} else if m.editorMode == editorModeBackupExport {
			editorTitle = "Submux Runtime · 创建脱敏备份清单"
			if m.backupPreview.IncludeSecrets {
				editorTitle = "Submux Runtime · 创建完整明文备份"
			}
			editorHelp = "Ctrl+S 创建 owner-only 新文件 · Esc 取消；不会覆盖已有文件，路径不会发送给 Runtime"
		} else if m.editorMode == editorModeBackupRestore {
			editorTitle = "Submux Runtime · 检查整体恢复备份"
			editorHelp = "Ctrl+S 读取并检查 · Esc 取消；文件内容经本机 IPC 上传，路径不会发送给 Runtime"
		}
		content := strings.Join([]string{
			titleStyle.Render(editorTitle),
			mutedStyle.Render(editorHelp),
			"",
			m.editor.View(),
			"",
			renderStatus(m.status, m.err, m.busy),
		}, "\n")
		return tea.NewView(content)
	}

	runtimeVersion := valueOr(m.snapshot.Runtime.Version, "未知")
	serviceState := valueOr(m.snapshot.Runtime.ServiceState, "不可用")
	mihomoState := valueOr(m.snapshot.Mihomo.State, "未知")
	mihomoDesired := valueOr(m.snapshot.Mihomo.DesiredState, "未设置")
	mihomoRecovery := valueOr(m.snapshot.Mihomo.Recovery, "未知")
	nextRestart := "无"
	if m.snapshot.Mihomo.NextRestartAt != nil {
		nextRestart = m.snapshot.Mihomo.NextRestartAt.Local().Format(time.RFC3339)
	}
	runMode := valueOr(m.snapshot.RunMode, "未配置")
	networkMode := valueOr(m.snapshot.Network.Mode, "未配置")
	if m.snapshot.Network.PreviewOnly {
		networkMode += "（预览）"
	}
	lines := []string{
		titleStyle.Render("Submux Runtime"),
		"",
		fmt.Sprintf("%s %s  %s %s", labelStyle.Render("Runtime"), runtimeVersion, labelStyle.Render("服务"), serviceState),
		fmt.Sprintf("%s %s  %s %s", labelStyle.Render("Mihomo 实际"), mihomoState, labelStyle.Render("期望"), mihomoDesired),
		fmt.Sprintf("%s %s  %s %s", labelStyle.Render("崩溃恢复"), mihomoRecovery, labelStyle.Render("运行方式"), runMode),
		fmt.Sprintf(
			"%s %s · %s%s  %s %t",
			labelStyle.Render("网络接管"),
			valueOr(m.snapshot.Network.State, "未知"),
			networkMode,
			networkDeviceSuffix(m.snapshot.Network.Device),
			labelStyle.Render("特权服务"),
			m.snapshot.Network.Available,
		),
		fmt.Sprintf("%s %d  %s %s", labelStyle.Render("重试次数"), m.snapshot.Mihomo.CrashAttempts, labelStyle.Render("下次重试"), nextRestart),
		fmt.Sprintf("%s %d  %s %d", labelStyle.Render("Revision"), m.snapshot.Revision, labelStyle.Render("队列"), m.snapshot.Operations.Queued),
		fmt.Sprintf(
			"%s %s  %s %s",
			labelStyle.Render("Mihomo 核心"),
			valueOr(m.snapshot.Updates.MihomoCurrentVersion, "未安装"),
			labelStyle.Render("上一版"),
			valueOr(m.snapshot.Updates.MihomoPreviousVersion, "无"),
		),
	}
	if m.snapshot.Mihomo.Fault != nil {
		lines = append(lines, errorStyle.Render(fmt.Sprintf(
			"Mihomo 故障：%s · %s",
			m.snapshot.Mihomo.Fault.Code,
			m.snapshot.Mihomo.Fault.Message,
		)))
	}
	if m.snapshot.Network.Fault != nil {
		lines = append(lines, errorStyle.Render(fmt.Sprintf(
			"网络故障：%s · %s",
			m.snapshot.Network.Fault.Code,
			m.snapshot.Network.Fault.Message,
		)))
	}
	for _, conflict := range m.snapshot.Network.Conflicts {
		lines = append(lines, errorStyle.Render(fmt.Sprintf(
			"网络冲突：%s · %s · %s",
			conflict.Kind,
			valueOr(conflict.Owner, "未知所有者"),
			conflict.Detail,
		)))
	}
	if len(m.snapshot.Network.Residuals) > 0 {
		for _, residual := range m.snapshot.Network.Residuals {
			lines = append(lines, errorStyle.Render(fmt.Sprintf(
				"网络残留：%s · %s · %s",
				residual.Kind,
				residual.Name,
				residual.State,
			)))
		}
	}
	if m.snapshot.Network.GatewaySettings != nil {
		lines = append(lines, fmt.Sprintf(
			"%s IPv6 %s · DNS %s · TCP %t · UDP %t · 主机 %t",
			labelStyle.Render("网关设置"),
			m.snapshot.Network.GatewaySettings.IPv6Policy,
			m.snapshot.Network.GatewaySettings.DNSPolicy,
			m.snapshot.Network.GatewaySettings.CaptureTCP,
			m.snapshot.Network.GatewaySettings.CaptureUDP,
			m.snapshot.Network.GatewaySettings.ProxyHostTraffic,
		))
	}
	if m.networkPreview.PlanID != "" {
		previewMode := m.networkPreview.Mode
		if m.networkPreview.PreviewOnly {
			previewMode += "（预览）"
		}
		previewSettings := fmt.Sprintf(
			"%s · %s · IPv6 %s · DNS %s",
			m.networkPreview.PlanID,
			m.networkPreview.Device,
			m.networkPreview.Settings.IPv6Policy,
			m.networkPreview.Settings.DNSPolicy,
		)
		if m.networkPreview.GatewaySettings != nil {
			previewSettings = fmt.Sprintf(
				"%s · %s · IPv6 %s · DNS %s · TCP %t · UDP %t · 主机 %t",
				m.networkPreview.PlanID,
				m.networkPreview.Device,
				m.networkPreview.GatewaySettings.IPv6Policy,
				m.networkPreview.GatewaySettings.DNSPolicy,
				m.networkPreview.GatewaySettings.CaptureTCP,
				m.networkPreview.GatewaySettings.CaptureUDP,
				m.networkPreview.GatewaySettings.ProxyHostTraffic,
			)
		}
		lines = append(lines,
			"",
			labelStyle.Render(previewMode+" 网络预览"),
			previewSettings,
		)
		for _, route := range m.networkPreview.Routes {
			disposition := "绕过"
			if !route.Bypass {
				disposition = "接管"
			}
			lines = append(lines, fmt.Sprintf("%s · %s · %s · %s · %s", route.ID, route.CIDR, route.Interface, route.Role, disposition))
		}
		for _, conflict := range m.networkPreview.Conflicts {
			lines = append(lines, errorStyle.Render("冲突："+conflict.Detail))
		}
		for _, warning := range m.networkPreview.Warnings {
			lines = append(lines, warnStyle.Render("警告："+warning))
		}
	}
	if m.preview.CandidateSHA256 != "" {
		lines = append(lines,
			"",
			labelStyle.Render("候选配置"),
			fmt.Sprintf("%s · %s · %s", shortDigest(m.preview.CandidateSHA256), m.preview.ProxyKind, strings.Join(m.preview.ProxyAddresses, ", ")),
		)
		for _, origin := range m.preview.FieldOrigins {
			lines = append(lines, fmt.Sprintf("%s · %s · %s", origin.Path, origin.Origin, origin.Status))
		}
	}
	if m.mihomoUpdate.PlanID != "" {
		lines = append(lines,
			"",
			labelStyle.Render("Mihomo 更新计划"),
			fmt.Sprintf(
				"%s · %s · %s/%s · %s",
				m.mihomoUpdate.Version,
				m.mihomoUpdate.Trust,
				m.mihomoUpdate.Platform,
				m.mihomoUpdate.Arch,
				m.mihomoUpdate.PlanID,
			),
		)
		if m.mihomoUpdate.Warning != "" {
			lines = append(lines, warnStyle.Render(m.mihomoUpdate.Warning))
		}
		if len(m.backupPreview.Items) > 0 {
			title := "脱敏备份清单预览"
			if m.backupPreview.IncludeSecrets {
				title = "完整备份预览"
			}
			lines = append(lines, "", labelStyle.Render(title))
			for _, item := range m.backupPreview.Items {
				if item.Included {
					lines = append(lines, fmt.Sprintf("%s · %d 项 · %d 字节", item.Name, item.Count, item.Size))
				}
			}
			lines = append(lines, warnStyle.Render(m.backupPreview.Warning))
		}
		if m.backupFile != "" {
			lines = append(lines, "", okStyle.Render(fmt.Sprintf(
				"备份已保存：%s · %d 字节 · %s",
				m.backupFile,
				m.backupArchive.Size,
				shortDigest(m.backupArchive.SHA256),
			)))
		}
		if m.backupRestore.ContentID != "" {
			lines = append(lines,
				"",
				labelStyle.Render("整体恢复预览"),
				fmt.Sprintf(
					"%d 个来源 · %d 个托管资源 · %d 份近期配置",
					m.backupRestore.SourceCount,
					m.backupRestore.ManagedResourceCount,
					m.backupRestore.RecentConfigurationCount,
				),
				warnStyle.Render("待重新确认："+strings.Join(m.backupRestore.PendingSettings, "、")),
				warnStyle.Render(m.backupRestore.Warning),
			)
		}
	}
	if m.revealedURL != "" {
		lines = append(lines, "", warnStyle.Render("来源原始地址："+m.revealedURL))
	}
	if len(m.diagnostics.Items) > 0 {
		lines = append(lines, "", labelStyle.Render("诊断包预览"))
		for _, item := range m.diagnostics.Items {
			sensitivity := "已脱敏"
			if item.Sensitive {
				sensitivity = "敏感"
			}
			lines = append(lines, fmt.Sprintf("%s · %d 字节 · %s", item.Name, item.Size, sensitivity))
		}
	}
	if m.lastOperation.ID != "" {
		lines = append(lines,
			"",
			labelStyle.Render("最近运行操作"),
			fmt.Sprintf("%s · %s · %s · %d%%", m.lastOperation.ID, m.lastOperation.State, m.lastOperation.Stage, m.lastOperation.Progress),
		)
	}
	if len(m.snapshot.Sources.Items) > 0 {
		lines = append(lines, "", labelStyle.Render("配置来源"))
		for _, source := range m.snapshot.Sources.Items {
			selection := "  "
			if source.ID == m.selectedSourceID {
				selection = "▶ "
			}
			current := ""
			if source.Current || source.ID == m.snapshot.Sources.CurrentSourceID {
				current = " [当前]"
			}
			line := fmt.Sprintf("%s%s%s · %s · %s · %s · %s",
				selection,
				source.Name,
				current,
				source.Type,
				source.ID,
				source.RedactedTarget,
				source.Route,
			)
			if source.LastRefreshResult != "" {
				line += " · " + source.LastRefreshResult
			}
			if len(source.HighRiskSettings) > 0 {
				line += " · 高风险：" + strings.Join(source.HighRiskSettings, "、")
			}
			lines = append(lines, line)
		}
	}
	if len(m.snapshot.Resources.Items) > 0 {
		lines = append(lines, "", labelStyle.Render("托管资源"))
		for _, resource := range m.snapshot.Resources.Items {
			lines = append(lines, fmt.Sprintf("%s · %s · %s · %d 字节",
				resource.ID,
				resource.Kind,
				resource.Name,
				resource.Size,
			))
		}
	}
	if m.snapshot.AdvancedOverride.Present {
		lines = append(lines, "", fmt.Sprintf("%s %s · %d 字节",
			labelStyle.Render("高级覆盖"),
			shortDigest(m.snapshot.AdvancedOverride.SHA256),
			m.snapshot.AdvancedOverride.Size,
		))
	}
	lines = append(lines,
		"",
		renderStatus(m.status, m.err, m.busy),
		"",
		warnStyle.Render(runtimeapi.SensitiveDataWarning),
		mutedStyle.Render("b 预览/创建脱敏清单 · B 预览/创建完整备份 · L 检查/确认整体恢复 · U 检查/确认安装 Mihomo 更新 · R 确认回滚 Mihomo · Ctrl+T 编辑/预览普通 TUN · Ctrl+L 编辑/预览 Linux 网关 · Ctrl+E 启用网络接管 · Ctrl+X 停用网络接管 · [/] 选择来源 · u 添加远程来源 · Ctrl+U 显示原始地址 · Ctrl+G 预览/生成诊断包 · n 添加本机来源 · y 预览所选来源 · f/d/m 刷新所选来源 · t 切换 · k 允许缓存切换 · z 删除 · Ctrl+D 确认删除当前来源 · i 临时导入/预览 · e 添加资源 · o 编辑高级覆盖 · p 应用当前来源 · a 应用临时导入 · s 启动 · x 停止 · g 查询 · w 等待 · c 取消 · v 验证 · r 刷新状态 · q 退出"),
	)
	return tea.NewView(strings.Join(lines, "\n"))
}

func (m Model) observeCmd() tea.Cmd {
	return func() tea.Msg {
		snapshot, err := m.client.Observe(m.ctx)
		if err != nil {
			return errMsg{err: err}
		}
		return snapshotMsg{snapshot: snapshot}
	}
}

func (m Model) previewMihomoUpdateCmd(client MihomoUpdateClient) tea.Cmd {
	return func() tea.Msg {
		preview, err := client.PreviewMihomoUpdate(m.ctx, runtimeapi.MihomoUpdatePreviewRequest{
			Source: runtimeapi.MihomoUpdateSourceOnlineTUF,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return mihomoUpdateMsg{preview: preview}
	}
}

func (m Model) previewBackupCmd(client BackupClient, includeSecrets bool) tea.Cmd {
	return func() tea.Msg {
		preview, err := client.PreviewBackup(m.ctx, runtimeapi.BackupPreviewRequest{
			IncludeSecrets: includeSecrets,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return backupPreviewMsg{preview: preview}
	}
}

func (m Model) exportBackupCmd(client BackupClient, path string, includeSecrets bool) tea.Cmd {
	return func() tea.Msg {
		archive, err := client.ExportBackup(m.ctx, runtimeapi.BackupExportRequest{
			IncludeSecrets:   includeSecrets,
			ConfirmPlaintext: true,
		})
		if err != nil {
			return errMsg{err: err}
		}
		output, err := runtimebackupfile.WriteNew(path, archive.Body, runtimeapi.RuntimeBackupMaxBytes)
		if err != nil {
			return errMsg{err: err}
		}
		archive.Body = nil
		return backupExportMsg{archive: archive, path: output}
	}
}

func (m Model) previewBackupRestoreCmd(client BackupClient, path string) tea.Cmd {
	return func() tea.Msg {
		body, err := runtimebackupfile.Read(path, runtimeapi.RuntimeBackupMaxBytes)
		if err != nil {
			return errMsg{err: err}
		}
		content, err := m.client.UploadImport(m.ctx, runtimeapi.RuntimeBackupContentType, body)
		if err != nil {
			return errMsg{err: err}
		}
		preview, err := client.PreviewBackupRestore(m.ctx, runtimeapi.BackupRestorePreviewRequest{
			ContentID: content.ID,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return backupRestorePreviewMsg{preview: preview}
	}
}

func (m Model) previewNetworkCmd(request runtimeapi.NetworkPreviewRequest) tea.Cmd {
	return func() tea.Msg {
		client, ok := m.client.(NetworkClient)
		if !ok {
			return errMsg{err: errors.New("当前 Runtime 客户端不支持网络预览")}
		}
		preview, err := client.PreviewNetwork(m.ctx, request)
		if err != nil {
			return errMsg{err: err}
		}
		return networkPreviewMsg{preview: preview}
	}
}

func (m Model) importPreviewCmd(body []byte) tea.Cmd {
	return func() tea.Msg {
		content, err := m.client.UploadImport(m.ctx, "application/x-yaml", body)
		if err != nil {
			return errMsg{err: err}
		}
		preview, err := m.client.PreviewCandidate(m.ctx, content.ID)
		if err != nil {
			return errMsg{err: err}
		}
		return importPreviewMsg{content: content, preview: preview}
	}
}

func (m Model) addSourceCmd(body []byte) tea.Cmd {
	revision := m.snapshot.Revision
	return func() tea.Msg {
		content, err := m.client.UploadImport(m.ctx, runtimeapi.SourceDraftContentType, body)
		if err != nil {
			return errMsg{err: err}
		}
		operation, err := m.client.Execute(m.ctx, runtimeapi.CreateOperationRequest{
			IfRevision: revision,
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionAddRemoteSource,
				Params: runtimeapi.ActionParams{
					ContentID: content.ID,
				},
			},
		})
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) addImportedSourceCmd(draft importedSourceDraft) tea.Cmd {
	revision := m.snapshot.Revision
	return func() tea.Msg {
		content, err := m.client.UploadImport(
			m.ctx,
			"application/x-yaml",
			[]byte(draft.Content),
		)
		if err != nil {
			return errMsg{err: err}
		}
		operation, err := m.client.Execute(m.ctx, runtimeapi.CreateOperationRequest{
			IfRevision: revision,
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionAddImportedSource,
				Params: runtimeapi.ActionParams{
					ContentID:  content.ID,
					SourceName: draft.Name,
				},
			},
		})
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) addResourceCmd(draft resourceDraft) tea.Cmd {
	revision := m.snapshot.Revision
	return func() tea.Msg {
		content, err := m.client.UploadImport(
			m.ctx,
			runtimeapi.ManagedResourceContentType,
			[]byte(draft.Content),
		)
		if err != nil {
			return errMsg{err: err}
		}
		operation, err := m.client.Execute(m.ctx, runtimeapi.CreateOperationRequest{
			IfRevision: revision,
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionAddManagedResource,
				Params: runtimeapi.ActionParams{
					ContentID:    content.ID,
					ResourceKind: draft.Kind,
					ResourceName: draft.Name,
				},
			},
		})
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) getOverrideCmd() tea.Cmd {
	return func() tea.Msg {
		document, err := m.client.GetAdvancedOverride(m.ctx, true)
		if err != nil {
			return errMsg{err: err}
		}
		return overrideDocumentMsg{document: document}
	}
}

func (m Model) setOverrideCmd(body []byte) tea.Cmd {
	revision := m.snapshot.Revision
	return func() tea.Msg {
		content, err := m.client.UploadImport(m.ctx, "application/x-yaml", body)
		if err != nil {
			return errMsg{err: err}
		}
		operation, err := m.client.Execute(m.ctx, runtimeapi.CreateOperationRequest{
			IfRevision: revision,
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionSetAdvancedOverride,
				Params: runtimeapi.ActionParams{
					ContentID: content.ID,
				},
			},
		})
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) previewSourceCmd(sourceID string) tea.Cmd {
	return func() tea.Msg {
		preview, err := m.client.PreviewCandidateRequest(m.ctx, runtimeapi.PreviewCandidateRequest{
			SourceID: sourceID,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return importPreviewMsg{preview: preview}
	}
}

func (m Model) previewOverrideCmd(sourceID string, body []byte) tea.Cmd {
	return func() tea.Msg {
		content, err := m.client.UploadImport(m.ctx, "application/x-yaml", body)
		if err != nil {
			return errMsg{err: err}
		}
		preview, err := m.client.PreviewCandidateRequest(m.ctx, runtimeapi.PreviewCandidateRequest{
			SourceID:          sourceID,
			OverrideContentID: content.ID,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return overridePreviewMsg{preview: preview}
	}
}

func (m Model) executeCmd(action runtimeapi.Action) tea.Cmd {
	revision := m.snapshot.Revision
	return func() tea.Msg {
		operation, err := m.client.Execute(m.ctx, runtimeapi.CreateOperationRequest{
			IfRevision: revision,
			Action:     action,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) waitCmd(operationID string) tea.Cmd {
	return func() tea.Msg {
		operation, err := m.client.WaitOperation(m.ctx, operationID, 250*time.Millisecond)
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) getCmd(operationID string) tea.Cmd {
	return func() tea.Msg {
		operation, err := m.client.GetOperation(m.ctx, operationID)
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) cancelCmd(operationID string) tea.Cmd {
	revision := m.snapshot.Revision
	return func() tea.Msg {
		operation, err := m.client.CancelOperation(m.ctx, operationID, runtimeapi.CancelOperationRequest{
			IfRevision: revision,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) verifyCmd() tea.Cmd {
	return func() tea.Msg {
		verification, err := m.client.VerifyProxy(m.ctx)
		if err != nil {
			return errMsg{err: err}
		}
		return verificationMsg{verification: verification}
	}
}

func (m Model) revealSourceURLCmd(sourceID string) tea.Cmd {
	return func() tea.Msg {
		response, err := m.client.RevealSourceURL(m.ctx, sourceID, true)
		if err != nil {
			return errMsg{err: err}
		}
		return revealSourceURLMsg{response: response}
	}
}

func (m Model) previewDiagnosticsCmd() tea.Cmd {
	return func() tea.Msg {
		preview, err := m.client.PreviewDiagnostics(m.ctx, runtimeapi.DiagnosticsRequest{})
		if err != nil {
			return errMsg{err: err}
		}
		return diagnosticsPreviewMsg{preview: preview}
	}
}

func (m Model) createDiagnosticsCmd() tea.Cmd {
	return func() tea.Msg {
		result, err := m.client.CreateDiagnostics(m.ctx, runtimeapi.DiagnosticsRequest{})
		if err != nil {
			return errMsg{err: err}
		}
		return diagnosticsResultMsg{result: result}
	}
}

func renderStatus(status string, err error, busy bool) string {
	if err != nil {
		return errorStyle.Render(status)
	}
	if busy {
		return warnStyle.Render(status)
	}
	return okStyle.Render(status)
}

func publicErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	type codedError interface {
		Error() string
	}
	var candidate codedError
	if errors.As(err, &candidate) {
		if strings.Contains(strings.ToLower(candidate.Error()), "version") ||
			strings.Contains(strings.ToLower(candidate.Error()), "protocol") {
			return runtimeprivacy.RedactText(candidate.Error()) + "；请重启或更新界面"
		}
	}
	return runtimeprivacy.RedactError(err)
}

func operationStatus(operation runtimeapi.Operation) string {
	status := fmt.Sprintf("运行操作 %s：%s / %s", operation.ID, operation.State, operation.Stage)
	if operation.Result == nil {
		return status
	}
	switch operation.Action.Kind {
	case runtimeapi.ActionSwitchSource:
		cache := ""
		if operation.Result.UsedCachedSource {
			cache = "，使用已验证缓存"
		}
		return fmt.Sprintf("%s；来源 %s → %s%s",
			status,
			valueOr(operation.Result.PreviousSourceID, "无"),
			valueOr(operation.Result.SourceID, "无"),
			cache,
		)
	case runtimeapi.ActionDeleteSource:
		if operation.Result.Deleted {
			return fmt.Sprintf("%s；已删除来源 %s", status, operation.Result.SourceID)
		}
	}
	return status
}

func (m *Model) syncSelectedSource() {
	previousSourceID := m.selectedSourceID
	if len(m.snapshot.Sources.Items) == 0 {
		m.selectedSourceID = ""
		m.clearRevealedSource(previousSourceID)
		return
	}
	for _, source := range m.snapshot.Sources.Items {
		if source.ID == m.selectedSourceID {
			return
		}
	}
	if m.snapshot.Sources.CurrentSourceID != "" {
		for _, source := range m.snapshot.Sources.Items {
			if source.ID == m.snapshot.Sources.CurrentSourceID {
				m.selectedSourceID = source.ID
				m.clearRevealedSource(previousSourceID)
				return
			}
		}
	}
	m.selectedSourceID = m.snapshot.Sources.Items[0].ID
	m.clearRevealedSource(previousSourceID)
}

func (m *Model) selectAdjacentSource(forward bool) {
	items := m.snapshot.Sources.Items
	if len(items) == 0 {
		previousSourceID := m.selectedSourceID
		m.selectedSourceID = ""
		m.clearRevealedSource(previousSourceID)
		return
	}
	index := 0
	for candidateIndex, source := range items {
		if source.ID == m.selectedSourceID {
			index = candidateIndex
			break
		}
	}
	if forward {
		index = (index + 1) % len(items)
	} else {
		index = (index - 1 + len(items)) % len(items)
	}
	previousSourceID := m.selectedSourceID
	m.selectedSourceID = items[index].ID
	m.clearRevealedSource(previousSourceID)
}

func (m *Model) clearRevealedSource(previousSourceID string) {
	if previousSourceID == m.selectedSourceID {
		return
	}
	m.revealedURL = ""
	if strings.HasPrefix(m.sensitiveConfirm, "reveal:") {
		m.sensitiveConfirm = ""
	}
}

func (m Model) selectedSource() string {
	if m.selectedSourceID != "" {
		return m.selectedSourceID
	}
	return m.snapshot.Sources.CurrentSourceID
}

func (m Model) selectedSourceSummary() *runtimeapi.SourceSummary {
	for index := range m.snapshot.Sources.Items {
		if m.snapshot.Sources.Items[index].ID == m.selectedSource() {
			return &m.snapshot.Sources.Items[index]
		}
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func networkDeviceSuffix(device string) string {
	if device == "" {
		return ""
	}
	return " · " + device
}

func shortDigest(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func operationTerminal(state string) bool {
	switch state {
	case runtimeapi.OperationSucceeded,
		runtimeapi.OperationFailed,
		runtimeapi.OperationCancelled,
		runtimeapi.OperationOutcomeUnknown:
		return true
	default:
		return false
	}
}

func defaultSourceDraft() string {
	return `{
  "type": "remote_http",
  "name": "primary",
  "url": "https://example.com/config.yaml",
  "route": "direct",
  "authorized_target": "",
  "allow_private": false,
  "allow_http": false,
  "custom_ca_pem": "",
  "skip_tls_verify": false,
  "refresh_interval_seconds": 21600,
  "timeout_seconds": 30,
  "max_response_bytes": 8388608
}`
}

func defaultImportedSourceDraft() string {
	return `{
  "name": "local-copy",
  "content": "proxies: []\nrules:\n  - MATCH,DIRECT\n"
}`
}

func defaultResourceDraft() string {
	return `{
  "name": "provider",
  "kind": "proxy-provider-yaml",
  "content": "proxies:\n  - name: example\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n"
}`
}

func defaultNetworkDraft() string {
	return `{
  "mode": "tun",
  "ipv6_policy": "proxy",
  "dns_policy": "hijack",
  "capture_route_ids": []
}`
}

func defaultGatewayDraft() string {
	return `{
  "mode": "gateway",
  "ipv6_policy": "direct",
  "dns_policy": "hijack",
  "capture_tcp": true,
  "capture_udp": true,
  "proxy_host_traffic": false,
  "excluded_route_ids": [],
  "udp_exceptions": [],
  "dns_direct_cidrs": [],
  "host_exceptions": []
}`
}
