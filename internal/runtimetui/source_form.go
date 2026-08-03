package runtimetui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type sourceFormKind string

const (
	sourceFormRemote sourceFormKind = "remote"
	sourceFormLocal  sourceFormKind = "local"

	sourceFieldName             = "name"
	sourceFieldURL              = "url"
	sourceFieldPath             = "path"
	sourceFieldRoute            = "route"
	sourceFieldRefreshInterval  = "refresh_interval_seconds"
	sourceFieldTimeout          = "timeout_seconds"
	sourceFieldMaximumBytes     = "max_response_bytes"
	sourceFieldUserAgent        = "user_agent"
	sourceFieldUsername         = "username"
	sourceFieldPassword         = "password"
	sourceFieldAuthorizedTarget = "authorized_target"
	sourceFieldAllowPrivate     = "allow_private"
	sourceFieldAllowHTTP        = "allow_http"
	sourceFieldSkipTLSVerify    = "skip_tls_verify"
	sourceFieldCustomCAPath     = "custom_ca_path"

	maximumLocalSourceBytes = 8 << 20
	maximumCustomCABytes    = 1 << 20
)

type structuredField struct {
	key         string
	label       string
	value       string
	placeholder string
	toggle      bool
	secret      bool
	options     []string
}

type structuredForm struct {
	fields []structuredField
	index  int
	input  textinput.Model
}

type sourceForm struct {
	kind sourceFormKind
	*structuredForm
}

func newRemoteSourceForm() *sourceForm {
	return newSourceForm(sourceFormRemote, []structuredField{
		{key: sourceFieldName, label: "来源名称", value: "primary"},
		{key: sourceFieldURL, label: "配置地址", value: "https://example.com/config.yaml"},
		{key: sourceFieldRoute, label: "刷新路线", value: runtimeapi.SourceRouteDirect, options: []string{runtimeapi.SourceRouteDirect, runtimeapi.SourceRouteMihomo}},
		{key: sourceFieldRefreshInterval, label: "刷新间隔（秒）", value: "21600"},
		{key: sourceFieldTimeout, label: "超时（秒）", value: "30"},
		{key: sourceFieldMaximumBytes, label: "最大响应字节", value: "8388608"},
		{key: sourceFieldUserAgent, label: "User-Agent"},
		{key: sourceFieldUsername, label: "用户名"},
		{key: sourceFieldPassword, label: "密码", secret: true},
		{key: sourceFieldAuthorizedTarget, label: "授权目标", placeholder: "高风险地址需填写 host:port"},
		{key: sourceFieldCustomCAPath, label: "自定义 CA PEM 文件"},
		{key: sourceFieldAllowPrivate, label: "允许私网目标", toggle: true},
		{key: sourceFieldAllowHTTP, label: "允许明文 HTTP", toggle: true},
		{key: sourceFieldSkipTLSVerify, label: "跳过 TLS 校验", toggle: true},
	})
}

func newLocalSourceForm() *sourceForm {
	return newSourceForm(sourceFormLocal, []structuredField{
		{key: sourceFieldName, label: "来源名称", value: "local-copy"},
		{key: sourceFieldPath, label: "本机文件", placeholder: "Mihomo YAML 文件路径"},
	})
}

func newSourceForm(kind sourceFormKind, fields []structuredField) *sourceForm {
	return &sourceForm{kind: kind, structuredForm: newStructuredForm(fields)}
}

func newStructuredForm(fields []structuredField) *structuredForm {
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 4096
	input.SetWidth(72)
	form := &structuredForm{fields: fields, input: input}
	form.loadActiveField()
	return form
}

func (form *structuredForm) setValue(key string, value string) {
	for index := range form.fields {
		if form.fields[index].key != key {
			continue
		}
		form.fields[index].value = value
		if index == form.index && !form.fields[index].toggle {
			form.input.SetValue(value)
		}
		return
	}
}

func (form *structuredForm) value(key string) string {
	for _, field := range form.fields {
		if field.key == key {
			return field.value
		}
	}
	return ""
}

func (form *structuredForm) commitActiveField() {
	if len(form.fields) == 0 || form.fields[form.index].toggle || len(form.fields[form.index].options) > 0 {
		return
	}
	form.fields[form.index].value = form.input.Value()
}

func (form *structuredForm) loadActiveField() tea.Cmd {
	if len(form.fields) == 0 {
		return nil
	}
	field := form.fields[form.index]
	if field.toggle || len(field.options) > 0 {
		form.input.Blur()
		return nil
	}
	form.input.Placeholder = field.placeholder
	form.input.EchoMode = textinput.EchoNormal
	if field.secret {
		form.input.EchoMode = textinput.EchoPassword
	}
	form.input.SetValue(field.value)
	return form.input.Focus()
}

