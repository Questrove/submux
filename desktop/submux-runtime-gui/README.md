# Submux Runtime GUI

这是 Submux Runtime 的 Tauri 2 桌面客户端。WebView 只调用 `src-tauri`
注册的命令；Rust 后端通过 Unix Socket 或 Windows Named Pipe 访问 Runtime
的 `/v1` 本机 IPC，不打开 TCP 管理端口，也不连接 Mihomo。

开发构建需要 Rust stable、平台对应的 Tauri 2 系统依赖，以及已经运行的
`submux-runtime serve`：

```text
cd desktop/submux-runtime-gui/src-tauri
cargo run
```

发布构建通过 `SUBMUX_VERSION` 把与 Runtime 相同的产品版本传给 GUI。
本地未设置时使用 `dev`。GUI 每次写入前都会重新读取 Runtime 快照并检查
产品版本；不匹配时只保留只读状态，并提示重启或更新界面。

GUI 可以添加远程来源、显示脱敏目标和高风险兼容设置，并按保存线路、
直连或当前 Mihomo 手动刷新。WebView 只把来源草稿交给 Rust 桥接层，
由桥接层通过本机 IPC 上传；下载、地址检查、TLS、重定向、凭据隔离和
候选配置校验全部由 Runtime 执行。应用当前来源与启动 Mihomo 是两个明确
分开的操作。
