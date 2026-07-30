use serde::Serialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::fmt::{Display, Formatter};
use std::io::{Read, Write};
use std::sync::atomic::{AtomicU64, Ordering};
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const PROTOCOL_VERSION: u64 = 1;
const MAX_RESPONSE_BYTES: usize = 12 << 20;
const MAX_IMPORT_BYTES: usize = 8 << 20;
const MAX_SOURCE_DRAFT_BYTES: usize = 512 << 10;
const MAX_MANAGED_RESOURCE_BYTES: usize = 4 << 20;
const MAX_ADVANCED_OVERRIDE_BYTES: usize = 1 << 20;
const DEFAULT_WAIT_TIMEOUT_MS: u64 = 300_000;

#[cfg(target_os = "windows")]
const DEFAULT_ENDPOINT: &str = r"\\.\pipe\submux-runtime";
#[cfg(target_os = "linux")]
const DEFAULT_ENDPOINT: &str = "/run/submux-runtime/runtime.sock";
#[cfg(target_os = "macos")]
const DEFAULT_ENDPOINT: &str = "/var/run/submux-runtime/runtime.sock";

static REQUEST_SERIAL: AtomicU64 = AtomicU64::new(1);

trait ReadWrite: Read + Write {}
impl<T: Read + Write> ReadWrite for T {}

