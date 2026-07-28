# Submux Runtime 只接受本机管理

submux 只生成和发布输出订阅；Submux Runtime 不注册设备、不发送心跳，也不接受由 submux 发起的运行操作。GUI、TUI 和 CLI 通过本机 Unix Socket 或 Windows Named Pipe 管理一台机器上的一个 Runtime 和一个 Mihomo。此前的远程运行端便于集中操作，却把控制面失陷扩大为主机运行面风险，并要求长期维护设备身份、重试与离线状态；新的本机边界以放弃远程管理换取更小的攻击面、独立部署和明确的机器所有权。
