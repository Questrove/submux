const invoke = window.__TAURI__.core.invoke;

const state = {
  compatible: false,
  busy: false,
  contentId: "",
  lastOperationId: "",
  currentSourceId: "",
  selectedSourceId: "",
  sources: [],
  mihomoState: "",
};

const elements = {
  compatibility: document.querySelector("#compatibility"),
  runtimeState: document.querySelector("#runtime-state"),
  runtimeVersion: document.querySelector("#runtime-version"),
  mihomoState: document.querySelector("#mihomo-state"),
  mihomoRecovery: document.querySelector("#mihomo-recovery"),
  runMode: document.querySelector("#run-mode"),
  revision: document.querySelector("#revision"),
  queue: document.querySelector("#queue"),
  candidateState: document.querySelector("#candidate-state"),
  candidateYaml: document.querySelector("#candidate-yaml"),
  ownedFields: document.querySelector("#owned-fields"),
  fieldOrigins: document.querySelector("#field-origins"),
  sourceYaml: document.querySelector("#source-yaml"),
  operation: document.querySelector("#operation"),
  proxyVerification: document.querySelector("#proxy-verification"),
  message: document.querySelector("#message"),
  refresh: document.querySelector("#refresh"),
  importPreview: document.querySelector("#import-preview"),
  apply: document.querySelector("#apply"),
  start: document.querySelector("#start"),
  stop: document.querySelector("#stop"),
  verify: document.querySelector("#verify"),
  getOperation: document.querySelector("#get-operation"),
  wait: document.querySelector("#wait"),
  cancelOperation: document.querySelector("#cancel-operation"),
  sourceCount: document.querySelector("#source-count"),
  sourceList: document.querySelector("#source-list"),
  selectedSource: document.querySelector("#selected-source"),
  sourceType: document.querySelector("#remote-source-type"),
  sourceName: document.querySelector("#remote-source-name"),
  sourceUrl: document.querySelector("#remote-source-url"),
  sourceRoute: document.querySelector("#remote-source-route"),
  sourceInterval: document.querySelector("#remote-source-interval"),
  sourceTimeout: document.querySelector("#remote-source-timeout"),
  sourceMaxBytes: document.querySelector("#remote-source-max-bytes"),
  sourceUserAgent: document.querySelector("#remote-source-user-agent"),
  sourceUsername: document.querySelector("#remote-source-username"),
  sourcePassword: document.querySelector("#remote-source-password"),
  sourceAuthorizedTarget: document.querySelector("#remote-source-authorized-target"),
  sourceAllowPrivate: document.querySelector("#remote-source-allow-private"),
  sourceAllowHttp: document.querySelector("#remote-source-allow-http"),
  sourceSkipTls: document.querySelector("#remote-source-skip-tls"),
  sourceCa: document.querySelector("#remote-source-ca"),
  addRemoteSource: document.querySelector("#add-remote-source"),
  localSourceName: document.querySelector("#local-source-name"),
  localSourceYaml: document.querySelector("#local-source-yaml"),
  addImportedSource: document.querySelector("#add-imported-source"),
  applySource: document.querySelector("#apply-source"),
  refreshSource: document.querySelector("#refresh-source"),
  refreshSourceDirect: document.querySelector("#refresh-source-direct"),
  refreshSourceMihomo: document.querySelector("#refresh-source-mihomo"),
  previewSelectedSource: document.querySelector("#preview-selected-source"),
  switchSource: document.querySelector("#switch-source"),
  switchSourceCached: document.querySelector("#switch-source-cached"),
  deleteSource: document.querySelector("#delete-source"),
  resourceCount: document.querySelector("#resource-count"),
  resourceList: document.querySelector("#resource-list"),
  resourceName: document.querySelector("#managed-resource-name"),
  resourceKind: document.querySelector("#managed-resource-kind"),
  resourceContent: document.querySelector("#managed-resource-content"),
  addManagedResource: document.querySelector("#add-managed-resource"),
  overrideState: document.querySelector("#override-state"),
  advancedOverride: document.querySelector("#advanced-override"),
  loadAdvancedOverride: document.querySelector("#load-advanced-override"),
  previewAdvancedOverride: document.querySelector("#preview-advanced-override"),
  saveAdvancedOverride: document.querySelector("#save-advanced-override"),
};

