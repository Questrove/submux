const invoke = window.__TAURI__.core.invoke;
const sensitiveDataWarning =
  "敏感内容可能包含访问凭据、配置正文、完整日志或本机信息；仅在确认当前显示与保存环境安全时继续。";

const state = {
  compatible: false,
  busy: false,
  contentId: "",
  lastOperationId: "",
  currentSourceId: "",
  selectedSourceId: "",
  sources: [],
  mihomoState: "",
  networkPlanId: "",
  networkPlanMode: "",
  networkPlanExpiresAt: 0,
  networkPlanExpiryTimer: null,
  activeNetworkMode: "",
  mihomoUpdatePlan: null,
  previousMihomoVersion: "",
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
  networkState: document.querySelector("#network-state"),
  networkMode: document.querySelector("#network-mode"),
  networkIpv6: document.querySelector("#network-ipv6"),
  networkDns: document.querySelector("#network-dns"),
  networkTcp: document.querySelector("#network-tcp"),
  networkUdp: document.querySelector("#network-udp"),
  networkHost: document.querySelector("#network-host"),
  networkDnsDirect: document.querySelector("#network-dns-direct"),
  networkUdpExceptions: document.querySelector("#network-udp-exceptions"),
  networkHostExceptions: document.querySelector("#network-host-exceptions"),
  networkRouteList: document.querySelector("#network-route-list"),
  networkPreview: document.querySelector("#network-preview"),
  previewNetwork: document.querySelector("#preview-network"),
  enableTun: document.querySelector("#enable-tun"),
  disableTun: document.querySelector("#disable-tun"),
  mihomoCoreVersion: document.querySelector("#mihomo-core-version"),
  mihomoUpdateSource: document.querySelector("#mihomo-update-source"),
  mihomoUpdateVersion: document.querySelector("#mihomo-update-version"),
  mihomoUpdatePreview: document.querySelector("#mihomo-update-preview"),
  previewMihomoUpdate: document.querySelector("#preview-mihomo-update"),
  installMihomoUpdate: document.querySelector("#install-mihomo-update"),
  rollbackMihomo: document.querySelector("#rollback-mihomo"),
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
  revealSourceUrl: document.querySelector("#reveal-source-url"),
  revealedSourceUrl: document.querySelector("#revealed-source-url"),
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
  diagnosticsRawConfig: document.querySelector("#diagnostics-raw-config"),
  diagnosticsFullLogs: document.querySelector("#diagnostics-full-logs"),
  diagnosticsNetworkInfo: document.querySelector("#diagnostics-network-info"),
  previewDiagnostics: document.querySelector("#preview-diagnostics"),
  createDiagnostics: document.querySelector("#create-diagnostics"),
  diagnosticsPreview: document.querySelector("#diagnostics-preview"),
};

const writeButtons = [
  elements.importPreview,
  elements.apply,
  elements.start,
  elements.stop,
  elements.previewNetwork,
  elements.enableTun,
  elements.disableTun,
  elements.previewMihomoUpdate,
  elements.installMihomoUpdate,
  elements.rollbackMihomo,
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
  elements.createDiagnostics,
];

