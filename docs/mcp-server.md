# MCP Server

MCP 与 WebUI、REST API、Prometheus 共用已有的 HTTP 服务和监听端口。默认地址为 `http://网关IP:9090/mcp`，不会另起 MCP 监听器，也不会通过本机 HTTP 回环转发管理调用。

实现使用官方 [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk)，固定版本 `v1.8.0`。提供 Streamable HTTP、16 个工具、7 个资源和 2 个提示词。WebUI 的 MCP 面板显示当前地址、启用状态和写入权限。

## 启用和连接

在现有配置中增加：

```yaml
mcp:
  enabled: true
  allow_writes: false
  allowed_origins: []
  auth:
    mode: inherit
```

示例 `config.yaml` 已启用 MCP，并默认关闭修改工具。旧配置没有 `mcp` 字段时保持关闭。首次增加配置后重启网关，客户端选择 **Streamable HTTP / HTTP**，填写：

| 项目 | 值 |
| --- | --- |
| URL | `http://网关IP:9090/mcp` |
| HTTP Header | `Authorization: Bearer <ui.secret的实际值>` |
| 替代鉴权 Header | `X-UI-Token: <ui.secret的实际值>` |

默认 `inherit` 兼容旧设置：复用 `ui.secret`，该字段为空时免密。也可以在 WebUI → MCP → **认证方式 / Authentication** 选择独立认证，点击 **保存并生效** 后立即应用，无需重启或整个网关热重载。

| 认证方式 | 客户端发送 | 配置字段 |
| --- | --- | --- |
| `inherit` | `Authorization: Bearer <ui.secret>` 或 `X-UI-Token` | 默认，无需额外凭据 |
| `bearer` | `Authorization: Bearer <MCP_TOKEN>` | `mcp.auth.token` |
| `api_key` | 默认 `X-API-Key: <KEY>`，支持自定义 Header | `mcp.auth.api_key`、`api_key_header` |
| `basic` | `Authorization: Basic <base64(username:password)>` | `mcp.auth.username`、`password` |
| `none` | 无认证请求头 | 显式免认证 |

