package runtimetui

import (
	"context"
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
	PreviewCandidate(context.Context, string) (runtimeapi.CandidatePreview, error)
	Execute(context.Context, runtimeapi.CreateOperationRequest) (runtimeapi.Operation, error)
	GetOperation(context.Context, string) (runtimeapi.Operation, error)
	WaitOperation(context.Context, string, time.Duration) (runtimeapi.Operation, error)
	CancelOperation(context.Context, string, runtimeapi.CancelOperationRequest) (runtimeapi.Operation, error)
	VerifyProxy(context.Context) (runtimeapi.ProxyVerification, error)
}

type Model struct {
	ctx           context.Context
	client        Client
	editor        textarea.Model
	editing       bool
	busy          bool
	width         int
	height        int
	snapshot      runtimeapi.Snapshot
	preview       runtimeapi.CandidatePreview
	lastOperation runtimeapi.Operation
	verification  runtimeapi.ProxyVerification
	status        string
	err           error
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
		m.err = nil
		m.status = fmt.Sprintf("运行操作 %s：%s / %s", message.operation.ID, message.operation.State, message.operation.Stage)
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
				m.status = "正在上传并校验候选配置…"
				return m, m.importPreviewCmd(body)
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
			m.err = nil
			m.status = "粘贴配置后按 Ctrl+S 上传并预览，Esc 取消"
			return m, m.editor.Focus()
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
		}
	}
	return m, nil
}

func (m Model) View() tea.View {
	if m.editing {
		content := strings.Join([]string{
			titleStyle.Render("Submux Runtime · 导入本机配置副本"),
			mutedStyle.Render("Ctrl+S 上传并预览 · Esc 取消"),
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
	if m.preview.ContentID != "" {
		lines = append(lines,
			"",
			labelStyle.Render("候选配置"),
			fmt.Sprintf("%s · %s · %s", shortDigest(m.preview.CandidateSHA256), m.preview.ProxyKind, strings.Join(m.preview.ProxyAddresses, ", ")),
		)
	}
	if m.lastOperation.ID != "" {
		lines = append(lines,
			"",
			labelStyle.Render("最近运行操作"),
			fmt.Sprintf("%s · %s · %s · %d%%", m.lastOperation.ID, m.lastOperation.State, m.lastOperation.Stage, m.lastOperation.Progress),
		)
	}
	lines = append(lines,
		"",
		renderStatus(m.status, m.err, m.busy),
		"",
		mutedStyle.Render("i 导入/预览 · a 应用 · s 启动 · x 停止 · g 查询 · w 等待 · c 取消 · v 验证 · r 刷新 · q 退出"),
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
