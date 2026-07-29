const invoke = window.__TAURI__.core.invoke;

const state = {
  compatible: false,
  busy: false,
  contentId: "",
  lastOperationId: "",
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
};

const writeButtons = [elements.importPreview, elements.apply, elements.start, elements.stop];

function setBusy(busy, message = "") {
  state.busy = busy;
  for (const button of writeButtons) {
    button.disabled = busy || !state.compatible || (button === elements.apply && !state.contentId);
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

handshake();