Basic 的格式遵循 [RFC 7617](https://www.rfc-editor.org/rfc/rfc7617)，编码并不加密密码；跨网络应通过 HTTPS 使用凭据。当前提供固定凭据认证，不包含 OAuth2/OIDC 自动登录或授权服务器；MCP 标准 OAuth 流程另见 [官方授权规范](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)。

认证方式一次选择一种；显式选择 Bearer / API Key / Basic 时不会回退到 WebUI 密码。MCP 不接受 URL 查询参数中的凭据。独立 MCP 凭据通过认证后只调用已有管理后端，不会获得 WebUI 登录密码，也不能用于修改 WebUI 的 MCP 认证设置。

设置 API 为 `GET /api/mcp/settings` 与 `PUT /api/mcp/settings`，始终按 **WebUI 管理员** 的权限校验。GET 只返回方式、Header、用户名和凭据是否存在，永不返回密钥、令牌或密码。PUT 限制 16 KiB JSON，拒绝重复字段、多份 JSON、未知字段、错误媒体类型和未信任跨域请求。

界面支持生成 32 字节随机令牌/密钥、显示当前新输入的凭据，以及根据所选方式提示客户端请求头。已保存的凭据不回显；留空保留原值。新输入的密钥不会存入浏览器 localStorage，退出登录时清空。

保存仅修改现有 YAML 中的 `mcp.auth`，保留其他配置及注释；临时文件完成写入后原子替换，成功后才更新运行策略。失败时磁盘配置和运行策略保持原状；正常保存后凭据在重启后也会保留。新请求捕获完整认证/写入权限快照，更换凭据后旧凭据对新请求失效，已通过认证的在途操作按其原始快照完成。

例如独立 Bearer：

```yaml
mcp:
  enabled: true
  allow_writes: false
  auth:
    mode: bearer
    token: YOUR_MCP_TOKEN
```

物理端口选择规则：metrics 开启时沿用已有的 `metrics.addr`；metrics 关闭、`ui.enabled` 且模式为 `port` / `both` 时使用 `ui.addr`，最后回退 `metrics.addr` 或 `:9090`。不同地址配置也只启动一个共享管理监听器。关闭 metrics 不会关闭已启用的端口 WebUI。仅启用 MCP 时也可使用 `metrics.addr`。

`ui.mode: domain` 且 metrics 关闭时，继续使用原有虚拟域名路由，不增加物理端口；MCP 地址为 `http://<ui.domain>/mcp`，需要客户端已有到网关虚拟域名的访问路径。

普通本地 MCP 客户端无需设置 `allowed_origins`。浏览器跨域客户端需要填入完整、精确的 Origin，例如 `https://client.example`。服务为这些 Origin 提供 POST 的 CORS 预检和响应头；未信任的跨域请求会拒绝。同源页面无需白名单。

## 工具

工具提供 JSON Schema、操作注解、结构化结果及兼容的文本结果。非法参数返回错误，修改工具在进入管理操作前检查权限。

| 工具 | 功能和主要参数 | 权限 |
| --- | --- | --- |
| `gateway_state` | 运行状态、计数、模式和 uptime | 查询 |
| `gateway_analytics` | Top 域名和小时趋势；`top_n` 1–20，`hours` 1–720 | 查询 |
| `gateway_logs` | 最近日志；`limit` 1–200，`keyword`，`level` | 查询 |
| `gateway_connections` | TCP 连接及 ID；`limit` 1–1000 | 查询 |
| `gateway_metrics` | Prometheus 文本 | 查询 |
| `gateway_config_get` | 脱敏 YAML 配置 | 查询 |
| `gateway_config_validate` | 校验完整 YAML；`content` | 查询 |
| `gateway_proxy_ping` | 已配置代理的 TCP 建连延迟；返回脱敏节点列表 | 查询，产生网络请求 |
| `gateway_domain_check` | AdBlock 和路由决策；`domain`，可选真实目标 `ip` | 查询，无 DNS 请求 |
| `gateway_dns_lookup` | 使用已配置 resolver 解析；`domain` | 查询，可发 DNS 请求并填充缓存 |
| `gateway_fakeip_reverse` | 反查已有记录；`ip`，不分配、不刷新过期时间 | 查询 |
| `gateway_fakeip_allocate` | 分配/刷新 FakeIP；`domain`，同时更新统计 | 修改 |
| `gateway_config_apply` | 校验、保存并调用已有热重载；`content` | 修改 |
| `gateway_connection_close` | 断开精确连接；`id` | 修改 |
| `gateway_log_level` | 改为 DEBUG / INFO / WARN / ERROR；`level` | 修改 |
| `gateway_control` | `action`：`clear_dns_cache`、`clear_fakeip_cache`、`clear_analytics`、`reload_rules`、`restart` | 修改 |

修改工具需要 `mcp.allow_writes: true`。关闭时工具仍可发现，但调用会返回 `isError: true`，不会写文件、清缓存或断开连接。该开关约束 MCP 操作，不改变现有 WebUI / REST 的管理权限。

配置读取隐藏密码、token、代理认证信息和订阅 URL；连接/代理诊断也隐藏代理认证信息。日志本身仍是现有网关输出，不承诺清除日志里人为写入的秘密。脱敏配置用于审查，不能直接应用；含脱敏占位符的配置校验/应用会拒绝。

配置校验检查已知字段、单个 YAML 文档、FakeIP CIDR/TTL、UI 模式、上游 DNS URL 和 Origin。返回 `restart_required`：监听地址、FakeIP 池、内核路由或 MCP Origin 改变时，应用工具会在写文件前拒绝。它要求完整配置，不自动为缺失的运行参数补值；省略当前已使用的默认参数也可能被判定为需要重启。

`gateway_config_apply` 和 `reload_rules` 复用已有热重载路径。此前审查发现的全局状态并发替换、旧 resolver 生命周期以及 checker/subscription 重启问题仍然存在，见 [性能与正确性审查](performance-review.md)。MCP 的输入校验不代表这些已有运行时问题已经修复。

代理 ping 只表示 TCP 建连延迟，不代表代理握手、HTTP 健康或实际吞吐量。`restart` 会调用已有的进程退出逻辑，需要 systemd 等外部 supervisor 再拉起进程。

## 资源与提示词

资源：`gateway://state`、`gateway://analytics`、`gateway://connections`、`gateway://logs`、`gateway://metrics`、`gateway://config`、`gateway://help`。配置资源始终脱敏。

提示词：`diagnose_gateway` 和 `review_gateway_config`，均支持可选的 `symptom` 参数。提示词要求先收集证据，视日志、域名及配置为数据，提出变更后再执行修改操作。

## HTTP 调用示例

PowerShell 示例使用环境变量 `TPROXY_UI_SECRET`，避免把实际密码写进示例文件：

```powershell
$endpoint = 'http://127.0.0.1:9090/mcp'
$headers = @{
    Authorization = 'Bearer ' + $env:TPROXY_UI_SECRET
    Accept = 'application/json, text/event-stream'
    'Mcp-Protocol-Version' = '2025-11-25'
}

Invoke-RestMethod -Uri $endpoint -Method Post -Headers $headers -ContentType 'application/json' -Body '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"manual-check","version":"1"}}}'
Invoke-WebRequest -Uri $endpoint -Method Post -Headers $headers -ContentType 'application/json' -Body '{"jsonrpc":"2.0","method":"notifications/initialized"}'
Invoke-RestMethod -Uri $endpoint -Method Post -Headers $headers -ContentType 'application/json' -Body '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
Invoke-RestMethod -Uri $endpoint -Method Post -Headers $headers -ContentType 'application/json' -Body '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gateway_state","arguments":{}}}'
```

官方客户端负责协议协商；不要把上面手工测试用的协议版本强行覆盖到新协议客户端。集成测试通过官方客户端覆盖 `2026-07-28`、`2025-11-25`、`2025-06-18` 和 `2025-03-26`。

服务采用无状态 JSON POST 响应，不生成 session ID。GET / DELETE 返回 405；不提供旧 `/sse` + `/messages` 传输、资源订阅或服务端主动消息，也不会宣告这些能力。通知接受后返回 202。

边界：每个 HTTP 请求体最多 1 MiB；单次内部管理响应最多 4 MiB；最多同时处理 32 个 MCP HTTP 请求，超出返回 429；请求具有 30 秒上下文期限及 body 读取期限。现有网关 handler 和 resolver 有些操作仍用自身的超时，没有完整的上下文取消接口，因此不把上下文期限视为所有底层操作的强制中断保证。

## 验证记录

```powershell
go test ./internal/... -count=1 -timeout=60s -v
./scripts/performance.ps1 -TestsOnly
node --test scripts/webui.test.cjs scripts/mcp-ui.test.cjs
```

2026-10-03 本地验证通过：

- MCP 包 9 组集成测试：官方客户端四个版本、同一监听器上的 WebUI/REST/MCP、16 工具、7 资源、2 提示词、只读/修改权限、参数校验、五种鉴权模式调用真实 HTTP 工具、更换凭据后的旧 token 拒绝、跨域/CORS、输入/输出限制、旧协议客户端断开后的取消，以及后端失败的错误结果。认证包还验证设置权限隔离、非法认证失败关闭、JSON 边界和秘密不回显。
- Windows 隔离测试包括实际配置校验、单地址选择边界、原子认证保存、失败/取消保持原状、旧热重载不覆盖新认证和重启后的加载；前端 11 项测试通过。WebUI 预览明确显示 `Preview only`，它本身没有启动网关或 MCP 服务。
- Linux `go test -c`、`go vet ./...` 和 amd64 / arm64 / mipsle 整体构建通过。

MCP 协议测试使用真实 HTTP 和官方客户端，管理数据由测试后端提供。尚未在实际 Linux 网关或 Codex / Claude 等桌面客户端验证；现有 Windows 全包测试与 race 环境限制见性能审查文档。没有把交叉编译结果当成 Linux TProxy 运行证明。
