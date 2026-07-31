# Submux Linux airgap kit

本目录包含控制面、Submux Runtime、固定的官方 Mihomo 以及离线验证材料，供 systemd、glibc Linux 主机在无网环境中安装。

先检查文件是否完整：

```sh
./install.sh verify
```

安装控制面和 Runtime：

```sh
sudo ./install.sh all
```

只安装其中一个时，把 `all` 改为 `control` 或 `runtime`。桌面机器需要授权一个本机用户时使用：

```sh
sudo ./install.sh runtime --authorize-desktop "$USER"
```

安装器会保留 `/var/lib/submux`、`/var/lib/submux-runtime` 和特权 Runtime 状态。它不会创建配置来源，不会启用 TUN 或网关，也不会自动启动代理。修复同版本 Runtime 使用 `--repair`；降级必须同时使用 `--allow-downgrade --database-compatible`。

控制面和 Runtime 是两个独立安装。`all` 按顺序执行两者；如果 Runtime 阶段失败，已经成功安装的控制面会保留，可以排除错误后运行 `sudo ./install.sh runtime` 重试。
