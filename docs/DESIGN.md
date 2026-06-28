# submux 设计文档

## 概述

submux 是一个**订阅聚合 / 配置编排服务**(subscription multiplexer):把多个上游机场订阅合并成一份,叠加用户自定义的声明式覆盖配置,再按客户端类型输出聚合后的订阅。

它是一个 **Go 单二进制**,无系统 / 运行时依赖(`CGO_ENABLED=0` 可交叉编译出单文件),自带 Web 控制台。

**核心定位(订阅加工厂)**:submux 自己**不跑代理、不接管流量**。它只负责「输入多个订阅 → 合并 + 覆盖 → 输出一个聚合订阅 URL」,供各设备的代理客户端去订阅。

## 设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 语言 | Go,单二进制 | 体积小、无运行时依赖、好部署 |
| 角色 | 订阅加工厂(不跑代理) | 逻辑独立、好测;本机管控以后再加 |
| 覆盖逻辑 | **声明式 Merge YAML(零解释器)** | 覆盖本质是声明式「加 / 改」,无过程逻辑;避免塞入脚本解释器;复用 clash 生态 Merge 约定 |
| 输出格式 | 按 UA 多格式(clash + base64) | 多客户端通用;clash 是主链路,sing-box 列二期 |
| 访问控制 | 订阅 URL 带随机 token + 网页登录 | 对公网安全 |
| 缓存 | 只缓存「拉上游」这层;合并 / 覆盖 / 渲染实时算 | 解耦「客户端拉取频率」与「上游拉取频率」,**避免高频穿透打爆上游被限流**;渲染毫秒级,实时算保证改动立即生效 |
| 存储 | **bbolt**(纯 Go K-V) | 免 cgo、单文件、事务 + 崩溃安全;数据访问全是 K-V,不需要 SQL |

## 架构与组件

一个 Go 二进制,两条请求路径 + 一条后台定时任务:

```
                    ┌──────────────── submux (单 Go 二进制) ────────────────┐
客户端(带UA) ─GET /sub/{token}─►│ [A]HTTP → [C]合并主模型 → [D]Merge覆盖 → [E]UA分发适配器 │─► clash / base64
                    │                                                       │
浏览器 ──登录────────►│ [A]控制台 API + [G]内嵌前端                            │
                    │                                                       │
  上游机场订阅 ◄──定时拉取── [B]Fetcher/缓存 ──► [F]bbolt                    │
                    └───────────────────────────────────────────────────────┘
```

| 模块 | 包 | 职责 |
|------|-----|------|
| A HTTP 层 | `internal/server` | 路由、登录鉴权、`/sub/{token}` 输出端点 |
| B 源管理 + 拉取 | `internal/source` | 源 CRUD;后台定时拉原文 → 解析 → 写缓存;手动刷新;拉失败降级 |
| C 解析 + 合并 | `internal/parse`, `internal/merge` | 解析上游(clash yaml)→ 节点;合并多源 → clash 主模型(去重、来源前缀) |
| D 覆盖引擎 | `internal/override` | 把 Merge YAML 按约定合并进主模型 |
| E 输出适配器 | `internal/output` | `Render(主模型) → (bytes, contentType)`;UA 分发;Clash / Base64 适配器 |
| F 存储 | `internal/store` | bbolt:源、覆盖、设置、缓存 |
| G Web UI | `web/`(embed.FS) | 登录、源管理、覆盖编辑、URL / 预览、状态、刷新 |

**模块边界原则**:各包单一职责、通过明确接口通信、可独立测试。`output.Adapter` 是接口,新增格式只加一个实现 + 注册到 UA 分发器;`store` 接口与后端实现解耦(从 sqlite 迁到 bbolt 时其余包零改动)。

## 存储模型(bbolt)

bbolt 是纯 Go 的 B+tree 单文件 K-V。用 4 个 bucket,值用 JSON 序列化:

| bucket | key → value |
|--------|-------------|
| `settings` | `admin_pw_hash` / `session_secret` / `output_token` / `fetch_interval_sec` / `output_update_interval_hours` / `listen_addr` / `base_url` / `default_format` |
| `sources` | `id(8字节)` → Source{name,url,user_agent,enabled,sort_order,timestamps} |
| `source_cache` | `source_id(8字节)` → Cache{raw,nodes,userinfo,last_success_at,last_error} |
| `meta` | `override` → Merge YAML;`lastgood:<format>` → 上次成功渲染输出 |

SQL 的过滤 / 排序改为内存遍历(`ListEnabled`、按 `sort_order` 排序),外键级联改为删除源时手动删对应 cache,id 用 bucket `NextSequence()`。合并后的主模型**不入库**,每次请求实时算。

## 覆盖机制:Merge YAML

覆盖配置是一段 YAML,按 **Clash Verge / mihomo party 的 Merge 约定**合并进主模型;submux 内部展开成标准完整 clash yaml 再输出——**客户端拿到的是普通配置,不含任何 Merge 指令**。

**合并规则**:
1. `prepend-<key>` / `append-<key>`:对**同一层级**的数组字段前插 / 后插,指令键本身不写入结果
2. map 字段:递归深合并
3. 标量 / 普通数组:直接覆盖

示例:

```yaml
prepend-rules:                      # 插到 rules 最前
  - DOMAIN-SUFFIX,example.com,DIRECT
  - DOMAIN-KEYWORD,mykeyword,DIRECT
dns:
  fallback: [tls://1.1.1.1:853]     # 数组直接覆盖
  append-fake-ip-filter:            # 追加到 dns.fake-ip-filter
    - "+.example.com"
tun:
  append-route-exclude-address:     # 追加到 tun.route-exclude-address
    - 192.168.1.0/24
```

覆盖引擎纯声明式、零计算、无脚本执行,因此也无注入面。

## 数据流

**输出请求 `GET /sub/{token}`**:
1. 校验 token(常量时间比较),失败 → 401
2. 读所有 enabled 源的缓存节点;全部为空 → 503
3. **合并**:节点去重(server+port+uuid)、加来源前缀(`[源名] 节点名`)、组装 clash 主模型
4. **覆盖**:把 Merge YAML 合并进主模型
5. **UA 分发**:`clash`/`mihomo`/`meta` → Clash;`sing-box` → 回退默认(二期);其它 → Base64
6. **渲染** → 返回,设响应头:`Subscription-Userinfo`(各源聚合)、`Content-Disposition`、`Profile-Update-Interval`

**后台定时拉取**:每 `fetch_interval_sec` 对每个 enabled 源带其 UA 拉原文 → 解析 → 写缓存;失败只写 `last_error`、保留上次结果(stale-while-error)。**请求路径只读缓存,永不主动拉上游。**

## 错误处理与安全

- **上游拉取失败**:用上次缓存降级,控制台标红显示错误与上次成功时间
- **覆盖 / 渲染失败**:**绝不输出未覆盖的配置**(否则本该 DIRECT 的流量可能误走代理);返回上次成功输出 + 控制台报错,无则 503
- **鉴权**:密码 bcrypt 哈希;HMAC 签名 session cookie(`HttpOnly` + `SameSite`);输出 token 常量时间比较、可重置
- **部署**:默认监听 `127.0.0.1`;对外务必配反向代理 + HTTPS,订阅 URL 必须走 HTTPS 防 token 被窃

## Roadmap

- sing-box 输出适配器
- 多 profile(不同源组合 / 覆盖 / token)
- 本机 mihomo 内核管控(旁路由 / TUN)
- base64 支持 vmess / trojan / ss
- 覆盖编辑器换 CodeMirror