#[derive(Clone)]
pub struct RuntimeBridge {
    endpoint: String,
    client_version: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct BridgeError {
    pub code: String,
    pub message: String,
    pub retryable: bool,
    pub restart_required: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct Handshake {
    pub client_version: String,
    pub compatible: bool,
    pub snapshot: Value,
}

struct IpcResponse {
    status: u16,
    body: Vec<u8>,
}

impl Default for RuntimeBridge {
    fn default() -> Self {
        Self {
            endpoint: DEFAULT_ENDPOINT.to_string(),
            client_version: env!("SUBMUX_GUI_VERSION").to_string(),
        }
    }
}

impl Display for BridgeError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for BridgeError {}

impl RuntimeBridge {
    pub fn observe(&self) -> Result<Value, BridgeError> {
        self.call_json("GET", "/v1/snapshot", None, &[], &[], None)
    }

    pub fn handshake(&self) -> Result<Handshake, BridgeError> {
        let snapshot = self.observe()?;
        let protocol = snapshot
            .get("protocol_version")
            .and_then(Value::as_u64)
            .unwrap_or_default();
        if protocol != PROTOCOL_VERSION {
            return Err(BridgeError::restart(
                "protocol_unsupported",
                "Runtime protocol version is not supported",
            ));
        }
        let runtime_version = snapshot
            .pointer("/runtime/version")
            .and_then(Value::as_str)
            .unwrap_or_default();
        Ok(Handshake {
            client_version: self.client_version.clone(),
            compatible: runtime_version == self.client_version,
            snapshot,
        })
    }

    pub fn import_config(&self, content: &[u8]) -> Result<Value, BridgeError> {
        self.ensure_compatible()?;
        self.upload_content(
            "application/x-yaml",
            content,
            MAX_IMPORT_BYTES,
            "Imported configuration",
        )
    }

    fn upload_content(
        &self,
        content_type: &str,
        content: &[u8],
        maximum: usize,
        description: &str,
    ) -> Result<Value, BridgeError> {
        if content.is_empty() || content.len() > maximum {
            return Err(BridgeError::request(&format!(
                "{description} is empty or exceeds the Runtime limit"
            )));
        }
        let digest = hex::encode(Sha256::digest(content));
        let size = content.len().to_string();
        self.call_json(
            "POST",
            "/v1/imports",
            Some(content_type),
            &[
                ("X-Submux-Content-Size", size.as_str()),
                ("X-Submux-Content-SHA256", digest.as_str()),
            ],
            content,
            None,
        )
    }

    pub fn get_advanced_override(&self) -> Result<Value, BridgeError> {
        self.call_json(
            "GET",
            "/v1/advanced-override?reveal=1",
            None,
            &[],
            &[],
            None,
        )
    }

    pub fn set_advanced_override(&self, content: &[u8]) -> Result<Value, BridgeError> {
        self.ensure_compatible()?;
        let imported = self.upload_content(
            "application/x-yaml",
            content,
            MAX_ADVANCED_OVERRIDE_BYTES,
            "Advanced override",
        )?;
        let content_id = imported
            .get("content_id")
            .and_then(Value::as_str)
            .ok_or_else(|| BridgeError::service("Runtime override upload response is invalid"))?;
        self.execute_action_with_params("override.set", json!({ "content_id": content_id }))
    }

    pub fn add_managed_resource(
        &self,
        name: &str,
        kind: &str,
        content: &[u8],
    ) -> Result<Value, BridgeError> {
        self.ensure_compatible()?;
        validate_resource_name(name)?;
        validate_resource_kind(kind)?;
        let imported = self.upload_content(
            "application/vnd.submux.managed-resource",
            content,
            MAX_MANAGED_RESOURCE_BYTES,
            "Managed resource",
        )?;
        let content_id = imported
            .get("content_id")
            .and_then(Value::as_str)
            .ok_or_else(|| BridgeError::service("Runtime resource upload response is invalid"))?;
        self.execute_action_with_params(
            "resource.add",
            json!({
                "content_id": content_id,
                "resource_name": name,
                "resource_kind": kind,
            }),
        )
    }

    pub fn add_remote_source(&self, draft_json: &[u8]) -> Result<Value, BridgeError> {
        self.ensure_compatible()?;
        if draft_json.is_empty() || draft_json.len() > MAX_SOURCE_DRAFT_BYTES {
            return Err(BridgeError::request(
                "Remote source draft is empty or exceeds the Runtime limit",
            ));
        }
        let digest = hex::encode(Sha256::digest(draft_json));
        let size = draft_json.len().to_string();
        let imported = self.call_json(
            "POST",
            "/v1/imports",
            Some("application/vnd.submux.runtime-source+json"),
            &[
                ("X-Submux-Content-Size", size.as_str()),
                ("X-Submux-Content-SHA256", digest.as_str()),
            ],
            draft_json,
            None,
        )?;
        let content_id = imported
            .get("content_id")
            .and_then(Value::as_str)
            .ok_or_else(|| BridgeError::service("Runtime source upload response is invalid"))?;
        self.execute_action_with_params("source.add_remote", json!({ "content_id": content_id }))
    }

    pub fn add_imported_source(&self, name: &str, content: &[u8]) -> Result<Value, BridgeError> {
        self.ensure_compatible()?;
        validate_source_name(name)?;
        let imported = self.upload_content(
            "application/x-yaml",
            content,
            MAX_IMPORT_BYTES,
            "Imported source",
        )?;
        let content_id = imported
            .get("content_id")
            .and_then(Value::as_str)
            .ok_or_else(|| BridgeError::service("Runtime source upload response is invalid"))?;
        self.execute_action_with_params(
            "source.add_imported",
            json!({ "content_id": content_id, "source_name": name }),
        )
    }

    pub fn refresh_source(&self, source_id: &str, route: &str) -> Result<Value, BridgeError> {
        validate_source_id(source_id)?;
        validate_source_route(route)?;
        self.execute_action_with_params(
            "source.refresh",
            json!({ "source_id": source_id, "route": route }),
        )
    }

    pub fn switch_source(
        &self,
        source_id: &str,
        route: &str,
        use_cached: bool,
    ) -> Result<Value, BridgeError> {
        validate_source_id(source_id)?;
        validate_source_route(route)?;
        self.execute_action_with_params(
            "source.switch",
            json!({
                "source_id": source_id,
                "route": route,
                "use_cached": use_cached,
            }),
        )
    }

    pub fn delete_source(
        &self,
        source_id: &str,
        confirm_current: bool,
    ) -> Result<Value, BridgeError> {
        validate_source_id(source_id)?;
        self.execute_action_with_params(
            "source.delete",
            json!({ "source_id": source_id, "confirm": confirm_current }),
        )
    }

    pub fn apply_source(&self, source_id: &str) -> Result<Value, BridgeError> {
        validate_source_id(source_id)?;
        self.execute_action_with_params("source.apply", json!({ "source_id": source_id }))
    }

    pub fn preview_candidate(&self, content_id: &str) -> Result<Value, BridgeError> {
        self.preview_candidate_request(json!({ "content_id": content_id }))
    }

    pub fn preview_source(&self, source_id: &str) -> Result<Value, BridgeError> {
        validate_source_id(source_id)?;
        self.preview_candidate_request(json!({ "source_id": source_id }))
    }

    pub fn preview_override(&self, source_id: &str, content: &[u8]) -> Result<Value, BridgeError> {
        self.ensure_compatible()?;
        validate_source_id(source_id)?;
        let imported = self.upload_content(
            "application/x-yaml",
            content,
            MAX_ADVANCED_OVERRIDE_BYTES,
            "Advanced override",
        )?;
        let content_id = imported
            .get("content_id")
            .and_then(Value::as_str)
            .ok_or_else(|| BridgeError::service("Runtime override upload response is invalid"))?;
        self.preview_candidate_request(json!({
            "source_id": source_id,
            "override_content_id": content_id,
        }))
    }

    fn preview_candidate_request(&self, request: Value) -> Result<Value, BridgeError> {
        let body = serde_json::to_vec(&request).map_err(BridgeError::internal)?;
        self.call_json(
            "POST",
            "/v1/candidates/preview",
            Some("application/json"),
            &[],
            &body,
            None,
        )
    }

    pub fn preview_network(
        &self,
        mode: &str,
        ipv6_policy: &str,
        dns_policy: &str,
        capture_route_ids: &[String],
        capture_tcp: bool,
        capture_udp: bool,
        proxy_host_traffic: bool,
        excluded_route_ids: &[String],
        udp_exceptions: &[Value],
        dns_direct_cidrs: &[String],
        host_exceptions: &[Value],
    ) -> Result<Value, BridgeError> {
        if !matches!(mode, "tun" | "gateway") {
            return Err(BridgeError::request("Runtime network mode is invalid"));
        }
        if !matches!(ipv6_policy, "proxy" | "direct" | "block")
            || (mode == "gateway" && ipv6_policy == "proxy")
        {
            return Err(BridgeError::request("Runtime network IPv6 policy is invalid"));
        }
        if !matches!(dns_policy, "hijack" | "off") {
            return Err(BridgeError::request("Runtime TUN DNS policy is invalid"));
        }
        if capture_route_ids.len() > 512
            || capture_route_ids.iter().any(|value| {
                value.len() < 3
                    || value.len() > 96
                    || !value
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || b"-_.:".contains(&byte))
            })
        {
            return Err(BridgeError::request(
                "Runtime captured route selection is invalid",
            ));
        }
        validate_identifier_list(excluded_route_ids, "Runtime gateway route exclusion")?;
        if dns_direct_cidrs.len() > 128
            || dns_direct_cidrs
                .iter()
                .any(|value| value.is_empty() || value.len() > 64)
        {
            return Err(BridgeError::request(
                "Runtime gateway DNS direct list is invalid",
            ));
        }
        validate_gateway_exceptions(udp_exceptions, false)?;
        validate_gateway_exceptions(host_exceptions, true)?;
        if mode == "tun"
            && (!excluded_route_ids.is_empty()
                || !udp_exceptions.is_empty()
                || !dns_direct_cidrs.is_empty()
                || !host_exceptions.is_empty()
                || !capture_tcp
                || !capture_udp
                || proxy_host_traffic)
        {
            return Err(BridgeError::request(
                "ordinary TUN does not accept gateway settings",
            ));
        }
        let body = serde_json::to_vec(&json!({
            "mode": mode,
            "ipv6_policy": ipv6_policy,
            "dns_policy": dns_policy,
            "capture_route_ids": capture_route_ids,
            "capture_tcp": capture_tcp,
            "capture_udp": capture_udp,
            "proxy_host_traffic": proxy_host_traffic,
            "excluded_route_ids": excluded_route_ids,
            "udp_exceptions": udp_exceptions,
            "dns_direct_cidrs": dns_direct_cidrs,
            "host_exceptions": host_exceptions,
        }))
        .map_err(BridgeError::internal)?;
        self.call_json(
            "POST",
            "/v1/network/preview",
            Some("application/json"),
            &[],
            &body,
            None,
        )
    }

    pub fn enable_tun(&self, plan_id: &str) -> Result<Value, BridgeError> {
        validate_plan_id(plan_id)?;
        self.execute_action_with_params("network.enable_tun", json!({ "plan_id": plan_id }))
    }

    pub fn disable_tun(&self) -> Result<Value, BridgeError> {
        self.execute_action_with_params("network.disable_tun", json!({}))
    }

    pub fn enable_gateway(&self, plan_id: &str) -> Result<Value, BridgeError> {
        validate_plan_id(plan_id)?;
        self.execute_action_with_params(
            "network.enable_gateway",
            json!({ "plan_id": plan_id }),
        )
    }

    pub fn disable_gateway(&self) -> Result<Value, BridgeError> {
        self.execute_action_with_params("network.disable_gateway", json!({}))
    }

    pub fn apply_candidate(&self, content_id: &str) -> Result<Value, BridgeError> {
        self.execute_action("proxy.apply_import", Some(content_id))
    }

    pub fn execute_action(
        &self,
        kind: &str,
        content_id: Option<&str>,
    ) -> Result<Value, BridgeError> {
        let params = match content_id {
            Some(value) => json!({ "content_id": value }),
            None => json!({}),
        };
        self.execute_action_with_params(kind, params)
    }

    fn execute_action_with_params(&self, kind: &str, params: Value) -> Result<Value, BridgeError> {
        let snapshot = self.ensure_compatible()?;
        let revision = snapshot
            .get("revision")
            .and_then(Value::as_u64)
            .ok_or_else(|| BridgeError::service("Runtime snapshot revision is missing"))?;
        let request_id = new_request_id();
        let body = serde_json::to_vec(&json!({
            "request_id": request_id,
            "if_revision": revision,
            "action": {
                "kind": kind,
                "params": params
            }
        }))
        .map_err(BridgeError::internal)?;
        let response = self.call_json(
            "POST",
            "/v1/operations",
            Some("application/json"),
            &[],
            &body,
            Some(&request_id),
        )?;
        response
            .get("operation")
            .cloned()
            .ok_or_else(|| BridgeError::service("Runtime operation response is invalid"))
    }

    pub fn wait_operation(
        &self,
        operation_id: &str,
        timeout_ms: u64,
    ) -> Result<Value, BridgeError> {
        validate_operation_id(operation_id)?;
        let timeout_ms = if timeout_ms == 0 {
            DEFAULT_WAIT_TIMEOUT_MS
        } else {
            timeout_ms.min(DEFAULT_WAIT_TIMEOUT_MS)
        };
        let deadline = std::time::Instant::now() + Duration::from_millis(timeout_ms);
        loop {
            let response = self.call_json(
                "GET",
                &format!("/v1/operations/{operation_id}"),
                None,
                &[],
                &[],
                None,
            )?;
            let operation = response
                .get("operation")
                .cloned()
                .ok_or_else(|| BridgeError::service("Runtime operation response is invalid"))?;
            let state = operation
                .get("state")
                .and_then(Value::as_str)
                .unwrap_or_default();
            if matches!(
                state,
                "succeeded" | "failed" | "cancelled" | "outcome_unknown"
            ) {
                return Ok(operation);
            }
            if std::time::Instant::now() >= deadline {
                return Err(BridgeError {
                    code: "service_unavailable".to_string(),
                    message: "Timed out waiting for the Runtime operation".to_string(),
                    retryable: true,
                    restart_required: false,
                });
            }
            thread::sleep(Duration::from_millis(250));
        }
    }

    pub fn get_operation(&self, operation_id: &str) -> Result<Value, BridgeError> {
        validate_operation_id(operation_id)?;
        let response = self.call_json(
            "GET",
            &format!("/v1/operations/{operation_id}"),
            None,
            &[],
            &[],
            None,
        )?;
        response
            .get("operation")
            .cloned()
            .ok_or_else(|| BridgeError::service("Runtime operation response is invalid"))
    }

    pub fn cancel_operation(&self, operation_id: &str) -> Result<Value, BridgeError> {
        validate_operation_id(operation_id)?;
        let snapshot = self.ensure_compatible()?;
        let revision = snapshot
            .get("revision")
            .and_then(Value::as_u64)
            .ok_or_else(|| BridgeError::service("Runtime snapshot revision is missing"))?;
        let request_id = new_request_id();
        let body = serde_json::to_vec(&json!({
            "request_id": request_id,
            "if_revision": revision
        }))
        .map_err(BridgeError::internal)?;
        let response = self.call_json(
            "POST",
            &format!("/v1/operations/{operation_id}/cancel"),
            Some("application/json"),
            &[],
            &body,
            Some(&request_id),
        )?;
        response
            .get("operation")
            .cloned()
            .ok_or_else(|| BridgeError::service("Runtime cancellation response is invalid"))
    }

    pub fn verify_proxy(&self) -> Result<Value, BridgeError> {
        self.call_json("GET", "/v1/proxy/verify", None, &[], &[], None)
    }

    pub fn reveal_source_url(
        &self,
        source_id: &str,
        confirm: bool,
    ) -> Result<Value, BridgeError> {
        validate_source_id(source_id)?;
        if !confirm {
            return Err(BridgeError::request(
                "Revealing a source URL requires explicit confirmation",
            ));
        }
        let body = serde_json::to_vec(&json!({
            "source_id": source_id,
            "confirm": true,
        }))
        .map_err(BridgeError::internal)?;
        self.call_json(
            "POST",
            "/v1/sources/reveal-url",
            Some("application/json"),
            &[],
            &body,
            None,
        )
    }

    pub fn preview_diagnostics(
        &self,
        include_raw_config: bool,
        include_full_logs: bool,
        include_network_info: bool,
    ) -> Result<Value, BridgeError> {
        let body = serde_json::to_vec(&json!({
            "include_raw_config": include_raw_config,
            "include_full_logs": include_full_logs,
            "include_network_info": include_network_info,
        }))
        .map_err(BridgeError::internal)?;
        self.call_json(
            "POST",
            "/v1/diagnostics/preview",
            Some("application/json"),
            &[],
            &body,
            None,
        )
    }

    pub fn create_diagnostics(
        &self,
        include_raw_config: bool,
        include_full_logs: bool,
        include_network_info: bool,
        confirm_sensitive: bool,
    ) -> Result<Value, BridgeError> {
        self.ensure_compatible()?;
        let body = serde_json::to_vec(&json!({
            "include_raw_config": include_raw_config,
            "include_full_logs": include_full_logs,
            "include_network_info": include_network_info,
            "confirm_sensitive": confirm_sensitive,
        }))
        .map_err(BridgeError::internal)?;
        self.call_json(
            "POST",
            "/v1/diagnostics/create",
            Some("application/json"),
            &[],
            &body,
            None,
        )
    }

    fn ensure_compatible(&self) -> Result<Value, BridgeError> {
        let handshake = self.handshake()?;
        if !handshake.compatible {
            return Err(BridgeError::restart(
                "protocol_unsupported",
                "GUI and Runtime product versions do not match",
            ));
        }
        Ok(handshake.snapshot)
    }

    fn call_json(
        &self,
        method: &str,
        path: &str,
        content_type: Option<&str>,
        headers: &[(&str, &str)],
        body: &[u8],
        fixed_request_id: Option<&str>,
    ) -> Result<Value, BridgeError> {
        let request_id = fixed_request_id
            .map(ToOwned::to_owned)
            .unwrap_or_else(new_request_id);
        let response = self.request(method, path, content_type, headers, body, &request_id)?;
        let value: Value = serde_json::from_slice(&response.body).map_err(|error| BridgeError {
            code: "service_unavailable".to_string(),
            message: format!("Runtime returned invalid JSON: {error}"),
            retryable: true,
            restart_required: false,
        })?;
        if !(200..300).contains(&response.status) {
            let code = value
                .pointer("/error/code")
                .and_then(Value::as_str)
                .unwrap_or("service_unavailable");
            let message = value
                .pointer("/error/message")
                .and_then(Value::as_str)
                .unwrap_or("Runtime request failed");
            let retryable = value
                .pointer("/error/retryable")
                .and_then(Value::as_bool)
                .unwrap_or(response.status >= 500);
            return Err(BridgeError {
                code: code.to_string(),
                message: message.to_string(),
                retryable,
                restart_required: code == "protocol_unsupported",
            });
        }
        Ok(value)
    }

    fn request(
        &self,
        method: &str,
        path: &str,
        content_type: Option<&str>,
        headers: &[(&str, &str)],
        body: &[u8],
        request_id: &str,
    ) -> Result<IpcResponse, BridgeError> {
        let mut stream = connect(&self.endpoint).map_err(BridgeError::transport)?;
        let mut request = format!(
            "{method} {path} HTTP/1.1\r\n\
             Host: runtime\r\n\
             X-Submux-Request-ID: {request_id}\r\n\
             X-Submux-Protocol-Version: {PROTOCOL_VERSION}\r\n\
             X-Submux-Client-Type: gui\r\n\
             X-Submux-Client-Version: {}\r\n\
             Content-Length: {}\r\n\
             Connection: close\r\n",
            self.client_version,
            body.len()
        );
        if let Some(value) = content_type {
            request.push_str(&format!("Content-Type: {value}\r\n"));
        }
        for (name, value) in headers {
            request.push_str(&format!("{name}: {value}\r\n"));
        }
        request.push_str("\r\n");
        stream
            .write_all(request.as_bytes())
            .and_then(|_| stream.write_all(body))
            .and_then(|_| stream.flush())
            .map_err(BridgeError::transport)?;

        let raw = read_limited_response(&mut stream)?;
        parse_http_response(&raw)
    }
}

impl BridgeError {
    fn request(message: &str) -> Self {
        Self {
            code: "invalid_request".to_string(),
            message: message.to_string(),
            retryable: false,
            restart_required: false,
        }
    }

