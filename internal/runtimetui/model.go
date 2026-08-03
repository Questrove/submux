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
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"submux/internal/runtimeapi"
	"submux/internal/runtimebackupfile"
	"submux/internal/runtimeprivacy"
)

type Client interface {
	Observe(context.Context) (runtimeapi.Snapshot, error)
	WatchEvents(context.Context, uint64, func(runtimeapi.Event) error) error
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

type ProductUpdateClient interface {
	PreviewProductUpdate(context.Context, runtimeapi.ProductUpdatePreviewRequest) (runtimeapi.ProductUpdatePlan, error)
}

type BackupClient interface {
	PreviewBackup(context.Context, runtimeapi.BackupPreviewRequest) (runtimeapi.BackupPreview, error)
	ExportBackup(context.Context, runtimeapi.BackupExportRequest) (runtimeapi.BackupArchive, error)
	PreviewBackupRestore(context.Context, runtimeapi.BackupRestorePreviewRequest) (runtimeapi.BackupRestorePreview, error)
}

type TrafficClient interface {
	TrafficHistory(context.Context, runtimeapi.TrafficHistoryRequest) (runtimeapi.TrafficHistory, error)
}

type ConnectionClient interface {
	Connections(context.Context, runtimeapi.ConnectionQuery) (runtimeapi.ConnectionPage, error)
}

type RuleClient interface {
	Rules(context.Context, runtimeapi.RuleQuery) (runtimeapi.RuleSet, error)
}

type Model struct {
	ctx                   context.Context
	client                Client
	editor                textarea.Model
	editing               bool
	editorMode            string
	busy                  bool
	width                 int
	height                int
	snapshot              runtimeapi.Snapshot
	selectedSourceID      string
	preview               runtimeapi.CandidatePreview
	previewSourceID       string
	sourceForm            *sourceForm
	sourceDiagnosticOpen  bool
	networkForm           *networkForm
	networkPreview        runtimeapi.NetworkPreview
	mihomoUpdate          runtimeapi.MihomoUpdatePlan
	productUpdate         runtimeapi.ProductUpdatePlan
	lastOperation         runtimeapi.Operation
	verification          runtimeapi.ProxyVerification
	status                string
	err                   error
	sensitiveConfirm      string
	revealedURL           string
	diagnostics           runtimeapi.DiagnosticsPreview
	diagnosticsFile       runtimeapi.DiagnosticsResult
	backupPreview         runtimeapi.BackupPreview
	backupRestore         runtimeapi.BackupRestorePreview
	backupArchive         runtimeapi.BackupArchive
	backupFile            string
	page                  pageID
	focusIndex            int
	palette               textinput.Model
	paletteOpen           bool
	paletteIndex          int
	showHelp              bool
	snapshotStale         bool
	snapshotFault         error
	watchingEvents        bool
	reconnectAttempts     int
	observeGeneration     uint64
	confirmation          *actionConfirmation
	operationUncertain    bool
	operationFault        error
	waitingOperationID    string
	trafficHistory        []runtimeapi.TrafficSample
	trafficCursor         uint64
	trafficRange          time.Duration
	trafficStale          bool
	trafficFault          error
	connectionFilter      *connectionFilterForm
	connectionQuery       runtimeapi.ConnectionQuery
	connectionLoadedQuery runtimeapi.ConnectionQuery
	connectionPage        runtimeapi.ConnectionPage
	connectionHasData     bool
	connectionSelected    int
	connectionStale       bool
	connectionFault       error
	connectionsPolling    bool
	connectionsGeneration uint64
	connectionFailures    int
	trafficPolicyEditing  bool
	trafficPolicyDraft    string
	ruleViewerOpen        bool
	ruleFilter            *ruleFilterForm
	ruleView              string
	ruleQuery             runtimeapi.RuleQuery
	ruleSet               runtimeapi.RuleSet
	ruleSelected          int
	ruleGeneration        uint64
	proxyGroupOpen        bool
	proxyGroups           runtimeapi.ProxyGroupList
	proxyGroupSelected    int
	proxyNodeSelected     int
	proxyGroupLoading     bool
	proxyGroupFault       error
	proxyGroupGeneration  uint64
}

const (
	editorModeConfig        = "config"
	editorModeResource      = "resource"
	editorModeOverride      = "override"
	editorModeBackupExport  = "backup_export"
	editorModeBackupRestore = "backup_restore"
	editorModeProductImport = "product_import"
)

type resourceDraft struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type snapshotMsg struct {
	snapshot   runtimeapi.Snapshot
	generation uint64
}

type snapshotErrorMsg struct {
	err        error
	generation uint64
}

type runtimeEventMsg struct {
	event runtimeapi.Event
}

type runtimeWatchErrorMsg struct {
	err error
}

type runtimeReconnectMsg struct{}

type trafficHistoryMsg struct {
	history runtimeapi.TrafficHistory
	rangeAt time.Duration
	initial bool
}

type trafficHistoryErrorMsg struct {
	err     error
	rangeAt time.Duration
}

type trafficTickMsg struct{}

type connectionPageMsg struct {
	page       runtimeapi.ConnectionPage
	query      runtimeapi.ConnectionQuery
	generation uint64
}

type connectionPageErrorMsg struct {
	err        error
	query      runtimeapi.ConnectionQuery
	generation uint64
}

type connectionTickMsg struct {
	generation uint64
}

type trafficPolicyPreviewMsg struct {
	preview   runtimeapi.CandidatePreview
	selection string
}

type ruleSetMsg struct {
	rules      runtimeapi.RuleSet
	query      runtimeapi.RuleQuery
	generation uint64
}

type ruleSetErrorMsg struct {
	err        error
	query      runtimeapi.RuleQuery
	generation uint64
}

type preparedActionMsg struct {
	action runtimeapi.Action
}

type operationSubmitErrorMsg struct {
	action runtimeapi.Action
	err    error
}

type operationIOErrorMsg struct {
	operationID string
	mutating    bool
	err         error
}

type importPreviewMsg struct {
	content  runtimeapi.ImportContent
	preview  runtimeapi.CandidatePreview
	sourceID string
}

type operationMsg struct {
	operation runtimeapi.Operation
	fromWait  bool
}

type verificationMsg struct {
	verification runtimeapi.ProxyVerification
}

type overrideDocumentMsg struct {
	document runtimeapi.AdvancedOverrideDocument
}

type overridePreviewMsg struct {
	preview  runtimeapi.CandidatePreview
	sourceID string
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

type productUpdateMsg struct {
	preview runtimeapi.ProductUpdatePlan
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
	errRuntimeEventReceived = errors.New("Runtime event received")
	titleStyle              = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7DD3FC"))
	labelStyle              = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#CBD5E1"))
	okStyle                 = lipgloss.NewStyle().Foreground(lipgloss.Color("#86EFAC"))
	warnStyle               = lipgloss.NewStyle().Foreground(lipgloss.Color("#FDE68A"))
	errorStyle              = lipgloss.NewStyle().Foreground(lipgloss.Color("#FCA5A5"))
	mutedStyle              = lipgloss.NewStyle().Foreground(lipgloss.Color("#94A3B8"))
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
	palette := textinput.New()
	palette.Prompt = "搜索 › "
	palette.Placeholder = "输入页面或操作，例如：来源、启动、诊断"
	palette.CharLimit = 80
	palette.SetWidth(64)
	return Model{
		ctx:               ctx,
		client:            client,
		editor:            editor,
		page:              pageStatus,
		palette:           palette,
		status:            "正在读取 Runtime 状态…",
		busy:              true,
		observeGeneration: 1,
		trafficRange:      5 * time.Minute,
		connectionQuery: runtimeapi.ConnectionQuery{
			Page: 1, PageSize: runtimeapi.ConnectionPageDefaultSize,
		},
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
	return m.observeCmd(m.observeGeneration)
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
		if message.generation != m.observeGeneration {
			return m, nil
		}
		m.snapshot = message.snapshot
		m.syncSelectedSource()
		m.busy = false
		m.err = nil
		m.snapshotStale = false
		m.snapshotFault = nil
		m.reconnectAttempts = 0
		m.status = "状态已更新"
		return m, m.snapshotFollowUpCmd()
	case proxyGroupListMsg:
		if !m.acceptProxyGroupList(message) {
			return m, nil
		}
		if m.proxyGroupOpen {
			m.status = "Tab 切换代理组，↑↓ 选择节点，Enter 进入统一确认"
		}
		return m, nil
	case proxyGroupListErrorMsg:
		if !m.acceptProxyGroupError(message) {
			return m, nil
		}
		if m.proxyGroupOpen {
			m.status = publicErrorMessage(message.err)
		}
		return m, nil
	case snapshotErrorMsg:
		if message.generation != m.observeGeneration {
			return m, nil
		}
		m.busy = false
		m.snapshotStale = true
		m.snapshotFault = message.err
		if m.runningOperation() || m.snapshot.Operations.CurrentOperationID != "" {
			m.operationUncertain = true
			m.operationFault = message.err
		}
		m.status = "Runtime 本机 IPC 暂时不可用，正在重连"
		m.reconnectAttempts++
		return m, reconnectCmd(m.reconnectAttempts)
	case runtimeEventMsg:
		m.watchingEvents = false
		m.status = fmt.Sprintf("收到 Runtime 事件 %s，正在同步状态…", message.event.Type)
		observe := m.startObserveCmd()
		if message.event.OperationID != "" {
			return m, tea.Batch(observe, m.getCmd(message.event.OperationID))
		}
		return m, observe
	case runtimeWatchErrorMsg:
		m.watchingEvents = false
		if errors.Is(message.err, context.Canceled) ||
			(m.ctx.Err() != nil && errors.Is(message.err, m.ctx.Err())) {
			return m, nil
		}
		m.snapshotStale = true
		m.snapshotFault = message.err
		if m.runningOperation() || m.snapshot.Operations.CurrentOperationID != "" {
			m.operationUncertain = true
			m.operationFault = message.err
		}
		m.status = "Runtime 事件连接已断开，保留上次数据并重连"
		m.reconnectAttempts++
		return m, reconnectCmd(m.reconnectAttempts)
	case runtimeReconnectMsg:
		m.status = "正在重新连接 Runtime 本机 IPC…"
		return m, m.startObserveCmd()
	case trafficHistoryMsg:
		if m.page != pageMonitor || message.rangeAt != m.trafficRange {
			return m, nil
		}
		m.snapshot.Traffic = message.history.Status
		if message.initial || message.history.ResetRequired {
			m.trafficHistory = append([]runtimeapi.TrafficSample(nil), message.history.Samples...)
		} else {
			m.trafficHistory = appendTrafficSamples(m.trafficHistory, message.history.Samples)
		}
		m.trafficCursor = message.history.LatestCursor
		m.trafficStale = false
		m.trafficFault = nil
		if !m.connectionsPolling {
			m.connectionsPolling = true
			return m, tea.Batch(trafficTickCmd(), m.connectionsCmd())
		}
		return m, trafficTickCmd()
	case trafficHistoryErrorMsg:
		if m.page != pageMonitor || message.rangeAt != m.trafficRange {
			return m, nil
		}
		m.trafficStale = true
		m.trafficFault = message.err
		if !m.connectionsPolling {
			m.connectionsPolling = true
			return m, tea.Batch(trafficTickCmd(), m.connectionsCmd())
		}
		return m, trafficTickCmd()
	case trafficTickMsg:
		if m.page != pageMonitor {
			return m, nil
		}
		return m, m.trafficHistoryCmd(false)
	case connectionPageMsg:
		if m.page != pageMonitor || message.query != m.connectionQuery || message.generation != m.connectionsGeneration {
			return m, nil
		}
		m.connectionPage = message.page
		m.connectionLoadedQuery = message.query
		m.connectionHasData = true
		if len(message.page.Items) == 0 {
			m.connectionSelected = 0
		} else if m.connectionSelected >= len(message.page.Items) {
			m.connectionSelected = len(message.page.Items) - 1
		}
		m.connectionStale = !message.page.Available
		m.connectionFault = nil
		m.connectionsPolling = true
		if message.page.Available {
			m.connectionFailures = 0
		} else {
			m.connectionFailures++
		}
		return m, connectionTickCmd(m.connectionsGeneration, m.connectionFailures)
	case connectionPageErrorMsg:
		if m.page != pageMonitor || message.query != m.connectionQuery || message.generation != m.connectionsGeneration {
			return m, nil
		}
		m.connectionStale = true
		m.connectionFault = message.err
		m.connectionsPolling = true
		m.connectionFailures++
		return m, connectionTickCmd(m.connectionsGeneration, m.connectionFailures)
	case connectionTickMsg:
		if message.generation != m.connectionsGeneration {
			return m, nil
		}
		if m.page != pageMonitor {
			m.connectionsPolling = false
			return m, nil
		}
		return m, m.connectionsCmd()
	case ruleSetMsg:
		if !m.ruleViewerOpen || m.ruleView != runtimeapi.RuleViewApplied ||
			message.query != m.ruleQuery || message.generation != m.ruleGeneration {
			return m, nil
		}
		m.ruleSet = message.rules
		m.ruleSet.View = runtimeapi.RuleViewApplied
		m.ruleSelected = 0
		m.busy = false
		m.err = nil
		m.status = fmt.Sprintf("已读取 %d 条当前运行规则", message.rules.Total)
		return m, nil
	case ruleSetErrorMsg:
		if !m.ruleViewerOpen || m.ruleView != runtimeapi.RuleViewApplied ||
			message.query != m.ruleQuery || message.generation != m.ruleGeneration {
			return m, nil
		}
		m.busy = false
		m.err = message.err
		m.status = publicErrorMessage(message.err)
		return m, nil
	case trafficPolicyPreviewMsg:
		m.page = pageConfig
		m.focusIndex = 2
		m.preview = message.preview
		m.previewSourceID = m.snapshot.Sources.CurrentSourceID
		m.trafficPolicyEditing = false
		m.busy = false
		m.err = nil
		m.status = "流量策略候选配置已通过校验，等待确认保存"
		m.prepareAction(runtimeapi.Action{
			Kind:   runtimeapi.ActionSetTrafficPolicy,
			Params: runtimeapi.ActionParams{TrafficPolicy: message.selection},
		})
		return m, nil
	case preparedActionMsg:
		m.editing = false
		m.editor.Blur()
		m.prepareAction(message.action)
		return m, nil
	case operationSubmitErrorMsg:
		m.busy = false
		m.err = message.err
		if operationOutcomeUncertain(message.err) {
			m.operationUncertain = true
			m.operationFault = message.err
			m.status = "运行操作提交结果不确定；不会自动重放，请重新连接后核对运行操作"
		} else {
			m.operationUncertain = false
			m.operationFault = nil
			m.status = publicErrorMessage(message.err)
		}
		return m, nil
	case operationIOErrorMsg:
		if m.waitingOperationID == message.operationID {
			m.waitingOperationID = ""
		}
		m.busy = false
		m.err = message.err
		if operationOutcomeUncertain(message.err) {
			m.operationUncertain = true
			m.operationFault = message.err
			m.status = "运行操作状态暂时无法确认；等待重新连接后核对"
			if message.mutating {
				m.status = "取消请求结果不确定；不会自动重复取消，请重新连接后核对"
			}
		} else {
			m.status = publicErrorMessage(message.err)
		}
		return m, nil
	case importPreviewMsg:
		m.page = pageConfig
		m.focusIndex = 1
		m.preview = message.preview
		m.previewSourceID = message.sourceID
		m.busy = false
		m.editing = false
		m.err = nil
		m.status = fmt.Sprintf("候选配置已校验：%s", shortDigest(message.preview.CandidateSHA256))
		m.editor.Blur()
	case operationMsg:
		if m.waitingOperationID == message.operation.ID &&
			(message.fromWait || operationTerminal(message.operation.State)) {
			m.waitingOperationID = ""
		}
		m.lastOperation = message.operation
		m.operationUncertain = false
		m.operationFault = nil
		m.busy = false
		if m.editing {
			m.editing = false
			m.editor.Blur()
		}
		m.err = nil
		m.status = operationStatus(message.operation)
		if message.operation.State == runtimeapi.OperationSucceeded &&
			message.operation.Result != nil &&
			message.operation.Result.CandidateSHA256 != "" &&
			strings.EqualFold(message.operation.Result.CandidateSHA256, m.preview.CandidateSHA256) {
			m.preview = runtimeapi.CandidatePreview{}
			m.previewSourceID = ""
		}
		if operationTerminal(message.operation.State) {
			return m, m.startObserveCmd()
		}
		return m, m.waitForOperationCmd(message.operation.ID)
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
		m.page = pageConfig
		m.focusIndex = 2
		m.busy = false
		m.editing = true
		m.editorMode = editorModeOverride
		m.err = nil
		m.sensitiveConfirm = ""
		m.editor.SetValue(message.document.YAML)
		m.status = "编辑高级覆盖后按 Ctrl+S 校验并保存，Esc 取消"
		return m, m.editor.Focus()
	case overridePreviewMsg:
		m.page = pageConfig
		m.focusIndex = 2
		m.preview = message.preview
		m.previewSourceID = message.sourceID
		m.busy = false
		m.err = nil
		m.status = fmt.Sprintf("高级覆盖预览已校验：%s；Ctrl+S 保存", shortDigest(message.preview.CandidateSHA256))
	case revealSourceURLMsg:
		m.page = pageConfig
		m.focusIndex = 0
		m.busy = false
		m.err = nil
		m.sensitiveConfirm = ""
		m.revealedURL = message.response.URL
		m.sourceDiagnosticOpen = true
		m.status = "已打开来源只读诊断；敏感内容不会保存"
	case diagnosticsPreviewMsg:
		m.page = pageMaintenance
		m.focusIndex = 2
		m.busy = false
		m.err = nil
		m.diagnostics = message.preview
		m.sensitiveConfirm = "diagnostics"
		m.status = "诊断包内容已预览；再次按 Ctrl+G 生成默认脱敏诊断包"
	case diagnosticsResultMsg:
		m.page = pageMaintenance
		m.focusIndex = 2
		m.busy = false
		m.err = nil
		m.sensitiveConfirm = ""
		m.diagnosticsFile = message.result
		m.status = fmt.Sprintf("诊断包已保存：%s", message.result.FileName)
	case networkPreviewMsg:
		m.page = pageNetwork
		m.focusIndex = 1
		m.networkPreview = message.preview
		m.busy = false
		m.editing = false
		m.err = nil
		m.editor.Blur()
		m.status = fmt.Sprintf("%s 网络预览已生成：%s", message.preview.Mode, message.preview.PlanID)
	case mihomoUpdateMsg:
		m.page = pageMaintenance
		m.focusIndex = 0
		m.mihomoUpdate = message.preview
		m.busy = false
		m.err = nil
		m.sensitiveConfirm = ""
		m.status = fmt.Sprintf("Mihomo %s 更新计划已验证；再按 U 查看确认提示", message.preview.Version)
	case productUpdateMsg:
		m.page = pageMaintenance
		m.focusIndex = 0
		m.productUpdate = message.preview
		m.busy = false
		m.editing = false
		m.err = nil
		m.sensitiveConfirm = ""
		m.editor.Blur()
		m.status = fmt.Sprintf("Runtime %s 产品更新计划已验证；再按 P 查看确认提示", message.preview.Version)
	case backupPreviewMsg:
		m.page = pageMaintenance
		m.focusIndex = 1
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
		m.page = pageMaintenance
		m.focusIndex = 1
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
		m.page = pageMaintenance
		m.focusIndex = 1
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

	if m.sourceDiagnosticOpen {
		if key, ok := message.(tea.KeyPressMsg); ok {
			switch key.String() {
			case "esc", "ctrl+u":
				m.sourceDiagnosticOpen = false
				m.revealedURL = ""
				m.sensitiveConfirm = ""
				m.status = "已关闭来源只读诊断"
				return m, nil
			}
		}
		return m, nil
	}
	if m.proxyGroupOpen {
		return m.updateProxyGroups(message)
	}

	if m.sourceForm != nil {
		return m.updateSourceForm(message)
	}
	if m.networkForm != nil {
		return m.updateNetworkForm(message)
	}
	if m.connectionFilter != nil {
		return m.updateConnectionFilter(message)
	}
	if m.ruleFilter != nil {
		return m.updateRuleFilter(message)
	}
	if m.ruleViewerOpen {
		return m.updateRuleViewer(message)
	}
	if m.trafficPolicyEditing {
		return m.updateTrafficPolicyEditor(message)
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
				if m.editorMode == editorModeProductImport {
					client, ok := m.client.(ProductUpdateClient)
					if !ok {
						m.busy = false
						m.err = errors.New("当前 TUI 客户端不支持 Runtime 产品更新")
						m.status = m.err.Error()
						return m, nil
					}
					m.status = "正在读取、上传并验证离线产品更新包…"
					return m, m.previewOfflineProductUpdateCmd(client, strings.TrimSpace(string(body)))
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

	if m.paletteOpen {
		return m.updatePalette(message)
	}

	if key, ok := message.(tea.KeyPressMsg); ok {
		if m.confirmation != nil {
			switch key.String() {
			case "enter":
				return m, m.confirmAction()
			case "esc":
				m.cancelActionConfirmation()
				return m, nil
			}
		}
		switch key.String() {
		case "/", "ctrl+k":
			return m.openPalette()
		case "?":
			m.showHelp = !m.showHelp
			return m, nil
		case "1", "2", "3", "4", "5":
			page := pageFromKey(key.String())
			m.openPage(page)
			if page == pageMonitor {
				return m, m.trafficHistoryCmd(true)
			}
			if page == pageStatus {
				return m, m.startProxyGroupsCmd(m.snapshot.Sources.CurrentSourceID)
			}
			if page == pageConfig {
				return m, m.startProxyGroupsCmd(m.selectedSource())
			}
			return m, nil
		case "tab":
			m.moveFocus(1)
			return m, nil
		case "shift+tab":
			m.moveFocus(-1)
			return m, nil
		case "up", "down":
			if command := m.moveSelection(key.String() == "down"); command != nil {
				return m, command
			}
			return m, nil
		case "enter":
			if command := m.activateFocus(); command != nil {
				return m, command
			}
			return m, nil
		case "esc":
			if m.showHelp {
				m.showHelp = false
			} else if m.page != pageStatus {
				m.openPage(pageStatus)
			}
			return m, nil
		}
		if m.busy && key.String() != "q" && key.String() != "ctrl+c" {
			return m, nil
		}
		switch key.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "ctrl+r":
			if m.page == pageConfig {
				return m, m.openAppliedRules(runtimeapi.RuleQuery{})
			}
			if m.page == pageMonitor && m.focusIndex == 2 {
				if len(m.connectionPage.Items) == 0 || m.connectionSelected < 0 || m.connectionSelected >= len(m.connectionPage.Items) {
					m.err = errors.New("当前没有可定位规则的活动连接")
					m.status = m.err.Error()
					return m, nil
				}
				connection := m.connectionPage.Items[m.connectionSelected]
				target := ""
				if len(connection.OutboundChain) > 0 {
					target = connection.OutboundChain[0]
				}
				return m, m.openAppliedRules(runtimeapi.RuleQuery{Content: connection.RulePayload, Type: connection.Rule, Target: target})
			}
			m.status = "最终规则只读查看器位于配置页；监控页可从活动连接跳转"
			return m, nil
		case "r":
			m.busy = true
			m.status = "正在读取 Runtime 状态…"
			return m, m.startObserveCmd()
		case "i":
			m.editing = true
			m.editorMode = editorModeConfig
			m.err = nil
			m.editor.SetValue("")
			m.status = "粘贴配置后按 Ctrl+S 上传并预览，Esc 取消"
			return m, m.editor.Focus()
		case "u":
			m.sourceForm = newRemoteSourceForm()
			m.err = nil
			m.status = "填写远程来源表单；Tab 切换字段，Ctrl+S 预览添加"
			return m, m.sourceForm.loadActiveField()
		case "n":
			m.sourceForm = newLocalSourceForm()
			m.err = nil
			m.status = "填写本机来源表单；文件只在本机读取，路径不会发送给 Runtime"
			return m, m.sourceForm.loadActiveField()
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
		case "ctrl+n":
			sourceID := m.snapshot.Sources.CurrentSourceID
			if m.page == pageConfig {
				sourceID = m.selectedSource()
			}
			return m, m.openProxyGroups(sourceID)
		case "a":
			if m.preview.ContentID == "" {
				m.err = errors.New("请先导入并预览配置")
				m.status = m.err.Error()
				return m, nil
			}
			m.prepareAction(runtimeapi.Action{
				Kind:   runtimeapi.ActionApplyImportedConfig,
				Params: runtimeapi.ActionParams{ContentID: m.preview.ContentID},
			})
			return m, nil
		case "s":
			m.prepareAction(runtimeapi.Action{Kind: runtimeapi.ActionStartProxy})
			return m, nil
		case "x":
			m.prepareAction(runtimeapi.Action{Kind: runtimeapi.ActionStopProxy})
			return m, nil
		case "w":
			operationID := m.operationIDForControl()
			if operationID == "" {
				m.err = errors.New("当前没有可等待的运行操作")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在等待运行操作完成…"
			return m, m.waitForOperationCmd(operationID)
		case "g":
			operationID := m.operationIDForControl()
			if operationID == "" {
				m.err = errors.New("当前没有可查询的运行操作")
				m.status = m.err.Error()
				return m, nil
			}
			m.busy = true
			m.status = "正在查询运行操作…"
			return m, m.getCmd(operationID)
		case "c":
			operationID := m.operationIDForControl()
			if operationID == "" {
				m.err = errors.New("当前没有可取消的运行操作")
				m.status = m.err.Error()
				return m, nil
			}
			if m.lastOperation.ID != operationID {
				m.busy = true
				m.status = "正在读取 Runtime 声明的可取消状态…"
				return m, m.getCmd(operationID)
			}
			if !m.lastOperation.Cancellable {
				m.err = errors.New("Runtime 声明当前运行操作不可取消")
				m.status = m.err.Error()
				return m, nil
			}
			m.prepareCancellation(m.lastOperation)
			return m, nil
		case "v":
			m.busy = true
			m.status = "正在验证显式代理…"
			return m, m.verifyCmd()
		case "ctrl+u":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可打开只读诊断的配置来源")
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
			if source := m.selectedSourceSummary(); source != nil && source.Type == runtimeapi.SourceTypeLocalImport {
				m.sensitiveConfirm = ""
				m.revealedURL = ""
				m.sourceDiagnosticOpen = true
				m.status = "已打开本机来源只读诊断；本机导入没有远程请求地址"
				return m, nil
			}
			m.busy = true
			m.status = "正在读取来源原始地址…"
			return m, m.revealSourceURLCmd(sourceID)
		case "ctrl+y":
			if m.page != pageConfig || m.focusIndex != 2 {
				m.status = "流量策略只在配置页的本机配置层中修改"
				return m, nil
			}
			selection := m.snapshot.TrafficPolicy.Selection
			if selection == "" {
				selection = runtimeapi.TrafficPolicyFollowSource
			}
			m.trafficPolicyEditing = true
			m.trafficPolicyDraft = selection
			m.err = nil
			m.status = "选择流量策略后按 Enter 生成候选配置，Esc 取消"
			return m, nil
		case "ctrl+g":
			if m.sensitiveConfirm != "diagnostics" {
				m.busy = true
				m.status = runtimeapi.SensitiveDataWarning + " 正在生成默认脱敏预览…"
				return m, m.previewDiagnosticsCmd()
			}
			m.busy = true
			m.status = "正在生成默认脱敏诊断包…"
			return m, m.createDiagnosticsCmd()
		case "ctrl+p":
			m.networkForm = newNetworkForm(runtimeapi.RunModeExplicit)
			m.err = nil
			m.status = "已选择显式代理；Ctrl+S 进入统一确认"
			return m, m.networkForm.loadActiveField()
		case "ctrl+t":
			m.networkForm = newNetworkForm(runtimeapi.RunModeTUN)
			m.err = nil
			m.status = "填写普通 TUN 设置；Ctrl+S 生成真实网络预览"
			return m, m.networkForm.loadActiveField()
		case "ctrl+l":
			m.networkForm = newNetworkForm(runtimeapi.RunModeGateway)
			m.err = nil
			m.status = "填写 Linux 网关设置；Ctrl+S 生成真实网络预览"
			return m, m.networkForm.loadActiveField()
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
			actionKind := runtimeapi.ActionEnableTUN
			if m.networkPreview.Mode == runtimeapi.RunModeGateway {
				actionKind = runtimeapi.ActionEnableGateway
			}
			m.prepareAction(runtimeapi.Action{
				Kind: actionKind,
				Params: runtimeapi.ActionParams{
					PlanID: m.networkPreview.PlanID,
				},
			})
			return m, nil
		case "ctrl+x":
			actionKind := runtimeapi.ActionStopProxy
			switch m.snapshot.Network.Mode {
			case runtimeapi.RunModeTUN:
				actionKind = runtimeapi.ActionDisableTUN
			case runtimeapi.RunModeGateway:
				actionKind = runtimeapi.ActionDisableGateway
			}
			m.prepareAction(runtimeapi.Action{Kind: actionKind})
			return m, nil
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
			m.sensitiveConfirm = ""
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionUpdateMihomo,
				Params: runtimeapi.ActionParams{
					PlanID:  m.mihomoUpdate.PlanID,
					Trust:   m.mihomoUpdate.Trust,
					Confirm: true,
				},
			})
			return m, nil
		case "R":
			if m.snapshot.Updates.MihomoPreviousVersion == "" {
				m.err = errors.New("当前没有可回滚的上一版 Mihomo 核心")
				m.status = m.err.Error()
				return m, nil
			}
			m.sensitiveConfirm = ""
			m.prepareAction(runtimeapi.Action{
				Kind:   runtimeapi.ActionRollbackMihomo,
				Params: runtimeapi.ActionParams{Confirm: true},
			})
			return m, nil
		case "P":
			if m.productUpdate.PlanID == "" ||
				!m.productUpdate.ExpiresAt.IsZero() && !time.Now().Before(m.productUpdate.ExpiresAt) {
				client, ok := m.client.(ProductUpdateClient)
				if !ok {
					m.err = errors.New("当前 TUI 客户端不支持 Runtime 产品更新")
					m.status = m.err.Error()
					return m, nil
				}
				m.busy = true
				m.productUpdate = runtimeapi.ProductUpdatePlan{}
				m.status = "正在通过 TUF 检查 Runtime 稳定产品更新；不会预下载程序…"
				return m, m.previewProductUpdateCmd(client)
			}
			if !m.productUpdate.Installable {
				m.err = errors.New("当前平台安装器不可用；已验证计划只能查看，不能安装")
				m.status = m.err.Error()
				return m, nil
			}
			m.sensitiveConfirm = ""
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionUpdateProduct,
				Params: runtimeapi.ActionParams{
					PlanID:  m.productUpdate.PlanID,
					Trust:   runtimeapi.ProductUpdateTrustTUF,
					Confirm: true,
				},
			})
			return m, nil
		case "I":
			if _, ok := m.client.(ProductUpdateClient); !ok {
				m.err = errors.New("当前 TUI 客户端不支持 Runtime 产品更新")
				m.status = m.err.Error()
				return m, nil
			}
			m.productUpdate = runtimeapi.ProductUpdatePlan{}
			m.sensitiveConfirm = ""
			m.editing = true
			m.editorMode = editorModeProductImport
			m.err = nil
			m.editor.SetValue("")
			m.status = "输入离线产品 TUF 包路径后按 Ctrl+S 验证；文件路径不会发送给 Runtime"
			return m, m.editor.Focus()
		case "O":
			if m.snapshot.Updates.RuntimePreviousVersion == "" {
				m.err = errors.New("当前没有可回滚的上一版 Runtime 产品")
				m.status = m.err.Error()
				return m, nil
			}
			m.sensitiveConfirm = ""
			m.prepareAction(runtimeapi.Action{
				Kind:   runtimeapi.ActionRollbackProduct,
				Params: runtimeapi.ActionParams{Confirm: true},
			})
			return m, nil
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
			if m.backupRestore.ContentID != "" {
				m.sensitiveConfirm = ""
				m.prepareAction(runtimeapi.Action{
					Kind: runtimeapi.ActionRestoreBackup,
					Params: runtimeapi.ActionParams{
						ContentID: m.backupRestore.ContentID,
						Confirm:   true,
					},
				})
				return m, nil
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
		case "D":
			if m.page != pageMonitor || m.focusIndex != 2 {
				m.status = "关闭连接只在监控页的活动连接区域可用"
				return m, nil
			}
			if len(m.connectionPage.Items) == 0 || m.connectionSelected < 0 || m.connectionSelected >= len(m.connectionPage.Items) {
				m.status = "当前没有可关闭的活动连接"
				return m, nil
			}
			connection := m.connectionPage.Items[m.connectionSelected]
			if connection.ID == "" {
				m.err = errors.New("所选连接没有稳定连接标识，无法关闭")
				m.status = m.err.Error()
				return m, nil
			}
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionCloseConnection,
				Params: runtimeapi.ActionParams{
					ConnectionID:     connection.ID,
					ConnectionTarget: connection.Target,
				},
			})
			return m, nil
		case "X":
			if m.page != pageMonitor || m.focusIndex != 2 {
				m.status = "批量关闭连接只在监控页的活动连接区域可用"
				return m, nil
			}
			if !m.connectionHasData || m.connectionStale || !m.connectionPage.Available || m.connectionPage.ScopeToken == "" {
				m.err = errors.New("活动连接数据未同步或已过期，请刷新后再确认范围")
				m.status = m.err.Error()
				return m, nil
			}
			if m.connectionPage.Total <= 0 {
				m.status = "当前筛选范围内没有可关闭的活动连接"
				return m, nil
			}
			scope := m.connectionLoadedQuery
			scope.Page = 0
			scope.PageSize = 0
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionCloseConnections,
				Params: runtimeapi.ActionParams{
					ConnectionScope:      &scope,
					ConnectionScopeToken: m.connectionPage.ScopeToken,
					ConnectionCount:      m.connectionPage.Total,
					Confirm:              true,
				},
			})
			return m, nil
		case "f", "d", "m":
			if key.String() == "f" && m.page == pageMonitor {
				m.connectionFilter = newConnectionFilterForm(m.connectionQuery)
				m.err = nil
				m.status = "填写活动连接筛选；Ctrl+S 应用，Esc 取消"
				return m, m.connectionFilter.loadActiveField()
			}
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
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionRefreshSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
					Route:    route,
				},
			})
			return m, nil
		case ",", ".":
			if m.page != pageMonitor || m.focusIndex != 2 {
				return m, nil
			}
			if !m.changeConnectionPage(key.String() == ".") {
				return m, nil
			}
			return m, m.connectionsCmd()
		case "[", "]":
			if m.page == pageMonitor {
				m.cycleTrafficRange(key.String() == "]")
				return m, m.trafficHistoryCmd(true)
			}
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
			useCached := key.String() == "k"
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionSwitchSource,
				Params: runtimeapi.ActionParams{
					SourceID:  sourceID,
					UseCached: useCached,
				},
			})
			return m, nil
		case "z":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可删除的配置来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionDeleteSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
					Confirm:  true,
				},
			})
			return m, nil
		case "ctrl+d":
			sourceID := m.selectedSource()
			if sourceID == "" {
				m.err = errors.New("当前没有可删除的配置来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionDeleteSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
					Confirm:  true,
				},
			})
			return m, nil
		case "p":
			sourceID := m.snapshot.Sources.CurrentSourceID
			if sourceID == "" {
				m.err = errors.New("当前没有可应用的远程来源")
				m.status = m.err.Error()
				return m, nil
			}
			m.prepareAction(runtimeapi.Action{
				Kind: runtimeapi.ActionApplySource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceID,
				},
			})
			return m, nil
		}
	}
	return m, nil
}

