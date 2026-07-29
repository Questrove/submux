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
)

type Client interface {
	Observe(context.Context) (runtimeapi.Snapshot, error)
	UploadImport(context.Context, string, []byte) (runtimeapi.ImportContent, error)
	GetAdvancedOverride(context.Context) (runtimeapi.AdvancedOverrideDocument, error)
	PreviewCandidate(context.Context, string) (runtimeapi.CandidatePreview, error)
	PreviewCandidateRequest(context.Context, runtimeapi.PreviewCandidateRequest) (runtimeapi.CandidatePreview, error)
	Execute(context.Context, runtimeapi.CreateOperationRequest) (runtimeapi.Operation, error)
	GetOperation(context.Context, string) (runtimeapi.Operation, error)
	WaitOperation(context.Context, string, time.Duration) (runtimeapi.Operation, error)
	CancelOperation(context.Context, string, runtimeapi.CancelOperationRequest) (runtimeapi.Operation, error)
	VerifyProxy(context.Context) (runtimeapi.ProxyVerification, error)
}

type Model struct {
	ctx              context.Context
	client           Client
	editor           textarea.Model
	editing          bool
	editorMode       string
	busy             bool
	width            int
	height           int
	snapshot         runtimeapi.Snapshot
	selectedSourceID string
	preview          runtimeapi.CandidatePreview
	lastOperation    runtimeapi.Operation
	verification     runtimeapi.ProxyVerification
	status           string
	err              error
}

const (
	editorModeConfig         = "config"
	editorModeSource         = "source"
	editorModeImportedSource = "imported_source"
	editorModeResource       = "resource"
	editorModeOverride       = "override"
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
		m.editor.SetValue(message.document.YAML)
		m.status = "编辑高级覆盖后按 Ctrl+S 校验并保存，Esc 取消"
		return m, m.editor.Focus()
	case overridePreviewMsg:
		m.preview = message.preview
		m.busy = false
		m.err = nil
		m.status = fmt.Sprintf("高级覆盖预览已校验：%s；Ctrl+S 保存", shortDigest(message.preview.CandidateSHA256))
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
	runMode := valueOr(m.snapshot.RunMode, "未配置")
	lines := []string{
		titleStyle.Render("Submux Runtime"),
		"",
		fmt.Sprintf("%s %s  %s %s", labelStyle.Render("Runtime"), runtimeVersion, labelStyle.Render("服务"), serviceState),
		fmt.Sprintf("%s %s  %s %s", labelStyle.Render("Mihomo"), mihomoState, labelStyle.Render("运行方式"), runMode),
		fmt.Sprintf("%s %d  %s %d", labelStyle.Render("Revision"), m.snapshot.Revision, labelStyle.Render("队列"), m.snapshot.Operations.Queued),
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
		mutedStyle.Render("[/] 选择来源 · u 添加远程来源 · n 添加本机来源 · y 预览所选来源 · f/d/m 刷新所选来源 · t 切换 · k 允许缓存切换 · z 删除 · Ctrl+D 确认删除当前来源 · i 临时导入/预览 · e 添加资源 · o 编辑高级覆盖 · p 应用当前来源 · a 应用临时导入 · s 启动 · x 停止 · g 查询 · w 等待 · c 取消 · v 验证 · r 刷新状态 · q 退出"),
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
		document, err := m.client.GetAdvancedOverride(m.ctx)
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
			return candidate.Error() + "；请重启或更新界面"
		}
	}
	return err.Error()
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
	if len(m.snapshot.Sources.Items) == 0 {
		m.selectedSourceID = ""
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
				return
			}
		}
	}
	m.selectedSourceID = m.snapshot.Sources.Items[0].ID
}

func (m *Model) selectAdjacentSource(forward bool) {
	items := m.snapshot.Sources.Items
	if len(items) == 0 {
		m.selectedSourceID = ""
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
	m.selectedSourceID = items[index].ID
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