func (form *structuredForm) move(offset int) tea.Cmd {
	if len(form.fields) == 0 {
		return nil
	}
	form.commitActiveField()
	form.index = (form.index + offset + len(form.fields)) % len(form.fields)
	return form.loadActiveField()
}

func (form *structuredForm) toggleActiveField() bool {
	if len(form.fields) == 0 || !form.fields[form.index].toggle {
		return false
	}
	value := form.fields[form.index].value != "true"
	form.fields[form.index].value = strconv.FormatBool(value)
	return true
}

func (form *structuredForm) cycleActiveOption(offset int) bool {
	if len(form.fields) == 0 || len(form.fields[form.index].options) == 0 {
		return false
	}
	field := &form.fields[form.index]
	optionIndex := 0
	for index, option := range field.options {
		if option == field.value {
			optionIndex = index
			break
		}
	}
	optionIndex = (optionIndex + offset + len(field.options)) % len(field.options)
	field.value = field.options[optionIndex]
	return true
}

func (form *structuredForm) close() {
	form.input.Blur()
	for index := range form.fields {
		if form.fields[index].secret || form.fields[index].key == sourceFieldPath {
			form.fields[index].value = ""
		}
	}
}

func (form *sourceForm) remoteDraft() (runtimeapi.RemoteSourceDraft, error) {
	form.commitActiveField()
	name := strings.TrimSpace(form.value(sourceFieldName))
	url := strings.TrimSpace(form.value(sourceFieldURL))
	if name == "" || url == "" {
		return runtimeapi.RemoteSourceDraft{}, errors.New("远程来源必须填写来源名称和配置地址")
	}
	route := strings.TrimSpace(form.value(sourceFieldRoute))
	if route != runtimeapi.SourceRouteDirect && route != runtimeapi.SourceRouteMihomo {
		return runtimeapi.RemoteSourceDraft{}, errors.New("刷新路线必须是 direct 或 mihomo")
	}
	refresh, err := positiveInt64(form.value(sourceFieldRefreshInterval), "刷新间隔")
	if err != nil {
		return runtimeapi.RemoteSourceDraft{}, err
	}
	timeout, err := positiveInt(form.value(sourceFieldTimeout), "超时")
	if err != nil {
		return runtimeapi.RemoteSourceDraft{}, err
	}
	maximum, err := positiveInt64(form.value(sourceFieldMaximumBytes), "最大响应字节")
	if err != nil {
		return runtimeapi.RemoteSourceDraft{}, err
	}
	customCA, err := readOptionalFile(form.value(sourceFieldCustomCAPath), maximumCustomCABytes)
	if err != nil {
		return runtimeapi.RemoteSourceDraft{}, fmt.Errorf("读取自定义 CA PEM 文件: %w", err)
	}
	return runtimeapi.RemoteSourceDraft{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   name,
		URL:                    url,
		Route:                  route,
		UserAgent:              strings.TrimSpace(form.value(sourceFieldUserAgent)),
		Username:               strings.TrimSpace(form.value(sourceFieldUsername)),
		Password:               form.value(sourceFieldPassword),
		AuthorizedTarget:       strings.TrimSpace(form.value(sourceFieldAuthorizedTarget)),
		AllowPrivate:           form.value(sourceFieldAllowPrivate) == "true",
		AllowHTTP:              form.value(sourceFieldAllowHTTP) == "true",
		CustomCAPEM:            customCA,
		SkipTLSVerify:          form.value(sourceFieldSkipTLSVerify) == "true",
		RefreshIntervalSeconds: &refresh,
		TimeoutSeconds:         timeout,
		MaxResponseBytes:       maximum,
	}, nil
}

func positiveInt(value string, label string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s必须是正整数", label)
	}
	return parsed, nil
}

func positiveInt64(value string, label string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s必须是正整数", label)
	}
	return parsed, nil
}

func readOptionalFile(path string, maximum int64) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	body, err := readBoundedFile(path, maximum)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, fmt.Errorf("文件超过 %d 字节限制", maximum)
	}
	return body, nil
}

func (m Model) updateSourceForm(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.sourceForm == nil {
		return m, nil
	}
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			m.sourceForm.close()
			m.sourceForm = nil
			m.err = nil
			m.status = "已取消添加配置来源"
			return m, nil
		case "tab", "down", "enter":
			return m, m.sourceForm.move(1)
		case "shift+tab", "up":
			return m, m.sourceForm.move(-1)
		case "left":
			if m.sourceForm.cycleActiveOption(-1) {
				return m, nil
			}
		case "right":
			if m.sourceForm.cycleActiveOption(1) {
				return m, nil
			}
		case " ":
			if m.sourceForm.toggleActiveField() {
				return m, nil
			}
			if m.sourceForm.cycleActiveOption(1) {
				return m, nil
			}
		case "ctrl+s":
			return m.submitSourceForm()
		}
	}
	if len(m.sourceForm.fields) == 0 || m.sourceForm.fields[m.sourceForm.index].toggle {
		return m, nil
	}
	var command tea.Cmd
	m.sourceForm.input, command = m.sourceForm.input.Update(message)
	return m, command
}

