# 性能审查与 WebUI 改进

审查日期：2026-10-03。基线提交：`e144a79dad7e995f5be4ca0f41d136497522e987`，当前工作区为 detached HEAD。本次改动保留在当前工作区，未提交或推送。

## 已完成的改动

- `analytics.go`：Top N 从全量排序改为容量为 N 的最小堆；只在离开统计锁后排序候选和小时趋势。同分域名按名称排序，返回结果稳定。缓存当前小时桶，减少每次 DNS/TCP/UDP 计数的时间格式化和字符串分配。
- `analytics.go`：复制持久化快照后在锁外编码 JSON，避免 JSON 编码长期阻塞计数。串行化保存，并在写入/重命名失败时保留 dirty 标记；并发的新计数不会被保存结束时的标记复位覆盖。代价是保存期间需要额外的完整快照内存。
- `adblock.go`：下载和解析在查询路径之外完成，使用不可变 map 和原子指针发布；查询不再获取共享锁。刷新失败或仅部分源成功时保留旧快照，成功刷新会移除过时规则。新增简单 `||domain^`、多域名 hosts 和 IPv6 hosts 支持；没有实现完整 AdBlock 语法。代价是刷新时同时保留新旧规则集。
- `log.go`：真正的固定容量环形缓存，满载后直接覆盖，读取仍按时间顺序返回独立副本。时间戳生成移到锁外。
- `tproxy.go`、`inbound_proxy.go`：TCP、UDP、HTTP 入站目的端点统一用 `net.JoinHostPort`，修复裸 IPv6 地址拼接；UDP 读取循环在取消或 socket 关闭时退出，避免关闭后的忙循环。
- `webui/index.html`、`webui/theme.css`、`webui/theme.js`：重做卡片、排版、导航、图表、表格、登录和编辑器样式；支持白天、黑夜和跟随系统，保留选择并同步标签页。移除持续运动的大面积模糊背景；支持减少动画设置和手机布局。
- WebUI 刷新改为完成后计时，隐藏页面和登录界面停止轮询；相同 GET 合并请求，超时后释放请求。未变化的图表、连接表和日志不再替换 DOM，阅读旧日志时不强制滚到底部。图表按钮只操作自身筛选组，30 天的 720 根柱子不会超出绘图区。修复日志等级及图表标题的 HTML 转义。

## 基准结果

环境：Windows amd64，Intel Core i9-13900K，Go 1.26.4，默认 GOMAXPROCS=32。前后各运行 3 次，表中耗时为中位数；使用 Go 默认基准运行时长，没有网络 I/O。输入固定、连续执行，没有到达率、排队或尾延迟测量。这些是本机微基准，不能用于声称 Linux 实际转发吞吐提升相同倍数。

| 场景 | 优化前 | 优化后 | 每次分配：前 → 后 |
| --- | ---: | ---: | ---: |
| 1,000 域名取 Top 20 | 127.4 µs | 9.78 µs | 24,696 B → 600 B |
| 100,000 域名取 Top 20 | 9.63 ms | 0.608 ms | 2,400,418 B → 600 B |
| 同一小写域名的 DNS 计数 | 196.7 ns | 72.84 ns | 16 B / 1 alloc → 0 B / 0 alloc |
| 已满的 500 条日志缓存写入 | 270.1 ns | 158.8 ns | 131 B → 16 B（时间戳字符串） |
| AdBlock 并行未命中查询 | 88.88 ns/op | 4.216 ns/op | 0 B → 0 B |

Top N 输入为 `host-NNNNNN.example`，计数为 `index % 1000`，基准不包含小时历史。DNS 计数基准预填 1,000 域名，重复记录同一域名。AdBlock 输入为一个 `ads.example.org` 规则，查询 `assets.example.org`，使用 `RunParallel`；此项 ns/op 表示并行吞吐摊销成本，不是单请求延迟。

100,000 域名 Top 20 的三轮耗时范围：优化前 9.27–12.24 ms，优化后 0.603–0.624 ms，改善方向稳定。DNS 计数优化后波动较大（47.16–72.96 ns）；没有把一次最快结果当作效果。没有测量生产 CPU/RSS、真实 DNS 或 TCP/UDP 吞吐。

原始输出：`performance-before.txt`、`performance-after.txt`。AdBlock 的有效基线在 `performance-adblock-before.txt`；首次空规则集基准已从主基线文件剔除，不能和有规则查询比较。

## 已完成验证与边界

