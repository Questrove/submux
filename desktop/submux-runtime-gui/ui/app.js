const invoke = window.__TAURI__.core.invoke;

const state = {
  compatible: false,
  busy: false,
  contentId: "",
  lastOperationId: "",
  currentSourceId: "",
};

const elements = {
  compatibility: document.querySelector("#compatibility"),
  runtimeState: document.querySelector("#runtime-state"),
  runtimeVersion: document.querySelector("#runtime-version"),
  mihomoState: document.querySelector("#mihomo-state"),
  runMode: document.querySelector("#run-mode"),
  revision: document.querySelector("#revision"),
  queue: document.querySelector("#queue"),
  candidateState: document.querySelector("#candidate-state"),
  candidateYaml: document.querySelector("#candidate-yaml"),
  ownedFields: document.querySelector("#owned-fields"),
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
  applySource: document.querySelector("#apply-source"),
  refreshSource: document.querySelector("#refresh-source"),
  refreshSourceDirect: document.querySelector("#refresh-source-direct"),
  refreshSourceMihomo: document.querySelector("#refresh-source-mihomo"),
};

const writeButtons = [
  elements.importPreview,
  elements.apply,
  elements.start,
  elements.stop,
  elements.addRemoteSource,
  elements.applySource,
  elements.refreshSource,
  elements.refreshSourceDirect,
  elements.refreshSourceMihomo,
];

function setBusy(busy, message = "") {
  state.busy = busy;
  for (const button of writeButtons) {
    button.disabled = busy || !state.compatible || (button === elements.apply && !state.contentId);
  }
  for (const button of [
    elements.refreshSource,
    elements.refreshSourceDirect,
    elements.refreshSourceMihomo,
    elements.applySource,
  ]) {
    button.disabled = busy || !state.compatible || !state.currentSourceId;
  }
  elements.wait.disabled = busy || !state.lastOperationId;
  elements.getOperation.disabled = busy || !state.lastOperationId;
  elements.cancelOperation.disabled = busy || !state.compatible || !state.lastOperationId;
  elements.refresh.disabled = busy;
  elements.verify.disabled = busy;
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
  elements.mihomoState.textContent = snapshot.mihomo?.state || "未知";
  elements.runMode.textContent = `运行方式：${snapshot.run_mode || "未配置"}`;
  elements.revision.textContent = String(snapshot.revision ?? "—");
  elements.queue.textContent = `排队：${snapshot.operations?.queued ?? 0}`;
  const currentOperationId = snapshot.operations?.current_operation_id || "";
  if (currentOperationId) {
    state.lastOperationId = currentOperationId;
    elements.operation.textContent = `当前运行操作：${currentOperationId}`;
  }
  renderSources(snapshot.sources || {});
}

function renderSources(sources) {
  const items = Array.isArray(sources.items) ? sources.items : [];
  state.currentSourceId = sources.current_source_id || "";
  elements.sourceCount.textContent = `${items.length} 个来源`;
  elements.sourceList.replaceChildren();
  if (!items.length) {
    elements.sourceList.textContent = "尚未添加远程来源。";
    return;
  }
  for (const source of items) {
    const item = document.createElement("div");
    item.className = "source-item";
    const title = document.createElement("strong");
    title.textContent = `${source.name || source.id}${source.id === state.currentSourceId ? " · 当前" : ""}`;
    const target = document.createElement("small");
    target.textContent = `${source.redacted_target} · ${source.route} · ${source.last_refresh_result || "尚未刷新"}`;
    item.append(title, target);
    if (Array.isArray(source.high_risk_settings) && source.high_risk_settings.length) {
      const risk = document.createElement("small");
      risk.className = "source-risk";
      risk.textContent = `高风险设置：${source.high_risk_settings.join("、")}`;
      item.append(risk);
    }
    elements.sourceList.append(item);
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
    elements.candidateYaml.textContent = preview.candidate_yaml;
    elements.candidateState.textContent = `已校验 · ${preview.candidate_sha256.slice(0, 12)}`;
    elements.ownedFields.textContent =
      `Runtime 保留字段：${preview.runtime_owned_fields.join("、")}`;
    setMessage(`候选配置可应用；监听 ${preview.proxy_addresses.join("、")}。`);
  } catch (error) {
    state.contentId = "";
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

async function refreshCurrentSource(route) {
  if (!state.currentSourceId) return;
  setBusy(true, "正在提交来源刷新操作…");
  try {
    renderOperation(await invoke("runtime_refresh_source", {
      sourceId: state.currentSourceId,
      route,
    }));
    setMessage("来源刷新已经进入 Runtime 运行操作队列，不会自动切换线路或来源。");
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
    setMessage(`运行操作已结束：${operation.state}。`, operation.state !== "succeeded");
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
elements.applySource.addEventListener("click", applyCurrentSource);
elements.refreshSource.addEventListener("click", () => refreshCurrentSource(""));
elements.refreshSourceDirect.addEventListener("click", () => refreshCurrentSource("direct"));
elements.refreshSourceMihomo.addEventListener("click", () => refreshCurrentSource("mihomo"));

handshake();
