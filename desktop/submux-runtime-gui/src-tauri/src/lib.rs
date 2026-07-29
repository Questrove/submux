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
fn runtime_refresh_source(
    bridge: State<'_, RuntimeBridge>,
    source_id: String,
    route: String,
) -> Result<Value, BridgeError> {
    bridge.refresh_source(&source_id, &route)
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

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .manage(RuntimeBridge::default())
        .invoke_handler(tauri::generate_handler![
            runtime_handshake,
            runtime_observe,
            runtime_import_config,
            runtime_preview_candidate,
            runtime_apply_candidate,
            runtime_add_remote_source,
            runtime_refresh_source,
            runtime_apply_source,
            runtime_start_proxy,
            runtime_stop_proxy,
            runtime_get_operation,
            runtime_wait_operation,
            runtime_cancel_operation,
            runtime_verify_proxy
        ])
        .run(tauri::generate_context!())
        .expect("could not run Submux Runtime GUI");
}