    fn service(message: &str) -> Self {
        Self {
            code: "service_unavailable".to_string(),
            message: message.to_string(),
            retryable: true,
            restart_required: false,
        }
    }

    fn restart(code: &str, message: &str) -> Self {
        Self {
            code: code.to_string(),
            message: message.to_string(),
            retryable: false,
            restart_required: true,
        }
    }

    fn internal(error: impl Display) -> Self {
        Self {
            code: "internal".to_string(),
            message: format!("Could not encode Runtime request: {error}"),
            retryable: false,
            restart_required: false,
        }
    }

    fn transport(error: std::io::Error) -> Self {
        let permission_denied = error.kind() == std::io::ErrorKind::PermissionDenied;
        Self {
            code: if permission_denied {
                "permission_denied"
            } else {
                "service_unavailable"
            }
            .to_string(),
            message: if permission_denied {
                "Permission to access Submux Runtime was denied"
            } else {
                "Submux Runtime is unavailable"
            }
            .to_string(),
            retryable: !permission_denied,
            restart_required: false,
        }
    }
}

#[cfg(unix)]
fn connect(endpoint: &str) -> std::io::Result<Box<dyn ReadWrite>> {
    use std::os::unix::net::UnixStream;
    let stream = UnixStream::connect(endpoint)?;
    stream.set_read_timeout(Some(Duration::from_secs(15)))?;
    stream.set_write_timeout(Some(Duration::from_secs(15)))?;
    Ok(Box::new(stream))
}

#[cfg(target_os = "windows")]
fn connect(endpoint: &str) -> std::io::Result<Box<dyn ReadWrite>> {
    use std::fs::OpenOptions;
    let deadline = std::time::Instant::now() + Duration::from_secs(15);
    loop {
        match OpenOptions::new().read(true).write(true).open(endpoint) {
            Ok(pipe) => return Ok(Box::new(pipe)),
            Err(error)
                if error.raw_os_error() == Some(231) && std::time::Instant::now() < deadline =>
            {
                thread::sleep(Duration::from_millis(50));
            }
            Err(error) => return Err(error),
        }
    }
}

fn read_limited_response(stream: &mut Box<dyn ReadWrite>) -> Result<Vec<u8>, BridgeError> {
    let limit = MAX_RESPONSE_BYTES + 64 * 1024;
    let mut raw = Vec::new();
    let mut buffer = [0_u8; 16 * 1024];
    loop {
        match stream.read(&mut buffer) {
            Ok(0) => break,
            Ok(count) => {
                if raw.len() + count > limit {
                    return Err(BridgeError::service(
                        "Runtime response exceeds the hard size limit",
                    ));
                }
                raw.extend_from_slice(&buffer[..count]);
            }
            Err(error) if error.kind() == std::io::ErrorKind::BrokenPipe && !raw.is_empty() => {
                break;
            }
            Err(error) => return Err(BridgeError::transport(error)),
        }
    }
    Ok(raw)
}

fn parse_http_response(raw: &[u8]) -> Result<IpcResponse, BridgeError> {
    let header_end = raw
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .ok_or_else(|| BridgeError::service("Runtime returned an invalid HTTP response"))?;
    let headers = std::str::from_utf8(&raw[..header_end])
        .map_err(|_| BridgeError::service("Runtime returned invalid HTTP headers"))?;
    let mut lines = headers.split("\r\n");
    let status = lines
        .next()
        .and_then(|line| line.split_whitespace().nth(1))
        .and_then(|value| value.parse::<u16>().ok())
        .ok_or_else(|| BridgeError::service("Runtime returned an invalid HTTP status"))?;
    let mut chunked = false;
    let mut content_length = None;
    for line in lines {
        let Some((name, value)) = line.split_once(':') else {
            return Err(BridgeError::service(
                "Runtime returned an invalid HTTP header",
            ));
        };
        if name.eq_ignore_ascii_case("transfer-encoding")
            && value.trim().eq_ignore_ascii_case("chunked")
        {
            chunked = true;
        }
        if name.eq_ignore_ascii_case("content-length") {
            content_length = value.trim().parse::<usize>().ok();
        }
    }
    let body = &raw[header_end + 4..];
    let body = if chunked {
        decode_chunked(body)?
    } else if let Some(length) = content_length {
        if length > body.len() || length > MAX_RESPONSE_BYTES {
            return Err(BridgeError::service("Runtime response length is invalid"));
        }
        body[..length].to_vec()
    } else {
        body.to_vec()
    };
    if body.len() > MAX_RESPONSE_BYTES {
        return Err(BridgeError::service(
            "Runtime response exceeds the hard size limit",
        ));
    }
    Ok(IpcResponse { status, body })
}

fn decode_chunked(mut raw: &[u8]) -> Result<Vec<u8>, BridgeError> {
    let mut decoded = Vec::new();
    loop {
        let line_end = raw
            .windows(2)
            .position(|window| window == b"\r\n")
            .ok_or_else(|| BridgeError::service("Runtime chunked response is invalid"))?;
        let size_text = std::str::from_utf8(&raw[..line_end])
            .map_err(|_| BridgeError::service("Runtime chunk size is invalid"))?;
        let size_text = size_text.split(';').next().unwrap_or_default();
        let size = usize::from_str_radix(size_text.trim(), 16)
            .map_err(|_| BridgeError::service("Runtime chunk size is invalid"))?;
        raw = &raw[line_end + 2..];
        if size == 0 {
            return Ok(decoded);
        }
        if size > raw.len().saturating_sub(2) || decoded.len() + size > MAX_RESPONSE_BYTES {
            return Err(BridgeError::service(
                "Runtime chunked response exceeds its limit",
            ));
        }
        decoded.extend_from_slice(&raw[..size]);
        if &raw[size..size + 2] != b"\r\n" {
            return Err(BridgeError::service("Runtime chunk terminator is invalid"));
        }
        raw = &raw[size + 2..];
    }
}

fn new_request_id() -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let serial = REQUEST_SERIAL.fetch_add(1, Ordering::Relaxed);
    format!("gui-{}-{nanos:x}-{serial:x}", std::process::id())
}

