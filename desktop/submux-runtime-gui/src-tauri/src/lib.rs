mod runtime_ipc;

use runtime_ipc::{BridgeError, Handshake, RuntimeBridge};
use serde_json::Value;
use tauri::State;

#[tauri::command]
fn runtime_handshake(bridge: State<'_, RuntimeBridge>) -> Result<Handshake, BridgeError> {
    bridge.handshake()
}

#[tauri::command]
fn runtime_observe(bridge: State<'_, RuntimeBridge>) -> Result<Value, BridgeError> {
    bridge.observe()
}

#[tauri::command]
fn runtime_import_config(
    bridge: State<'_, RuntimeBridge>,
    content: String,
) -> Result<Value, BridgeError> {
    bridge.import_config(content.as_bytes())
}

#[tauri::command]
fn runtime_preview_candidate(
    bridge: State<'_, RuntimeBridge>,
    content_id: String,
) -> Result<Value, BridgeError> {
    bridge.preview_candidate(&content_id)
}

#[tauri::command]
fn runtime_preview_source(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
) -> Result<Value, BridgeError> {
    bridge.preview_source(&source_id)
}

#[tauri::command]
fn runtime_preview_override(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
    content: String,
) -> Result<Value, BridgeError> {
    bridge.preview_override(&source_id, content.as_bytes())
}

#[tauri::command]
fn runtime_apply_candidate(
    bridge: State<'_, RuntimeBridge>,
    content_id: String,
) -> Result<Value, BridgeError> {
    bridge.apply_candidate(&content_id)
}

#[tauri::command]
fn runtime_add_remote_source(
    bridge: State<'_, RuntimeBridge>,
    draft_json: String,
) -> Result<Value, BridgeError> {
    bridge.add_remote_source(draft_json.as_bytes())
}

#[tauri::command]
fn runtime_add_imported_source(
    bridge: State<'_, RuntimeBridge>,
    name: String,
    content: String,
) -> Result<Value, BridgeError> {
    bridge.add_imported_source(&name, content.as_bytes())
}

#[tauri::command]
fn runtime_get_advanced_override(bridge: State<'_, RuntimeBridge>) -> Result<Value, BridgeError> {
    bridge.get_advanced_override()
}

#[tauri::command]
fn runtime_set_advanced_override(
    bridge: State<'_, RuntimeBridge>,
    content: String,
) -> Result<Value, BridgeError> {
    bridge.set_advanced_override(content.as_bytes())
}

#[tauri::command]
fn runtime_add_managed_resource(
    bridge: State<'_, RuntimeBridge>,
    name: String,
    kind: String,
    content: String,
) -> Result<Value, BridgeError> {
    bridge.add_managed_resource(&name, &kind, content.as_bytes())
}

#[tauri::command]
fn runtime_refresh_source(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
    route: String,
) -> Result<Value, BridgeError> {
    bridge.refresh_source(&source_id, &route)
}

#[tauri::command]
fn runtime_switch_source(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
    route: String,
    use_cached: bool,
) -> Result<Value, BridgeError> {
    bridge.switch_source(&source_id, &route, use_cached)
}

#[tauri::command]
fn runtime_delete_source(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
    confirm_current: bool,
) -> Result<Value, BridgeError> {
    bridge.delete_source(&source_id, confirm_current)
}

#[tauri::command]
fn runtime_apply_source(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
) -> Result<Value, BridgeError> {
    bridge.apply_source(&source_id)
}

#[tauri::command]
fn runtime_start_proxy(bridge: State<'_, RuntimeBridge>) -> Result<Value, BridgeError> {
    bridge.execute_action("proxy.start", None)
}

#[tauri::command]
fn runtime_stop_proxy(bridge: State<'_, RuntimeBridge>) -> Result<Value, BridgeError> {
    bridge.execute_action("proxy.stop", None)
}

#[tauri::command]
fn runtime_preview_network(
    bridge: State<'_, RuntimeBridge>,
    mode: String,
    ipv6_policy: String,
    dns_policy: String,
    capture_route_ids: Vec<String>,
    capture_tcp: bool,
    capture_udp: bool,
    proxy_host_traffic: bool,
    excluded_route_ids: Vec<String>,
    udp_exceptions: Vec<Value>,
    dns_direct_cidrs: Vec<String>,
    host_exceptions: Vec<Value>,
) -> Result<Value, BridgeError> {
    bridge.preview_network(
        &mode,
        &ipv6_policy,
        &dns_policy,
        &capture_route_ids,
        capture_tcp,
        capture_udp,
        proxy_host_traffic,
        &excluded_route_ids,
        &udp_exceptions,
        &dns_direct_cidrs,
        &host_exceptions,
    )
}

#[tauri::command]
fn runtime_enable_tun(
    bridge: State<'_, RuntimeBridge>,
    plan_id: String,
) -> Result<Value, BridgeError> {
    bridge.enable_tun(&plan_id)
}

#[tauri::command]
fn runtime_disable_tun(bridge: State<'_, RuntimeBridge>) -> Result<Value, BridgeError> {
    bridge.disable_tun()
}

#[tauri::command]
fn runtime_enable_gateway(
    bridge: State<'_, RuntimeBridge>,
    plan_id: String,
) -> Result<Value, BridgeError> {
    bridge.enable_gateway(&plan_id)
}

#[tauri::command]
fn runtime_disable_gateway(bridge: State<'_, RuntimeBridge>) -> Result<Value, BridgeError> {
    bridge.disable_gateway()
}

#[tauri::command]
fn runtime_preview_mihomo_update(
    bridge: State<'_, RuntimeBridge>,
    source: String,
    version: String,
) -> Result<Value, BridgeError> {
    bridge.preview_mihomo_update(&source, &version)
}