func (m Model) legacyView() tea.View {
	if m.editing {
		editorTitle := "Submux Runtime · 导入本机配置副本"
		editorHelp := "Ctrl+S 上传并预览 · Esc 取消"
		if m.editorMode == editorModeResource {
			editorTitle = "Submux Runtime · 添加托管资源"
			editorHelp = "Ctrl+S 上传内容并添加 · Esc 取消；只接受约定的资源类型"
		} else if m.editorMode == editorModeOverride {
			editorTitle = "Submux Runtime · 高级覆盖"
			editorHelp = "Ctrl+P 预览 · Ctrl+S 校验并保存 · Esc 取消；Runtime 保留字段不能覆盖"
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
	}
	if m.productUpdate.PlanID != "" {
		lines = append(lines,
			"",
			labelStyle.Render("Runtime 产品更新计划"),
			fmt.Sprintf(
				"%s → %s · %s/%s · %s",
				valueOr(m.productUpdate.CurrentVersion, "未知"),
				m.productUpdate.Version,
				m.productUpdate.Platform,
				m.productUpdate.Arch,
				m.productUpdate.PlanID,
			),
			fmt.Sprintf(
				"数据库 %d → %d · IPC %d（允许 %d-%d）",
				m.productUpdate.Migration.CurrentSchema,
				m.productUpdate.Migration.TargetSchema,
				m.productUpdate.Migration.CurrentProtocol,
				m.productUpdate.Migration.ProtocolMin,
				m.productUpdate.Migration.ProtocolMax,
			),
			m.productUpdate.NetworkInterruption,
			m.productUpdate.ReleaseNotes,
		)
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
		mutedStyle.Render("P 检查/确认安装 Runtime 产品更新 · I 验证离线产品包 · O 确认回滚 Runtime 产品 · U 检查/确认安装 Mihomo 更新 · R 确认回滚 Mihomo · b 预览/创建脱敏清单 · B 预览/创建完整备份 · L 检查/确认整体恢复 · Ctrl+P 选择显式代理 · Ctrl+T 配置/预览普通 TUN · Ctrl+L 配置/预览 Linux 网关 · Ctrl+E 启用网络接管 · Ctrl+X 停用网络接管 · [/] 选择来源 · u 添加远程来源 · Ctrl+U 打开来源只读诊断 · Ctrl+G 预览/生成诊断包 · n 添加本机来源 · y 预览所选来源 · f/d/m 刷新所选来源 · t 切换 · k 允许缓存切换 · z 删除 · Ctrl+D 确认删除当前来源 · i 临时导入/预览 · e 添加资源 · o 编辑高级覆盖 · p 应用当前来源 · a 应用临时导入 · s 启动 · x 停止 · g 查询 · w 等待 · c 取消 · v 验证 · r 刷新状态 · q 退出"),
	)
	return tea.NewView(strings.Join(lines, "\n"))
}

func (m *Model) startObserveCmd() tea.Cmd {
	m.observeGeneration++
	return m.observeCmd(m.observeGeneration)
}

func (m Model) observeCmd(generation uint64) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := m.client.Observe(m.ctx)
		if err != nil {
			return snapshotErrorMsg{err: err, generation: generation}
		}
		return snapshotMsg{snapshot: snapshot, generation: generation}
	}
}