fn validate_operation_id(operation_id: &str) -> Result<(), BridgeError> {
    let valid = operation_id.starts_with("op_")
        && operation_id.len() <= 128
        && operation_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"-_.:".contains(&byte));
    if valid {
        Ok(())
    } else {
        Err(BridgeError::request("Runtime operation ID is invalid"))
    }
}

fn validate_identifier_list(values: &[String], label: &str) -> Result<(), BridgeError> {
    let valid = values.len() <= 512
        && values.iter().all(|value| {
            value.len() >= 3
                && value.len() <= 96
                && value
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || b"-_.:".contains(&byte))
        });
    if valid {
        Ok(())
    } else {
        Err(BridgeError::request(format!("{label} is invalid")))
    }
}

fn validate_gateway_exceptions(
    values: &[Value],
    allow_uid: bool,
) -> Result<(), BridgeError> {
    if values.len() > 128 {
        return Err(BridgeError::request(
            "Runtime gateway exception list exceeds 128 entries",
        ));
    }
    for value in values {
        let object = value
            .as_object()
            .ok_or_else(|| BridgeError::request("Runtime gateway exception must be an object"))?;
        if object.is_empty()
            || object.keys().any(|key| {
                !matches!(
                    key.as_str(),
                    "source_cidr" | "destination_cidr" | "destination_ports" | "uid"
                )
            })
        {
            return Err(BridgeError::request(
                "Runtime gateway exception contains invalid fields",
            ));
        }
        for key in ["source_cidr", "destination_cidr"] {
            if let Some(value) = object.get(key) {
                let text = value.as_str().ok_or_else(|| {
                    BridgeError::request("Runtime gateway exception CIDR must be a string")
                })?;
                if text.is_empty() || text.len() > 64 {
                    return Err(BridgeError::request(
                        "Runtime gateway exception CIDR is invalid",
                    ));
                }
            }
        }
        if let Some(ports) = object.get("destination_ports") {
            let ports = ports.as_array().ok_or_else(|| {
                BridgeError::request("Runtime gateway destination_ports must be an array")
            })?;
            if ports.len() > 128 {
                return Err(BridgeError::request(
                    "Runtime gateway destination port list is too large",
                ));
            }
            for port_range in ports {
                let port_range = port_range.as_object().ok_or_else(|| {
                    BridgeError::request("Runtime gateway port range must be an object")
                })?;
                if port_range
                    .keys()
                    .any(|key| !matches!(key.as_str(), "start" | "end"))
                    || port_range
                        .get("start")
                        .and_then(Value::as_u64)
                        .is_none_or(|port| port == 0 || port > 65535)
                    || port_range
                        .get("end")
                        .and_then(Value::as_u64)
                        .is_some_and(|port| port > 65535)
                {
                    return Err(BridgeError::request(
                        "Runtime gateway port range is invalid",
                    ));
                }
            }
        }
        if let Some(uid) = object.get("uid") {
            if !allow_uid || uid.as_u64().is_none_or(|value| value > u32::MAX.into()) {
                return Err(BridgeError::request(
                    "Runtime gateway exception UID is invalid",
                ));
            }
        }
    }
    Ok(())
}

