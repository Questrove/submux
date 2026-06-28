# submux

订阅聚合 / 配置编排服务(subscription multiplexer)。合并多个 Clash 订阅源,应用声明式 Merge YAML 覆盖,按客户端 UA 输出多格式订阅。**Go 单二进制,无运行时依赖。**

submux 自身不跑代理,只做「订阅加工厂」:输入多个订阅 → 合并 + 覆盖 → 输出一个聚合订阅 URL 给各客户端。

## 特性

- **多源合并**:去重、加来源前缀(`[源名] 节点名`)
- **声明式覆盖**(Merge YAML:`prepend-`/`append-`/深合并),无需脚本引擎
- **按 UA 多格式输出**:Clash YAML / Base64(sing-box 规划中)
- **上游拉取缓存**:解耦「客户端拉取频率」与「上游拉取频率」,避免机场限流;上游失败时用上次缓存降级
- **Web 控制台 + 鉴权**(登录 + 订阅 token)
- **聚合 `Subscription-Userinfo`**:汇总各源流量与到期

## 一键安装(Linux / macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash
```

装好后 `submux` 即在 PATH 中。带 systemd 服务(Linux):

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --service
```

## 构建

```sh
CGO_ENABLED=0 go build -o submux ./cmd/submux
```

纯 Go(`modernc.org/sqlite`,免 cgo),可交叉编译为各平台单文件。

## 运行

```sh
SUBMUX_DB=submux.db ./submux
```

默认监听 `127.0.0.1:8080`。首次打开 <http://127.0.0.1:8080> 设置管理员密码,系统自动生成订阅 token。

## 使用

1. 控制台「订阅源」添加机场订阅(填 URL,UA 可留空),点「刷新」拉取。
2. 「覆盖配置」写 Merge YAML(见下)。
3. 「订阅输出」复制订阅 URL,导入 mihomo / clash 等客户端;不同客户端按 UA 自动得到合适格式。

### 覆盖 YAML 示例

```yaml
prepend-rules:                 # 插到 rules 最前
  - DOMAIN-SUFFIX,example.com,DIRECT
  - DOMAIN-KEYWORD,mykeyword,DIRECT
dns:
  fallback: [tls://1.1.1.1:853]   # 数组直接覆盖
  append-fake-ip-filter:           # 追加到 dns.fake-ip-filter
    - "+.example.com"
tun:
  append-route-exclude-address:    # 追加到 tun.route-exclude-address
    - 192.168.1.0/24
```

合并规则:`prepend-X` / `append-X` 对**同层**数组 `X` 前插 / 后插;map 字段深合并;标量与普通数组直接覆盖。沿用 Clash Verge / mihomo party 的 Merge 约定。

## 部署:反向代理 + HTTPS(强烈建议)

submux 默认只监听本机。**对外提供订阅务必走 HTTPS**,否则订阅 token 会被中间人窃取。Nginx 示例:

```nginx
server {
    listen 443 ssl;
    server_name sub.example.com;
    ssl_certificate     /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;
    location / { proxy_pass http://127.0.0.1:8080; }
}
```

然后在控制台「设置」把 `base_url` 设为 `https://sub.example.com`,订阅 URL 即以该地址生成。

systemd 单元示例:

```ini
[Unit]
Description=submux
After=network.target
[Service]
ExecStart=/usr/local/bin/submux
Environment=SUBMUX_DB=/var/lib/submux/submux.db
Restart=on-failure
[Install]
WantedBy=multi-user.target
```

## 配置项(控制台「设置」)

| 项 | 默认 | 说明 |
|----|------|------|
| `listen_addr` | `127.0.0.1:8080` | 监听地址(重启生效) |
| `base_url` | (空) | 生成订阅 URL 的外部地址 |
| `fetch_interval_sec` | `10800` | 后台拉取上游间隔(秒) |
| `output_update_interval_hours` | `24` | 输出给客户端的 `Profile-Update-Interval` |
| `output_token` | (随机) | 订阅 URL token,可一键重置 |

## 安全

- 密码 bcrypt 哈希;会话 HMAC 签名 cookie(`HttpOnly` + `SameSite`)
- 订阅 token 常量时间比较;重置后旧链接立即失效
- 覆盖为纯数据合并,无脚本执行,无注入面

## Roadmap

sing-box 适配器、多 profile、本机内核管控(旁路由 / TUN)、base64 支持 vmess/trojan/ss。

设计文档见 [`docs/DESIGN.md`](docs/DESIGN.md)。
