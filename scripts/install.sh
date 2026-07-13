#!/usr/bin/env bash
# submux 一键安装脚本
#
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --service     # 同时装 systemd 服务(Linux)
#
# 环境变量:
#   VERSION       指定版本(默认取最新 release)
#   INSTALL_DIR   安装目录(默认 /usr/local/bin)
set -euo pipefail

REPO="Questrove/submux"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
WITH_SERVICE=0
[ "${1:-}" = "--service" ] && WITH_SERVICE=1

# --- 检测平台 ---
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  aarch64 | arm64) arch="arm64" ;;
  *) echo "不支持的架构: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux | darwin) ;;
  *) echo "不支持的系统: $os(仅 linux / macOS)" >&2; exit 1 ;;
esac
asset="submux-${os}-${arch}"

# --- 取版本 ---
version="${VERSION:-}"
if [ -z "$version" ]; then
  version="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | head -1 | cut -d'"' -f4)"
fi
[ -n "$version" ] || { echo "无法获取最新版本" >&2; exit 1; }

url="https://github.com/${REPO}/releases/download/${version}/${asset}"
echo "下载 submux ${version} (${os}/${arch}) ..."
tmp="$(mktemp)"
curl -fsSL "$url" -o "$tmp"
chmod +x "$tmp"

# --- 安装 ---
SUDO=""
[ -w "$INSTALL_DIR" ] || SUDO="sudo"
$SUDO mv "$tmp" "${INSTALL_DIR}/submux"
echo "✓ 已安装: ${INSTALL_DIR}/submux"

# --- 可选 systemd 服务 ---
if [ "$WITH_SERVICE" = "1" ]; then
  [ "$os" = "linux" ] || { echo "systemd 服务仅支持 Linux,已跳过。"; exit 0; }
  echo "配置 systemd 服务 ..."
  id submux >/dev/null 2>&1 || sudo useradd --system --no-create-home --shell /usr/sbin/nologin submux
  sudo mkdir -p /var/lib/submux
  sudo chown submux:submux /var/lib/submux
  sudo tee /etc/systemd/system/submux.service >/dev/null <<EOF
[Unit]
Description=submux subscription aggregator
After=network.target

[Service]
ExecStart=${INSTALL_DIR}/submux
Environment=SUBMUX_DB=/var/lib/submux/submux.db
User=submux
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
  sudo systemctl daemon-reload
  sudo systemctl enable --now submux
  echo "✓ submux 服务已启动(默认监听 127.0.0.1:8080)"
  echo "⚠ 对外提供请务必配反向代理 + HTTPS(见 README)"
else
  echo "运行: SUBMUX_DB=submux.db submux   然后打开 http://127.0.0.1:8080"
fi