const writeButtons = [
  elements.importPreview,
  elements.apply,
  elements.start,
  elements.stop,
  elements.addRemoteSource,
  elements.addImportedSource,
  elements.applySource,
  elements.refreshSource,
  elements.refreshSourceDirect,
  elements.refreshSourceMihomo,
  elements.switchSource,
  elements.switchSourceCached,
  elements.deleteSource,
  elements.addManagedResource,
  elements.saveAdvancedOverride,
  elements.previewAdvancedOverride,
];

function setBusy(busy, message = "") {
  state.busy = busy;
  for (const button of writeButtons) {
    button.disabled = busy || !state.compatible;
  }
  elements.apply.disabled = busy || !state.compatible || !state.contentId;
  for (const button of [
    elements.refreshSource,
    elements.refreshSourceDirect,
    elements.refreshSourceMihomo,
    elements.previewSelectedSource,
    elements.switchSource,
    elements.switchSourceCached,
    elements.deleteSource,
  ]) {
    button.disabled = busy || !state.compatible || !state.selectedSourceId;
  }
  elements.applySource.disabled = busy || !state.compatible || !state.currentSourceId;
  elements.previewAdvancedOverride.disabled =
    busy || !state.compatible || !state.currentSourceId;
  elements.selectedSource.disabled = busy || !state.sources.length;
  elements.wait.disabled = busy || !state.lastOperationId;
  elements.getOperation.disabled = busy || !state.lastOperationId;
  elements.cancelOperation.disabled = busy || !state.compatible || !state.lastOperationId;
  elements.refresh.disabled = busy;
  elements.verify.disabled = busy;
  elements.loadAdvancedOverride.disabled = busy;
  if (message) {
    setMessage(message);
  }
}

function setMessage(message, isError = false) {
  elements.message.textContent = message;
  elements.message.classList.toggle("error", isError);
}

function errorText(error) {
  if (error && typeof error === "object" && error.message) {
    return error.restart_required ? `${error.message} 请重启或更新界面。` : error.message;
  }
  return String(error);
}

function renderSnapshot(snapshot) {
  elements.runtimeState.textContent = snapshot.runtime?.service_state || "未知";
  elements.runtimeVersion.textContent = snapshot.runtime?.version || "未知版本";
  state.mihomoState = snapshot.mihomo?.state || "";
  const desiredState = snapshot.mihomo?.desired_state || "未设置";
  const recoveryState = snapshot.mihomo?.recovery || "未知";
  const crashAttempts = snapshot.mihomo?.crash_attempts ?? 0;
  const nextRestartAt = snapshot.mihomo?.next_restart_at || "无";
  elements.mihomoState.textContent = `实际：${state.mihomoState || "未知"} · 期望：${desiredState}`;
  const recoverySummary = `恢复：${recoveryState} · 重试：${crashAttempts} · 下次：${nextRestartAt}`;
  elements.mihomoRecovery.textContent = snapshot.mihomo?.fault
    ? `${recoverySummary} · ${snapshot.mihomo.fault.code}`
    : recoverySummary;
  elements.runMode.textContent = `运行方式：${snapshot.run_mode || "未配置"}`;
  elements.revision.textContent = String(snapshot.revision ?? "—");
  elements.queue.textContent = `排队：${snapshot.operations?.queued ?? 0}`;
  const currentOperationId = snapshot.operations?.current_operation_id || "";
  if (currentOperationId) {
    state.lastOperationId = currentOperationId;
    elements.operation.textContent = `当前运行操作：${currentOperationId}`;
  }
  renderSources(snapshot.sources || {});
  renderResources(snapshot.resources || {});
  const advancedOverride = snapshot.advanced_override || {};
  elements.overrideState.textContent = advancedOverride.present
    ? `${String(advancedOverride.sha256 || "").slice(0, 12)} · ${advancedOverride.size || 0} 字节`
    : "尚未配置";
}