fn validate_plan_id(plan_id: &str) -> Result<(), BridgeError> {
    let valid = plan_id.starts_with("plan_")
        && plan_id.len() == 37
        && plan_id[5..].bytes().all(|byte| byte.is_ascii_hexdigit());
    if valid {
        Ok(())
    } else {
        Err(BridgeError::request("Runtime network plan ID is invalid"))
    }
}

fn validate_source_id(source_id: &str) -> Result<(), BridgeError> {
    let valid = source_id.starts_with("src_")
        && source_id.len() == 36
        && source_id[4..].bytes().all(|byte| byte.is_ascii_hexdigit());
    if valid {
        Ok(())
    } else {
        Err(BridgeError::request("Runtime source ID is invalid"))
    }
}

fn validate_source_name(name: &str) -> Result<(), BridgeError> {
    let valid = !name.trim().is_empty() && name.len() <= 128 && !name.chars().any(char::is_control);
    if valid {
        Ok(())
    } else {
        Err(BridgeError::request("Runtime source name is invalid"))
    }
}

fn validate_source_route(route: &str) -> Result<(), BridgeError> {
    if matches!(route, "" | "direct" | "mihomo") {
        Ok(())
    } else {
        Err(BridgeError::request(
            "Remote source route must be direct or mihomo",
        ))
    }
}