#[tauri::command]
fn runtime_install_mihomo_update(
    bridge: State<'_, RuntimeBridge>,
    plan_id: String,
    trust: String,
    confirm: bool,
) -> Result<Value, BridgeError> {
    bridge.install_mihomo_update(&plan_id, &trust, confirm)
}

#[tauri::command]
fn runtime_rollback_mihomo(
    bridge: State<'_, RuntimeBridge>,
    confirm: bool,
) -> Result<Value, BridgeError> {
    bridge.rollback_mihomo(confirm)
}

#[tauri::command]
fn runtime_preview_product_update(
    bridge: State<'_, RuntimeBridge>,
    version: String,
    bundle_path: String,
) -> Result<Value, BridgeError> {
    bridge.preview_product_update(&version, &bundle_path)
}

#[tauri::command]
fn runtime_install_product_update(
    bridge: State<'_, RuntimeBridge>,
    plan_id: String,
    confirm: bool,
) -> Result<Value, BridgeError> {
    bridge.install_product_update(&plan_id, confirm)
}

#[tauri::command]
fn runtime_rollback_product(
    bridge: State<'_, RuntimeBridge>,
    confirm: bool,
) -> Result<Value, BridgeError> {
    bridge.rollback_product(confirm)
}

#[tauri::command]
fn runtime_get_operation(
    bridge: State<'_, RuntimeBridge>,
    operation_id: String,
) -> Result<Value, BridgeError> {
    bridge.get_operation(&operation_id)
}

#[tauri::command]
fn runtime_wait_operation(
    bridge: State<'_, RuntimeBridge>,
    operation_id: String,
    timeout_ms: u64,
) -> Result<Value, BridgeError> {
    bridge.wait_operation(&operation_id, timeout_ms)
}

#[tauri::command]
fn runtime_cancel_operation(
    bridge: State<'_, RuntimeBridge>,
    operation_id: String,
) -> Result<Value, BridgeError> {
    bridge.cancel_operation(&operation_id)
}

#[tauri::command]
fn runtime_verify_proxy(bridge: State<'_, RuntimeBridge>) -> Result<Value, BridgeError> {
    bridge.verify_proxy()
}

#[tauri::command]
fn runtime_reveal_source_url(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
    confirm: bool,
) -> Result<Value, BridgeError> {
    bridge.reveal_source_url(&source_id, confirm)
}

#[tauri::command]
fn runtime_preview_diagnostics(
    bridge: State<'_, RuntimeBridge>,
    include_raw_config: bool,
    include_full_logs: bool,
    include_network_info: bool,
) -> Result<Value, BridgeError> {
    bridge.preview_diagnostics(
        include_raw_config,
        include_full_logs,
        include_network_info,
    )
}

#[tauri::command]
fn runtime_create_diagnostics(
    bridge: State<'_, RuntimeBridge>,
    include_raw_config: bool,
    include_full_logs: bool,
    include_network_info: bool,
    confirm_sensitive: bool,
) -> Result<Value, BridgeError> {
    bridge.create_diagnostics(
        include_raw_config,
        include_full_logs,
        include_network_info,
        confirm_sensitive,
    )
}

#[tauri::command]
fn runtime_preview_backup(
    bridge: State<'_, RuntimeBridge>,
    include_secrets: bool,
) -> Result<Value, BridgeError> {
    bridge.preview_backup(include_secrets)
}

#[tauri::command]
fn runtime_export_backup(
    bridge: State<'_, RuntimeBridge>,
    path: String,
    include_secrets: bool,
    confirm_plaintext: bool,
) -> Result<Value, BridgeError> {
    bridge.export_backup(&path, include_secrets, confirm_plaintext)
}

#[tauri::command]
fn runtime_preview_backup_restore(
    bridge: State<'_, RuntimeBridge>,
    path: String,
) -> Result<Value, BridgeError> {
    bridge.preview_backup_restore(&path)
}

#[tauri::command]
fn runtime_restore_backup(
    bridge: State<'_, RuntimeBridge>,
    content_id: String,
    confirm: bool,
) -> Result<Value, BridgeError> {
    bridge.restore_backup(&content_id, confirm)
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .manage(RuntimeBridge::default())
        .invoke_handler(tauri::generate_handler![
            runtime_handshake,
            runtime_observe,
            runtime_import_config,
            runtime_preview_candidate,
            runtime_preview_source,
            runtime_preview_override,
            runtime_apply_candidate,
            runtime_add_remote_source,
            runtime_add_imported_source,
            runtime_get_advanced_override,
            runtime_set_advanced_override,
            runtime_add_managed_resource,
            runtime_refresh_source,
            runtime_switch_source,
            runtime_delete_source,
            runtime_apply_source,
            runtime_start_proxy,
            runtime_stop_proxy,
            runtime_preview_network,
            runtime_enable_tun,
            runtime_disable_tun,
            runtime_enable_gateway,
            runtime_disable_gateway,
            runtime_preview_mihomo_update,
            runtime_install_mihomo_update,
            runtime_rollback_mihomo,
            runtime_preview_product_update,
            runtime_install_product_update,
            runtime_rollback_product,
            runtime_get_operation,
            runtime_wait_operation,
            runtime_cancel_operation,
            runtime_verify_proxy,
            runtime_reveal_source_url,
            runtime_preview_diagnostics,
            runtime_create_diagnostics,
            runtime_preview_backup,
            runtime_export_backup,
            runtime_preview_backup_restore,
            runtime_restore_backup
        ])
        .run(tauri::generate_context!())
        .expect("could not run Submux Runtime GUI");
}