function renderSources(sources) {
  const items = Array.isArray(sources.items) ? sources.items : [];
  const previousSelection = state.selectedSourceId;
  state.sources = items;
  state.currentSourceId = sources.current_source_id || "";
  state.selectedSourceId = items.some((source) => source.id === previousSelection)
    ? previousSelection
    : state.currentSourceId || items[0]?.id || "";
  elements.sourceCount.textContent = `${items.length} 个来源`;
  elements.sourceList.replaceChildren();
  elements.selectedSource.replaceChildren();
  if (!items.length) {
    elements.sourceList.textContent = "尚未添加配置来源。";
    const option = document.createElement("option");
    option.value = "";
    option.textContent = "尚无来源";
    elements.selectedSource.append(option);
    setBusy(state.busy);
    return;
  }
  for (const source of items) {
    const option = document.createElement("option");
    option.value = source.id;
    option.textContent =
      `${source.name || source.id} · ${source.type || "未知类型"}` +
      `${source.id === state.currentSourceId || source.current ? " · 当前" : ""}`;
    option.selected = source.id === state.selectedSourceId;
    elements.selectedSource.append(option);

    const item = document.createElement("div");
    item.className = "source-item";
    const title = document.createElement("strong");
    title.textContent =
      `${source.name || source.id} · ${source.type || "未知类型"}` +
      `${source.id === state.currentSourceId || source.current ? " · 当前" : ""}`;
    const target = document.createElement("small");
    target.textContent =
      `${source.id} · ${source.redacted_target || "本机配置副本"} · ${source.route || "本机"}` +
      ` · ${source.last_refresh_result || "尚未刷新"}`;
    item.append(title, target);
    if (Array.isArray(source.high_risk_settings) && source.high_risk_settings.length) {
      const risk = document.createElement("small");
      risk.className = "source-risk";
      risk.textContent = `高风险设置：${source.high_risk_settings.join("、")}`;
      item.append(risk);
    }
    elements.sourceList.append(item);
  }
  setBusy(state.busy);
}

function renderResources(resources) {
  const items = Array.isArray(resources.items) ? resources.items : [];
  elements.resourceCount.textContent = `${items.length} 个资源 · ${resources.total_bytes || 0} 字节`;
  elements.resourceList.replaceChildren();
  if (!items.length) {
    elements.resourceList.textContent = "尚未添加托管资源。";
    return;
  }
  for (const resource of items) {
    const item = document.createElement("div");
    item.className = "source-item";
    const title = document.createElement("strong");
    title.textContent = `${resource.name} · ${resource.kind}`;
    const detail = document.createElement("small");
    detail.textContent = `${resource.id} · ${resource.size} 字节 · ${resource.sha256.slice(0, 12)}`;
    item.append(title, detail);
    elements.resourceList.append(item);
  }
}