fn validate_resource_kind(kind: &str) -> Result<(), BridgeError> {
    if matches!(
        kind,
        "proxy-provider-yaml" | "rule-provider-yaml" | "certificate-pem" | "private-key-pem"
    ) {
        Ok(())
    } else {
        Err(BridgeError::request(
            "Runtime managed resource kind is invalid",
        ))
    }
}

fn validate_resource_name(name: &str) -> Result<(), BridgeError> {
    let valid = !name.is_empty()
        && name.len() <= 128
        && !name.starts_with('.')
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"-_.:".contains(&byte));
    if valid {
        Ok(())
    } else {
        Err(BridgeError::request(
            "Runtime managed resource name is invalid",
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_content_length_response() {
        let raw = b"HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\n{\"ok\":true}";
        let response = parse_http_response(raw).expect("response should parse");
        assert_eq!(response.status, 200);
        assert_eq!(response.body, b"{\"ok\":true}");
    }

    #[test]
    fn parses_chunked_response() {
        let raw = b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4\r\n{\"ok\r\n7\r\n\":true}\r\n0\r\n\r\n";
        let response = parse_http_response(raw).expect("response should parse");
        assert_eq!(response.body, b"{\"ok\":true}");
    }

    #[test]
    fn generated_request_ids_are_unique_and_header_safe() {
        let first = new_request_id();
        let second = new_request_id();
        assert_ne!(first, second);
        assert!(
            first.chars().all(|character| {
                character.is_ascii_alphanumeric() || "-_.:".contains(character)
            })
        );
    }

    #[test]
    fn operation_ids_must_be_safe_path_segments() {
        assert!(validate_operation_id("op_0123456789abcdef").is_ok());
        assert!(validate_operation_id("op_a-b.c:d").is_ok());
        assert!(validate_operation_id("bad_0123456789abcdef").is_err());
        assert!(validate_operation_id("op_with/slash").is_err());
        assert!(validate_operation_id("op_with?query").is_err());
        assert!(validate_operation_id("op_with\r\nheader").is_err());
        assert!(validate_operation_id(&format!("op_{}", "a".repeat(126))).is_err());
    }

    #[test]
    fn source_ids_must_use_the_runtime_identifier_format() {
        assert!(validate_source_id("src_0123456789abcdef0123456789abcdef").is_ok());
        assert!(validate_source_id("src_0123456789ABCDEF0123456789ABCDEF").is_ok());
        assert!(validate_source_id("src_with/slash").is_err());
        assert!(validate_source_id("src_0123456789abcdef").is_err());
    }

    #[test]
    fn source_names_and_routes_are_validated_before_bridge_actions() {
        assert!(validate_source_name("local-copy").is_ok());
        assert!(validate_source_name("本机来源").is_ok());
        assert!(validate_source_name("").is_err());
        assert!(validate_source_name("bad\nname").is_err());
        assert!(validate_source_name(&"a".repeat(129)).is_err());
        assert!(validate_source_route("").is_ok());
        assert!(validate_source_route("direct").is_ok());
        assert!(validate_source_route("mihomo").is_ok());
        assert!(validate_source_route("arbitrary").is_err());
    }

    #[test]
    fn managed_resource_metadata_is_allowlisted() {
        assert!(validate_resource_name("provider.main").is_ok());
        assert!(validate_resource_name("../provider").is_err());
        assert!(validate_resource_name("provider/name").is_err());
        assert!(validate_resource_kind("proxy-provider-yaml").is_ok());
        assert!(validate_resource_kind("arbitrary-file").is_err());
    }
}