func (m Model) watchEventsCmd(after uint64) tea.Cmd {
	return func() tea.Msg {
		var received runtimeapi.Event
		err := m.client.WatchEvents(m.ctx, after, func(event runtimeapi.Event) error {
			received = event
			return errRuntimeEventReceived
		})
		if errors.Is(err, errRuntimeEventReceived) {
			return runtimeEventMsg{event: received}
		}
		return runtimeWatchErrorMsg{err: err}
	}
}

func reconnectCmd(attempt int) tea.Cmd {
	delay := 250 * time.Millisecond
	for index := 1; index < attempt && delay < 5*time.Second; index++ {
		delay *= 2
	}
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return runtimeReconnectMsg{}
	})
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

func (m Model) previewProductUpdateCmd(client ProductUpdateClient) tea.Cmd {
	return func() tea.Msg {
		preview, err := client.PreviewProductUpdate(m.ctx, runtimeapi.ProductUpdatePreviewRequest{
			Source: runtimeapi.ProductUpdateSourceOnlineTUF,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return productUpdateMsg{preview: preview}
	}
}

func (m Model) previewOfflineProductUpdateCmd(client ProductUpdateClient, path string) tea.Cmd {
	return func() tea.Msg {
		body, err := runtimebackupfile.Read(path, runtimeapi.RuntimeProductUpdateMaxBytes)
		if err != nil {
			return errMsg{err: err}
		}
		content, err := m.client.UploadImport(m.ctx, runtimeapi.ProductUpdateBundleContentType, body)
		if err != nil {
			return errMsg{err: err}
		}
		preview, err := client.PreviewProductUpdate(m.ctx, runtimeapi.ProductUpdatePreviewRequest{
			Source:    runtimeapi.ProductUpdateSourceOfflineTUF,
			ContentID: content.ID,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return productUpdateMsg{preview: preview}
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
	return func() tea.Msg {
		content, err := m.client.UploadImport(m.ctx, runtimeapi.SourceDraftContentType, body)
		if err != nil {
			return errMsg{err: err}
		}
		return preparedActionMsg{action: runtimeapi.Action{
			Kind: runtimeapi.ActionAddRemoteSource,
			Params: runtimeapi.ActionParams{
				ContentID: content.ID,
			},
		}}
	}
}

func (m Model) addResourceCmd(draft resourceDraft) tea.Cmd {
	return func() tea.Msg {
		content, err := m.client.UploadImport(
			m.ctx,
			runtimeapi.ManagedResourceContentType,
			[]byte(draft.Content),
		)
		if err != nil {
			return errMsg{err: err}
		}
		return preparedActionMsg{action: runtimeapi.Action{
			Kind: runtimeapi.ActionAddManagedResource,
			Params: runtimeapi.ActionParams{
				ContentID:    content.ID,
				ResourceKind: draft.Kind,
				ResourceName: draft.Name,
			},
		}}
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
	return func() tea.Msg {
		content, err := m.client.UploadImport(m.ctx, "application/x-yaml", body)
		if err != nil {
			return errMsg{err: err}
		}
		return preparedActionMsg{action: runtimeapi.Action{
			Kind: runtimeapi.ActionSetAdvancedOverride,
			Params: runtimeapi.ActionParams{
				ContentID: content.ID,
			},
		}}
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
		return importPreviewMsg{preview: preview, sourceID: sourceID}
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
		return overridePreviewMsg{preview: preview, sourceID: sourceID}
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
			return operationSubmitErrorMsg{action: action, err: err}
		}
		return operationMsg{operation: operation}
	}
}

func (m Model) waitCmd(operationID string) tea.Cmd {
	return func() tea.Msg {
		operation, err := m.client.WaitOperation(m.ctx, operationID, 250*time.Millisecond)
		if err != nil {
			return operationIOErrorMsg{operationID: operationID, err: err}
		}
		return operationMsg{operation: operation, fromWait: true}
	}
}

func (m Model) getCmd(operationID string) tea.Cmd {
	return func() tea.Msg {
		operation, err := m.client.GetOperation(m.ctx, operationID)
		if err != nil {
			return operationIOErrorMsg{operationID: operationID, err: err}
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
			return operationIOErrorMsg{operationID: operationID, mutating: true, err: err}
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
	case runtimeapi.ActionSelectProxyNode:
		return fmt.Sprintf("%s；代理组 %s 已选择 %s", status, operation.Result.ProxyGroup, operation.Result.ProxyNode)
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
	m.sourceDiagnosticOpen = false
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

func defaultResourceDraft() string {
	return `{
  "name": "provider",
  "kind": "proxy-provider-yaml",
  "content": "proxies:\n  - name: example\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n"
}`
}