async function handshake() {
  setBusy(true, "正在连接本机 Runtime…");
  try {
    const result = await invoke("runtime_handshake");
    state.compatible = result.compatible;
    renderSnapshot(result.snapshot);
    if (!result.compatible) {
      elements.compatibility.textContent =
        `界面版本 ${result.client_version} 与 Runtime ${result.snapshot.runtime.version} 不一致。` +
        "当前只显示状态，请重启或更新界面后再修改。";
      elements.compatibility.classList.remove("hidden");
      setMessage("已进入只读状态。", true);
    } else {
      elements.compatibility.classList.add("hidden");
      setMessage("已连接本机 Runtime。");
    }
  } catch (error) {
    state.compatible = false;
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function importAndPreview() {
  const content = elements.sourceYaml.value;
  if (!content.trim()) {
    setMessage("请先粘贴 Mihomo YAML 配置。", true);
    return;
  }
  setBusy(true, "正在上传本机配置副本…");
  try {
    const imported = await invoke("runtime_import_config", { content });
    state.contentId = imported.content_id;
    setMessage("正在生成并校验候选配置…");
    const preview = await invoke("runtime_preview_candidate", { contentId: state.contentId });
    renderCandidatePreview(preview);
    setMessage(`候选配置可应用；监听 ${preview.proxy_addresses.join("、")}。`);
  } catch (error) {
    state.contentId = "";
    elements.candidateState.textContent = "预览失败";
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

function renderCandidatePreview(preview) {
  elements.candidateYaml.textContent = preview.candidate_yaml;
  elements.candidateState.textContent = `已校验 · ${preview.candidate_sha256.slice(0, 12)}`;
  elements.ownedFields.textContent =
    `Runtime 保留字段：${preview.runtime_owned_fields.join("、")}`;
  const origins = Array.isArray(preview.field_origins) ? preview.field_origins : [];
  elements.fieldOrigins.textContent = origins.length
    ? origins.map((entry) => `${entry.path}\t${entry.origin}\t${entry.status}`).join("\n")
    : "最终候选配置没有可显示的字段来源。";
}

async function previewSelectedSource() {
  if (!state.selectedSourceId) return;
  state.contentId = "";
  setBusy(true, "正在生成所选来源的最终候选配置…");
  try {
    const preview = await invoke("runtime_preview_source", {
      sourceId: state.selectedSourceId,
    });
    renderCandidatePreview(preview);
    setMessage(`所选来源候选配置已校验；监听 ${preview.proxy_addresses.join("、")}。`);
  } catch (error) {
    elements.candidateState.textContent = "预览失败";
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

function renderOperation(operation) {
  state.lastOperationId = operation.id;
  elements.operation.textContent = JSON.stringify(operation, null, 2);
  setBusy(false);
}

function operationResultMessage(operation) {
  const result = operation?.result || {};
  if (operation?.action?.kind === "source.switch" && operation.state === "succeeded") {
    const cache = result.used_cached_source ? "，使用了显式允许的已验证缓存" : "";
    return `来源已从 ${result.previous_source_id || "无"} 切换到 ${result.source_id || "无"}${cache}。`;
  }
  if (operation?.action?.kind === "source.delete" && operation.state === "succeeded") {
    return `来源 ${result.source_id || operation.action.params?.source_id || ""} 已删除。`;
  }
  return `运行操作已结束：${operation.state}。`;
}

async function applyCandidate() {
  setBusy(true, "正在提交候选配置…");
  try {
    renderOperation(await invoke("runtime_apply_candidate", { contentId: state.contentId }));
    state.contentId = "";
    setMessage("候选配置已经进入运行操作队列。Mihomo 停止时不会自动启动。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

function sourceDraft() {
  return {
    type: elements.sourceType.value,
    name: elements.sourceName.value,
    url: elements.sourceUrl.value,
    route: elements.sourceRoute.value,
    user_agent: elements.sourceUserAgent.value,
    username: elements.sourceUsername.value,
    password: elements.sourcePassword.value,
    authorized_target: elements.sourceAuthorizedTarget.value,
    allow_private: elements.sourceAllowPrivate.checked,
    allow_http: elements.sourceAllowHttp.checked,
    custom_ca_pem: elements.sourceCa.value,
    skip_tls_verify: elements.sourceSkipTls.checked,
    refresh_interval_seconds: Number(elements.sourceInterval.value),
    timeout_seconds: Number(elements.sourceTimeout.value),
    max_response_bytes: Number(elements.sourceMaxBytes.value),
  };
}

async function addImportedSource() {
  const name = elements.localSourceName.value.trim();
  const content = elements.localSourceYaml.value;
  if (!name || !content.trim()) {
    setMessage("本机来源名称和配置内容不能为空。", true);
    return;
  }
  setBusy(true, "正在上传并校验本机配置来源…");
  try {
    renderOperation(await invoke("runtime_add_imported_source", { name, content }));
    setMessage("本机来源操作已经进入 Runtime 队列；原文件路径不会保存。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function addRemoteSource() {
  const draft = sourceDraft();
  if (!draft.name.trim() || !draft.url.trim()) {
    setMessage("来源名称和地址不能为空。", true);
    return;
  }
  setBusy(true, "正在下载并校验远程来源…");
  try {
    renderOperation(await invoke("runtime_add_remote_source", {
      draftJson: JSON.stringify(draft),
    }));
    elements.sourcePassword.value = "";
    setMessage("来源操作已经持久化；等待完成后会显示脱敏目标和刷新结果。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function refreshSelectedSource(route) {
  if (!state.selectedSourceId) return;
  setBusy(true, "正在提交所选来源刷新操作…");
  try {
    renderOperation(await invoke("runtime_refresh_source", {
      sourceId: state.selectedSourceId,
      route,
    }));
    setMessage("来源刷新已经进入 Runtime 运行操作队列，不会自动切换线路或来源。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function switchSelectedSource(useCached) {
  if (!state.selectedSourceId) return;
  setBusy(
    true,
    useCached
      ? "正在刷新并切换来源；刷新失败时允许使用已验证缓存…"
      : "正在刷新并切换来源…",
  );
  try {
    renderOperation(await invoke("runtime_switch_source", {
      sourceId: state.selectedSourceId,
      route: "",
      useCached,
    }));
    setMessage(
      useCached
        ? "来源切换已进入 Runtime 队列；只有刷新失败时才允许使用已验证缓存。"
        : "来源切换已进入 Runtime 队列；远程来源必须先刷新成功。",
    );
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function deleteSelectedSource() {
  if (!state.selectedSourceId) return;
  const selected = state.sources.find((source) => source.id === state.selectedSourceId);
  const isCurrent =
    state.selectedSourceId === state.currentSourceId || Boolean(selected?.current);
  const prompt = isCurrent
    ? "这是当前来源。仅当 Mihomo 已停止时才能删除；确认后当前来源会清空。继续吗？"
    : `确认删除来源“${selected?.name || state.selectedSourceId}”吗？`;
  if (!window.confirm(prompt)) return;
  setBusy(true, "正在删除所选来源…");
  try {
    renderOperation(await invoke("runtime_delete_source", {
      sourceId: state.selectedSourceId,
      confirmCurrent: isCurrent,
    }));
    setMessage("来源删除已经进入 Runtime 队列。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function applyCurrentSource() {
  if (!state.currentSourceId) return;
  setBusy(true, "正在提交来源应用操作…");
  try {
    renderOperation(await invoke("runtime_apply_source", {
      sourceId: state.currentSourceId,
    }));
    setMessage("来源应用已经进入 Runtime 队列；Mihomo 停止时仍需显式启动。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function addManagedResource() {
  const name = elements.resourceName.value.trim();
  const kind = elements.resourceKind.value;
  const content = elements.resourceContent.value;
  if (!name || !content.trim()) {
    setMessage("资源名称和内容不能为空。", true);
    return;
  }
  setBusy(true, "正在上传并校验托管资源…");
  try {
    renderOperation(await invoke("runtime_add_managed_resource", {
      name,
      kind,
      content,
    }));
    setMessage("资源操作已经进入 Runtime 队列；内容会保存到托管目录。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function loadAdvancedOverride() {
  setBusy(true, "正在读取高级覆盖…");
  try {
    const document = await invoke("runtime_get_advanced_override");
    elements.advancedOverride.value = document.yaml || "{}\n";
    setMessage("高级覆盖已读取。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function saveAdvancedOverride() {
  const content = elements.advancedOverride.value;
  if (!content.trim()) {
    setMessage("高级覆盖不能为空；如需清空，请填写 {}。", true);
    return;
  }
  setBusy(true, "正在校验并保存高级覆盖…");
  try {
    renderOperation(await invoke("runtime_set_advanced_override", { content }));
    setMessage("高级覆盖操作已经进入 Runtime 队列。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function previewAdvancedOverride() {
  if (!state.currentSourceId) return;
  const content = elements.advancedOverride.value;
  if (!content.trim()) {
    setMessage("高级覆盖不能为空。", true);
    return;
  }
  state.contentId = "";
  setBusy(true, "正在预览尚未保存的高级覆盖…");
  try {
    const preview = await invoke("runtime_preview_override", {
      sourceId: state.currentSourceId,
      content,
    });
    renderCandidatePreview(preview);
    setMessage("尚未保存的高级覆盖已通过最终候选配置校验。");
  } catch (error) {
    elements.candidateState.textContent = "预览失败";
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function executeSimple(command, message) {
  setBusy(true, message);
  try {
    renderOperation(await invoke(command));
    setMessage("运行操作已经持久化，可以等待或关闭 GUI。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function waitOperation() {
  if (!state.lastOperationId) return;
  setBusy(true, "正在等待运行操作完成…");
  try {
    const operation = await invoke("runtime_wait_operation", {
      operationId: state.lastOperationId,
      timeoutMs: 300000,
    });
    renderOperation(operation);
    await refreshStatus();
    setMessage(operationResultMessage(operation), operation.state !== "succeeded");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function getOperation() {
  if (!state.lastOperationId) return;
  setBusy(true, "正在查询运行操作…");
  try {
    renderOperation(await invoke("runtime_get_operation", {
      operationId: state.lastOperationId,
    }));
    setMessage("运行操作状态已更新。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function cancelOperation() {
  if (!state.lastOperationId) return;
  setBusy(true, "正在取消运行操作…");
  try {
    renderOperation(await invoke("runtime_cancel_operation", {
      operationId: state.lastOperationId,
    }));
    setMessage("取消请求已提交。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function verifyProxy() {
  setBusy(true, "正在验证显式代理…");
  try {
    const verification = await invoke("runtime_verify_proxy");
    elements.proxyVerification.textContent = verification.available
      ? `可用 · ${verification.addresses.join("、")}`
      : "不可用";
    setMessage(verification.available ? "显式代理验证通过。" : "显式代理不可用。", !verification.available);
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function refreshStatus() {
  setBusy(true, "正在刷新状态…");
  try {
    const snapshot = await invoke("runtime_observe");
    renderSnapshot(snapshot);
    setMessage("状态已更新。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

elements.refresh.addEventListener("click", refreshStatus);
elements.importPreview.addEventListener("click", importAndPreview);
elements.apply.addEventListener("click", applyCandidate);
elements.start.addEventListener("click", () => executeSimple("runtime_start_proxy", "正在提交启动操作…"));
elements.stop.addEventListener("click", () => executeSimple("runtime_stop_proxy", "正在提交停止操作…"));
elements.verify.addEventListener("click", verifyProxy);
elements.getOperation.addEventListener("click", getOperation);
elements.wait.addEventListener("click", waitOperation);
elements.cancelOperation.addEventListener("click", cancelOperation);
elements.addRemoteSource.addEventListener("click", addRemoteSource);
elements.addImportedSource.addEventListener("click", addImportedSource);
elements.selectedSource.addEventListener("change", () => {
  state.selectedSourceId = elements.selectedSource.value;
  setBusy(state.busy);
  const selected = state.sources.find((source) => source.id === state.selectedSourceId);
  setMessage(`已选择来源：${selected?.name || state.selectedSourceId}。`);
});
elements.applySource.addEventListener("click", applyCurrentSource);
elements.refreshSource.addEventListener("click", () => refreshSelectedSource(""));
elements.refreshSourceDirect.addEventListener("click", () => refreshSelectedSource("direct"));
elements.refreshSourceMihomo.addEventListener("click", () => refreshSelectedSource("mihomo"));
elements.previewSelectedSource.addEventListener("click", previewSelectedSource);
elements.switchSource.addEventListener("click", () => switchSelectedSource(false));
elements.switchSourceCached.addEventListener("click", () => switchSelectedSource(true));
elements.deleteSource.addEventListener("click", deleteSelectedSource);
elements.addManagedResource.addEventListener("click", addManagedResource);
elements.loadAdvancedOverride.addEventListener("click", loadAdvancedOverride);
elements.previewAdvancedOverride.addEventListener("click", previewAdvancedOverride);
elements.saveAdvancedOverride.addEventListener("click", saveAdvancedOverride);

handshake();