func (m Model) submitSourceForm() (tea.Model, tea.Cmd) {
	form := m.sourceForm
	if form == nil || m.busy {
		return m, nil
	}
	if form.kind == sourceFormRemote {
		draft, err := form.remoteDraft()
		if err != nil {
			m.err = err
			m.status = err.Error()
			return m, nil
		}
		body, err := json.Marshal(draft)
		if err != nil {
			m.err = err
			m.status = publicErrorMessage(err)
			return m, nil
		}
		form.close()
		m.sourceForm = nil
		m.busy = true
		m.err = nil
		m.status = "正在上传并添加远程来源…"
		return m, m.addSourceCmd(body)
	}

	form.commitActiveField()
	name := strings.TrimSpace(form.value(sourceFieldName))
	path := strings.TrimSpace(form.value(sourceFieldPath))
	if name == "" || path == "" {
		m.err = errors.New("本机来源必须填写来源名称和本机文件")
		m.status = m.err.Error()
		return m, nil
	}
	form.close()
	m.sourceForm = nil
	m.busy = true
	m.err = nil
	m.status = "正在读取、上传并添加本机配置来源…"
	return m, m.addImportedSourceFileCmd(name, path)
}

func (m Model) addImportedSourceFileCmd(name string, path string) tea.Cmd {
	return func() tea.Msg {
		body, err := readBoundedFile(path, maximumLocalSourceBytes)
		if err != nil {
			return errMsg{err: fmt.Errorf("读取本机配置来源: %w", err)}
		}
		content, err := m.client.UploadImport(m.ctx, "application/x-yaml", body)
		if err != nil {
			return errMsg{err: err}
		}
		return preparedActionMsg{action: runtimeapi.Action{
			Kind: runtimeapi.ActionAddImportedSource,
			Params: runtimeapi.ActionParams{
				ContentID:  content.ID,
				SourceName: name,
			},
		}}
	}
}

func (m Model) renderSourceForm() string {
	form := m.sourceForm
	if form == nil {
		return ""
	}
	title := "添加远程配置来源"
	help := "Tab / Shift+Tab 切换字段 · Space 切换选项 · Ctrl+S 进入统一确认 · Esc 取消"
	if form.kind == sourceFormLocal {
		title = "添加本机配置来源"
		help = "文件只在本机读取并上传内容，路径不会发送给 Runtime · Ctrl+S 进入统一确认"
	}
	lines := []string{titleStyle.Render(title), mutedStyle.Render(help), ""}
	for index, field := range form.fields {
		prefix := "  "
		if index == form.index {
			prefix = "▶ "
		}
		value := field.value
		if index == form.index && !field.toggle && len(field.options) == 0 {
			value = form.input.View()
		} else if field.toggle {
			value = "[ ]"
			if field.value == "true" {
				value = "[x]"
			}
		} else if len(field.options) > 0 {
			value = "[" + value + "]  ←/→"
		} else if field.secret && value != "" {
			value = strings.Repeat("•", len([]rune(value)))
		} else if value == "" {
			value = mutedStyle.Render("未填写")
		}
		lines = append(lines, fmt.Sprintf("%s%-22s %s", prefix, field.label, value))
	}
	lines = append(lines, "", renderStatus(m.status, m.err, m.busy))
	return strings.Join(lines, "\n")
}

func (m Model) renderSourceDiagnostics() string {
	sourceName := "未知来源"
	sourceID := m.selectedSource()
	if source := m.selectedSourceSummary(); source != nil {
		sourceName = source.Name + " · " + source.Type + " · " + source.ID
	}
	lines := []string{
		titleStyle.Render("配置来源 · 只读诊断"),
		warnStyle.Render(runtimeapi.SensitiveDataWarning),
		mutedStyle.Render("此视图不可编辑；Esc 或 Ctrl+U 关闭后立即清除临时显示的原始地址。"),
		"",
		labelStyle.Render("所选来源"),
		sourceName,
		"",
		labelStyle.Render("原始请求地址"),
		valueOr(m.revealedURL, "未读取"),
	}
	lines = append(lines, "", labelStyle.Render("候选配置正文"))
	if m.previewSourceID == sourceID && strings.TrimSpace(m.preview.CandidateYAML) != "" {
		lines = append(lines, m.preview.CandidateYAML)
	} else {
		lines = append(lines, mutedStyle.Render("尚未为所选来源生成候选配置"))
	}
	return strings.Join(lines, "\n")
}