- Windows 上执行 `scripts/performance.ps1 -TestsOnly`：9 项 Go 回归测试通过，覆盖 Top N 与完整排序对照（含同分）、小时切换、清空、并发计数/保存、保存失败、日志顺序/快照、AdBlock 替换/失败/慢下载及解析。
- `node --test scripts/webui.test.cjs`：6 项前端逻辑测试通过，覆盖主题选择/系统变化/存储不可用、隐藏及认证暂停、刷新不重入、DOM 去重、GET 合并、HTTP 错误及过期认证响应。POST 配置错误响应仍可由页面读取并展示。
- 实际浏览器验证：白天/黑夜切换、刷新记忆、7 天/30 天筛选和日志等级互不干扰；390 px 手机视口没有横向溢出；720 根柱子的最右端不超过绘图区；未发现浏览器控制台错误。
- Linux amd64 全包 `go test -c`、`go build`、`go vet ./...` 和 `git diff --check` 通过。交叉编译没有执行 Linux 测试二进制。
- 全包原生 Windows 测试仍被仓库已有 Linux socket API 阻塞。性能脚本在临时目录中复制平台无关文件，并仅为未测试的管理路由提供编译占位，因此其结果不代表完整管理 API 或 TProxy 运行验证。
- race 未验证：默认 CGO 关闭；开启 CGO 后 C 工具链缺少 `windows.h`。没有安装新工具链。真实 Linux TProxy、nftables、UDP socket、端到端流量和生产负载均未验证。

## 仍需修复的原有问题

以下为当前代码路径审查所得，本轮没有扩大到运行时架构重构；不能据本次微基准判断这些问题已经消失。

| 优先级 | 位置与证据 | 影响 | 建议与验证 |
| --- | --- | --- | --- |
| P1 | `main.go:87–96`、`metrics.go:238–245` 直接替换全局 cfg/resolver/checker；新 checker/subscription 没有 Start；`resolver.go:259` 清理 goroutine 没有取消入口 | 热重载的数据竞争、后台任务失效及旧 resolver goroutine 积累 | 发布一致的运行时快照，取消并等待旧后台任务，区分需重启的配置；在 Linux race 下反复重载并检查任务数量和规则生效 |
| P1 | `tproxy.go:478` 在 RLock 下写 LastActive；`tproxy.go:652` 无锁写；`tproxy.go:524` 会话创建缺少第二次检查；清理仅按 key 删除 | 并发读写时间、重复拨号、旧会话删除新会话；锁内网络写入还可能阻塞会话表 | 会话自身同步、合并建连、删除前核对对象身份、网络 I/O 移到锁外；用并发 UDP 压测及 race 验证 |
| P1 | `fakeip.go:301–311` 按 IP 分片下标删除域名分片，而域名分片按 domain hash 决定；ExpiresAt 在不同 shard 锁下写 | 到期时反向索引和域名索引可能不一致；共享记录仍有数据竞争 | 统一记录所有权，按域名 hash 和记录身份删除两侧索引；测试过期后重分配及双栈并发查询 |
| P1 | `tproxy.go:499–505` 将 IPv4/IPv6 original-destination 消息都按 28 字节 IPv6 sockaddr 解析 | 普通 16 字节 IPv4 original-destination 被丢弃 | 按 cmsg level/type 分别解码 sockaddr_in/sockaddr_in6；补 Linux 控制消息测试及真实双栈 UDP 流量验证 |
| P1 | `tproxy.go:199` 的 https 代理仍只进行普通 TCP Dial 后发 CONNECT；`resolver.go:94` 将 doq 映射到 tcp-tls | 与配置协议不一致：HTTPS 缺 TLS、DoQ 实际是 DoT | HTTPS 使用受验证的 TLS；DoQ 正确实现 QUIC 或明确拒绝；通过协议端点集成测试验证 |
| P2 | `analytics.go` 的 topDomains 没有容量或保留期，仅小时桶有 720 条限制 | 不同域名持续增长会增大常驻内存和持久化快照 | 定义保留期或可配置容量，明确对精确排名的影响；用高基数长期负载测量 RSS 和保存时延 |
| P2 | `config.yaml:32` 默认 admin123；`metrics.go:46` 支持 URL token；`rule_subscription.go:73–97` 仅解析 CSV 并把规则放在现有 MATCH 后 | 默认管理密码、URL 密钥泄漏风险；常见 payload 订阅或追加规则可能不生效 | 安装时生成随机密码，仅接受认证请求头；增加 payload 解析并把订阅插入 MATCH 前，补认证和规则优先级测试 |

## 复现

Windows（Go 测试脚本隔离已有 Linux-only 代码）：

```powershell
.\scripts\performance.ps1 -TestsOnly
.\scripts\performance.ps1 -Count 3
node --test scripts/webui.test.cjs
python scripts/preview.py --port 8765
```

预览仅绑定 `127.0.0.1`，访问 `http://127.0.0.1:8765/ui/`。页面中的指标是明确标注的模拟数据，不会启动网关、保存配置、清缓存、重启进程或拨号。

Linux 上仍需执行：

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench . -benchmem -count=3
```