function setBusy(busy, message = "") {
  state.busy = busy;
  for (const button of writeButtons) {
    button.disabled = busy || !state.compatible;
  }
  elements.apply.disabled = busy || !state.compatible || !state.contentId;
  elements.enableTun.disabled = busy || !state.compatible || !state.networkPlanId;
  elements.installMihomoUpdate.disabled =
    busy || !state.compatible || !state.mihomoUpdatePlan;
  elements.rollbackMihomo.disabled =
    busy || !state.compatible || !state.previousMihomoVersion;
  for (const button of [
    elements.refreshSource,
    elements.refreshSourceDirect,
    elements.refreshSourceMihomo,
    elements.previewSelectedSource,
    elements.switchSource,
    elements.switchSourceCached,
    elements.deleteSource,
    elements.revealSourceUrl,
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

function diagnosticsOptions() {
  return {
    includeRawConfig: elements.diagnosticsRawConfig.checked,
    includeFullLogs: elements.diagnosticsFullLogs.checked,
    includeNetworkInfo: elements.diagnosticsNetworkInfo.checked,
  };
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

function clearNetworkPlan() {
  state.networkPlanId = "";
  state.networkPlanMode = "";
  state.networkPlanExpiresAt = 0;
  if (state.networkPlanExpiryTimer !== null) {
    window.clearTimeout(state.networkPlanExpiryTimer);
    state.networkPlanExpiryTimer = null;
  }
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
  const updates = snapshot.updates || {};
  state.previousMihomoVersion = updates.mihomo_previous_version || "";
  elements.mihomoCoreVersion.textContent =
    updates.mihomo_current_version || snapshot.mihomo?.version || "未安装";
  const network = snapshot.network || {};
  state.activeNetworkMode = network.mode || "";
  elements.networkState.textContent =
    `${network.state || "未知"} · ${network.mode || "未配置"}` +
    `${network.preview_only ? " · 预览功能" : ""}` +
    `${network.device ? ` · ${network.device}` : ""}`;
  const networkDetails = [];
  if (network.preview_only) {
    networkDetails.push("预览功能：仍需通过真实物理 Linux 网关验收。");
  }
  if (network.fault) {
    networkDetails.push(`${network.fault.code}: ${network.fault.message}`);
  }
  for (const conflict of network.conflicts || []) {
    networkDetails.push(
      `冲突：${conflict.kind} · ${conflict.detail}${conflict.owner ? ` · ${conflict.owner}` : ""}`,
    );
  }
  for (const residual of network.residuals || []) {
    networkDetails.push(`残留：${residual.kind} · ${residual.name} · ${residual.state}`);
  }
  if (!state.networkPlanId) {
    if (["tun", "gateway"].includes(network.mode)) {
      elements.networkMode.value = network.mode;
    }
    const settings = network.gateway_settings || network.settings || {};
    if (["proxy", "direct", "block"].includes(settings.ipv6_policy)) {
      elements.networkIpv6.value = settings.ipv6_policy;
    }
    if (["hijack", "off"].includes(settings.dns_policy)) {
      elements.networkDns.value = settings.dns_policy;
    }
    if (network.gateway_settings) {
      elements.networkTcp.checked = Boolean(settings.capture_tcp);
      elements.networkUdp.checked = Boolean(settings.capture_udp);
      elements.networkHost.checked = Boolean(settings.proxy_host_traffic);
      elements.networkDnsDirect.value = (settings.dns_direct_cidrs || []).join("\n");
      elements.networkUdpExceptions.value = JSON.stringify(settings.udp_exceptions || [], null, 2);
      elements.networkHostExceptions.value = JSON.stringify(settings.host_exceptions || [], null, 2);
    }
    renderNetworkRoutes(network.routes || []);
    elements.networkPreview.textContent = networkDetails.length
      ? networkDetails.join("\n")
      : "当前状态未报告网络冲突、残留或故障。";
  }
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

function renderMihomoUpdatePlan(plan) {
  state.mihomoUpdatePlan = plan;
  elements.mihomoUpdatePreview.textContent = [
    `版本：${plan.version}`,
    `来源：${plan.repository} · ${plan.source}`,
    `信任：${plan.trust}`,
    `平台：${plan.platform}/${plan.arch}`,
    `资产：${plan.asset_name} · ${plan.asset_size} 字节`,
    `SHA-256：${plan.asset_sha256}`,
    `当前：${plan.current_version || "未安装"} · 上一版：${plan.previous_version || "无"}`,
    `当前候选配置静态检查：${plan.static_config_verified ? "通过" : "未通过"}`,
    `计划有效期：${plan.expires_at}`,
    plan.warning || "",
  ]
    .filter(Boolean)
    .join("\n");
  setBusy(false);
}

async function previewMihomoUpdate() {
  const source = elements.mihomoUpdateSource.value;
  const version = elements.mihomoUpdateVersion.value.trim();
  if (source === "upstream_only" && !version) {
    setMessage("仅上游路径必须填写精确稳定版本。", true);
    return;
  }
  state.mihomoUpdatePlan = null;
  setBusy(true, "正在验证 TUF 元数据、官方 Release 和当前候选配置…");
  try {
    const plan = await invoke("runtime_preview_mihomo_update", { source, version });
    renderMihomoUpdatePlan(plan);
    setMessage("候选核心已经验证。安装仍需再次明确确认。");
  } catch (error) {
    setBusy(false);
    setMessage(errorText(error), true);
  }
}

async function installMihomoUpdate() {
  const plan = state.mihomoUpdatePlan;
  if (!plan) return;
  const warning = plan.warning ? `\n\n${plan.warning}` : "";
  if (
    !window.confirm(
      `确认把 Mihomo ${plan.current_version || "未安装"} 更新为 ${plan.version}？代理会短暂停止。${warning}`,
    )
  ) {
    return;
  }
  setBusy(true, "正在提交已确认的 Mihomo 核心安装…");
  try {
    const operation = await invoke("runtime_install_mihomo_update", {
      planId: plan.plan_id,
      trust: plan.trust,
      confirm: true,
    });
    state.mihomoUpdatePlan = null;
    renderOperation(operation);
    elements.mihomoUpdatePreview.textContent = "更新计划已消费；再次安装必须重新检查并确认。";
    setMessage("Mihomo 更新已经进入 Runtime 操作队列。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function rollbackMihomo() {
  if (!state.previousMihomoVersion) return;
  if (
    !window.confirm(
      `确认回滚到 Mihomo ${state.previousMihomoVersion}？代理会短暂停止，回滚后的启动和即时健康检查仍必须通过。`,
    )
  ) {
    return;
  }
  setBusy(true, "正在提交已确认的 Mihomo 核心回滚…");
  try {
    renderOperation(await invoke("runtime_rollback_mihomo", { confirm: true }));
    setMessage("Mihomo 回滚已经进入 Runtime 操作队列。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

function selectedCapturedRoutes() {
  return Array.from(
    elements.networkRouteList.querySelectorAll('input[type="checkbox"]:checked'),
    (input) => input.value,
  );
}

function selectedGatewayExcludedRoutes() {
  return Array.from(
    elements.networkRouteList.querySelectorAll(
      'input[type="checkbox"][data-role="gateway_lan"]:not(:checked)',
    ),
    (input) => input.value,
  );
}

function parseGatewayExceptions(value, label) {
  let parsed;
  try {
    parsed = JSON.parse(value || "[]");
  } catch (error) {
    throw new Error(`${label}必须是有效 JSON 数组：${errorText(error)}`);
  }
  if (!Array.isArray(parsed)) {
    throw new Error(`${label}必须是 JSON 数组。`);
  }
  return parsed;
}

function gatewayDNSDirectCIDRs() {
  return elements.networkDnsDirect.value
    .split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean);
}

function renderNetworkRoutes(routes) {
  elements.networkRouteList.replaceChildren();
  if (!routes.length) {
    elements.networkRouteList.textContent = "没有发现需要逐项决定的更具体路由。";
  }
  for (const route of routes) {
    const item = document.createElement("label");
    item.className = "source-item";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.value = route.id;
    checkbox.checked = !route.bypass;
    checkbox.dataset.role = route.role || "";
    if (route.role && route.role !== "gateway_lan") {
      checkbox.disabled = true;
    }
    const title = document.createElement("strong");
    title.textContent = `${route.cidr} · ${route.interface} · ${route.family}`;
    const detail = document.createElement("small");
    const instruction =
      route.role === "gateway_lan"
        ? "勾选后纳入网关接管"
        : route.role
          ? "固定直连"
          : "勾选后纳入 TUN";
    detail.textContent = `${route.id} · ${route.source || "未知来源"} · ${route.role || "tun"} · ${instruction}`;
    item.append(checkbox, title, detail);
    elements.networkRouteList.append(item);
  }
}

function renderNetworkPreview(preview) {
  const routes = Array.isArray(preview.routes) ? preview.routes : [];
  const conflicts = Array.isArray(preview.conflicts) ? preview.conflicts : [];
  const warnings = Array.isArray(preview.warnings) ? preview.warnings : [];
  clearNetworkPlan();
  const expiresAt = Date.parse(preview.expires_at || "");
  if (!conflicts.length && preview.plan_id && Number.isFinite(expiresAt) && expiresAt > Date.now()) {
    state.networkPlanId = preview.plan_id;
    state.networkPlanMode = preview.mode;
    state.networkPlanExpiresAt = expiresAt;
    state.networkPlanExpiryTimer = window.setTimeout(() => {
      clearNetworkPlan();
      setBusy(state.busy);
      setMessage("网络预览已经过期，请重新生成。");
    }, Math.min(expiresAt - Date.now(), 2_147_483_647));
  }
  renderNetworkRoutes(routes);
  const settings = preview.gateway_settings || preview.settings || {};
  elements.networkPreview.textContent = [
    `计划：${preview.plan_id || "无"}`,
    `方式：${preview.mode || "未知"}`,
    `功能状态：${preview.preview_only ? "预览，仍需物理网关验收" : "稳定"}`,
    `设备：${preview.device || "无"}`,
    `IPv6：${settings.ipv6_policy || "未知"} · DNS：${settings.dns_policy || "未知"}`,
    ...(preview.gateway_settings
      ? [
          `TCP：${settings.capture_tcp} · UDP：${settings.capture_udp} · 主机：${settings.proxy_host_traffic}`,
        ]
      : []),
    `有效期：${preview.expires_at || "未知"}`,
    ...warnings.map((warning) => `警告：${warning}`),
    ...conflicts.map(
      (conflict) =>
        `冲突：${conflict.kind} · ${conflict.detail}${conflict.owner ? ` · ${conflict.owner}` : ""}`,
    ),
  ].join("\n");
  setBusy(state.busy);
}

async function previewNetwork() {
  setBusy(true, "正在读取路由并生成网络预览…");
  try {
    const mode = elements.networkMode.value;
    if (mode === "gateway" && elements.networkIpv6.value === "proxy") {
      throw new Error("Linux 网关第一版不代理 IPv6；请选择直连并警告或运行期间阻断。");
    }
    const preview = await invoke("runtime_preview_network", {
      mode,
      ipv6Policy: elements.networkIpv6.value,
      dnsPolicy: elements.networkDns.value,
      captureRouteIds: mode === "tun" ? selectedCapturedRoutes() : [],
      captureTcp: elements.networkTcp.checked,
      captureUdp: elements.networkUdp.checked,
      proxyHostTraffic: elements.networkHost.checked,
      excludedRouteIds: mode === "gateway" ? selectedGatewayExcludedRoutes() : [],
      udpExceptions:
        mode === "gateway"
          ? parseGatewayExceptions(elements.networkUdpExceptions.value, "UDP 直连例外")
          : [],
      dnsDirectCidrs: mode === "gateway" ? gatewayDNSDirectCIDRs() : [],
      hostExceptions:
        mode === "gateway"
          ? parseGatewayExceptions(elements.networkHostExceptions.value, "主机直连例外")
          : [],
    });
    renderNetworkPreview(preview);
    setMessage(
      preview.conflicts?.length
        ? "发现网络冲突；解决前不能启用。"
        : preview.preview_only
          ? "网络预览已生成；该运行方式仍处于预览状态，尚未通过物理网关验收。"
          : "网络预览已生成。修改路由勾选后需再次预览。",
      Boolean(preview.conflicts?.length),
    );
  } catch (error) {
    clearNetworkPlan();
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function enableTUN() {
  if (!state.networkPlanId) return;
  if (state.networkPlanExpiresAt <= Date.now()) {
    clearNetworkPlan();
    setBusy(state.busy);
    setMessage("普通 TUN 预览已经过期，请重新生成网络预览。", true);
    return;
  }
  setBusy(true, "正在提交网络接管启用操作…");
  try {
    const command =
      state.networkPlanMode === "gateway" ? "runtime_enable_gateway" : "runtime_enable_tun";
    const operation = await invoke(command, {
      planId: state.networkPlanId,
    });
    clearNetworkPlan();
    renderOperation(operation);
    setMessage("网络接管启用操作已经持久化；等待完成后会显示真实网络状态。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function disableTUN() {
  setBusy(true, "正在停用网络接管并恢复直连…");
  try {
    const command =
      state.activeNetworkMode === "gateway" ? "runtime_disable_gateway" : "runtime_disable_tun";
    const operation = await invoke(command);
    clearNetworkPlan();
    renderOperation(operation);
    setMessage("网络接管停用操作已经持久化。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
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

async function revealSelectedSourceURL() {
  if (!state.selectedSourceId) return;
  if (!window.confirm(`${sensitiveDataWarning}\n\n确认显示所选来源的原始地址吗？`)) return;
  setBusy(true, "正在读取来源原始地址…");
  try {
    const response = await invoke("runtime_reveal_source_url", {
      sourceId: state.selectedSourceId,
      confirm: true,
    });
    elements.revealedSourceUrl.textContent = response.url;
    elements.revealedSourceUrl.classList.remove("hidden");
    setMessage("原始地址仅保存在当前界面内存中；刷新或关闭界面后清除。");
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function previewDiagnostics() {
  const options = diagnosticsOptions();
  setBusy(true, "正在预览诊断包内容…");
  try {
    const preview = await invoke("runtime_preview_diagnostics", options);
    elements.diagnosticsPreview.textContent = preview.items
      .map((item) => `${item.name}\t${item.size || 0} 字节\t${item.sensitive ? "敏感" : "已脱敏"}`)
      .join("\n");
    setMessage(sensitiveDataWarning);
  } catch (error) {
    setMessage(errorText(error), true);
  } finally {
    setBusy(false);
  }
}

async function createDiagnostics() {
  const options = diagnosticsOptions();
  setBusy(true, "正在预览诊断包内容…");
  try {
    const preview = await invoke("runtime_preview_diagnostics", options);
    elements.diagnosticsPreview.textContent = preview.items
      .map((item) => `${item.name}\t${item.size || 0} 字节\t${item.sensitive ? "敏感" : "已脱敏"}`)
      .join("\n");
    const sensitive =
      options.includeRawConfig || options.includeFullLogs || options.includeNetworkInfo;
    if (sensitive && !window.confirm(`${sensitiveDataWarning}\n\n确认保存以上所选敏感内容吗？`)) {
      setMessage("已预览诊断包内容，未生成文件。");
      return;
    }
    setBusy(true, "正在本机生成诊断包…");
    const result = await invoke("runtime_create_diagnostics", {
      ...options,
      confirmSensitive: sensitive,
    });
    elements.diagnosticsPreview.textContent =
      `${result.file_name}\n${result.size} 字节\nSHA-256 ${result.sha256}`;
    setMessage("诊断包已保存到 Runtime 的本机诊断目录；界面没有上传入口。");
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
  if (!window.confirm(`${sensitiveDataWarning}\n\n确认读取并显示当前高级覆盖正文吗？`)) {
    setMessage("已取消读取高级覆盖。");
    return;
  }
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
  elements.revealedSourceUrl.textContent = "";
  elements.revealedSourceUrl.classList.add("hidden");
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
elements.previewNetwork.addEventListener("click", previewNetwork);
elements.enableTun.addEventListener("click", enableTUN);
elements.disableTun.addEventListener("click", disableTUN);
elements.previewMihomoUpdate.addEventListener("click", previewMihomoUpdate);
elements.installMihomoUpdate.addEventListener("click", installMihomoUpdate);
elements.rollbackMihomo.addEventListener("click", rollbackMihomo);
elements.mihomoUpdateSource.addEventListener("change", () => {
  state.mihomoUpdatePlan = null;
  elements.mihomoUpdatePreview.textContent = "信任路径已变化，请重新检查。";
  setBusy(state.busy);
});
elements.mihomoUpdateVersion.addEventListener("input", () => {
  state.mihomoUpdatePlan = null;
  elements.mihomoUpdatePreview.textContent = "版本已变化，请重新检查。";
  setBusy(state.busy);
});
elements.networkMode.addEventListener("change", () => {
  if (elements.networkMode.value === "gateway" && elements.networkIpv6.value === "proxy") {
    elements.networkIpv6.value = "direct";
  }
  clearNetworkPlan();
  setBusy(state.busy);
  setMessage("网络运行方式已改变，请重新生成网络预览。");
});
elements.networkIpv6.addEventListener("change", () => {
  clearNetworkPlan();
  setBusy(state.busy);
  setMessage("IPv6 设置已改变，请重新生成网络预览。");
});
elements.networkDns.addEventListener("change", () => {
  clearNetworkPlan();
  setBusy(state.busy);
  setMessage("DNS 设置已改变，请重新生成网络预览。");
});
for (const control of [
  elements.networkTcp,
  elements.networkUdp,
  elements.networkHost,
  elements.networkDnsDirect,
  elements.networkUdpExceptions,
  elements.networkHostExceptions,
]) {
  control.addEventListener("input", () => {
    clearNetworkPlan();
    setBusy(state.busy);
    setMessage("网关设置已改变，请重新生成网络预览。");
  });
}
elements.networkRouteList.addEventListener("change", (event) => {
  if (!(event.target instanceof HTMLInputElement) || event.target.type !== "checkbox") return;
  clearNetworkPlan();
  setBusy(state.busy);
  setMessage("更具体路由的选择已改变，请重新生成网络预览。");
});
elements.getOperation.addEventListener("click", getOperation);
elements.wait.addEventListener("click", waitOperation);
elements.cancelOperation.addEventListener("click", cancelOperation);
elements.addRemoteSource.addEventListener("click", addRemoteSource);
elements.addImportedSource.addEventListener("click", addImportedSource);
elements.selectedSource.addEventListener("change", () => {
  state.selectedSourceId = elements.selectedSource.value;
  elements.revealedSourceUrl.textContent = "";
  elements.revealedSourceUrl.classList.add("hidden");
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
elements.revealSourceUrl.addEventListener("click", revealSelectedSourceURL);
elements.addManagedResource.addEventListener("click", addManagedResource);
elements.loadAdvancedOverride.addEventListener("click", loadAdvancedOverride);
elements.previewAdvancedOverride.addEventListener("click", previewAdvancedOverride);
elements.saveAdvancedOverride.addEventListener("click", saveAdvancedOverride);
elements.previewDiagnostics.addEventListener("click", previewDiagnostics);
elements.createDiagnostics.addEventListener("click", createDiagnostics);

handshake();
