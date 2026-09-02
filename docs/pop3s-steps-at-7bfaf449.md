建议分成约 16–19 个小提交。测试与实现放在同一个原子提交里，不单独补一个“大测试提交”。

## 第一阶段：确认设计

| # | 提交标题 | 内容 |
|---|---|---|
| 1 | `docs: adopt single-process SMTP and POP3S` | 把两进程方案改成单进程、单用户，记录权限取舍及独立连接额度。 |
| 2 | `docs: specify lazy append-only POP mailbox` | 记录只读 POP、连续 ID、O(1) 快照、只有无参数 `LIST/UIDL` 才 O(N)。 |
| 3 | `docs: specify fixed TLS certificate discovery` | 记录单证书、父域目录查找、通配符验证、自动 reload 与过期告警。 |

## 第二阶段：重做 archive 身份与元数据

| # | 提交标题 | 内容 |
|---|---|---|
| 4 | `archive: encode IDs as bounded radix paths` | 实现 base-1024 变深路径的纯函数和边界测试，不改写入流程。 |
| 5 | `archive: persist mailbox summary atomically` | 引入带版本的 `last_id`、`total_octets` 小状态文件和原子更新。 |
| 6 | `archive: publish messages with sequential IDs` | 用连续 ID 和新路径替换年月目录；发布、目录 fsync、状态更新在同一锁内。 |
| 7 | `archive: recover interrupted tail publication` | 处理“消息已发布、状态尚未更新”的单尾部崩溃窗口；损坏则 fail closed。 |
| 8 | `archive: expose lazy mailbox access` | 提供 `Snapshot()`、`Open(id)`、`Size(id)`，不扫描整个 archive。 |
| 9 | `smtp: canonicalize stored messages to CRLF` | 确保存储大小等于 POP `LIST` 大小，dot-stuffing 不计入大小。 |

第 5–8 步比较敏感，每个提交都需要覆盖断电顺序、碰撞、状态损坏和 ID 边界测试。

## 第三阶段：证书处理

| # | 提交标题 | 内容 |
|---|---|---|
| 10 | `tls: discover one certificate by service hostname` | 从 `cert-dir` 按完整域名、父域查找一张证书，并验证覆盖全部 SMTP/POP hostname。 |
| 11 | `tls: reload changed certificate atomically` | 定期 hash PEM；新证书有效才替换，失败保留旧证书。 |
| 12 | `tls: report certificate expiration` | 添加剩余 30/7/1 天 warning 和过期 error，并抑制重复日志。 |

证书查找与 reload 分开提交，是因为前者属于选择正确证书，后者属于运行期状态和并发正确性。

## 第四阶段：POP3S

| # | 提交标题 | 内容 |
|---|---|---|
| 13 | `pop3: implement authorization state` | implicit TLS、greeting、`CAPA/USER/PASS/QUIT`、常量时间认证、失败次数限制。 |
| 14 | `pop3: implement lazy mailbox listings` | `STAT`、`LIST [n]`、`UIDL [n]`；认证只捕获 last ID 和 total octets。 |
| 15 | `pop3: stream message retrieval` | `RETR`、`TOP`、dot-stuffing、行尾和响应大小处理。 |
| 16 | `pop3: enforce read-only mailbox semantics` | `DELE` 明确失败，`RSET/NOOP/QUIT`，至少 10 分钟 idle timeout。 |
| 17 | `runtime: serve SMTP and POP3S together` | 单进程启动两个 listener，共享 archive 和证书，但使用独立连接额度。 |

其中 `UIDL` 可以直接由 archive ID 编码生成；无参数 `UIDL` 虽然 O(N)，但无需访问 N 个文件。

## 第五阶段：部署与认证秘密

| # | 提交标题 | 内容 |
|---|---|---|
| 18 | `deploy: generate POP3 app credentials` | env 不存在时生成随机 app password，保存 username 与 SHA-256；运行时缺失则认证全失败。 |
| 19 | `deploy: activate POP3S and fail2ban` | systemd 增加 995 socket、证书目录只读权限、fail2ban filter/jail、配置样例。 |

fail2ban 日志行为应随第 13 步实现和测试，第 19 步只安装部署文件。

## SMTP 可编程策略

它最好作为后续独立阶段，不和 POP3S 混在同一批提交：

1. `policy: define fail-closed SMTP decisions`
2. `smtp: evaluate recipient policy`
3. `archive: separate staging from publication`
4. `smtp: evaluate staged message policy`
5. `config: add optional local Go policy`

消息策略需要重构 staging 生命周期，风险和 POP3 协议实现不同。先完成 POP3S，可以更快得到一个可用且容易验证的版本；默认 SMTP 行为保持现状。

每个提交都会：

- 包含对应测试；
- 执行 `gofmt` 和 `go test ./...`；
- 使用项目要求的 `User request:`、`Decision:`，正确性相关提交再带 `Result:` body。

开始前还有一个迁移问题：当前 `YYYY/MM/*.eml` 与新连续 ID 布局不兼容。如果已有需要保留的邮件，应该额外增加一个显式、可重复运行的 migration 命令及独立提交；如果现有 archive 只是测试数据，则可以直接采用新格式，不背兼容包袱。我倾向默认“不兼容旧布局、不在服务启动时偷偷全量迁移”。
