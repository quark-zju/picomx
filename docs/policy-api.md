# picomx 策略与 POP3S 接口

本文描述当前接口方向，但仍不承诺长期 API 兼容。目标是让本机管理员用 Go 编写收件和
反垃圾规则，同时让协议实现继续负责状态机、错误码、资源限制与秘密处理。

## 进程与权限边界

picomx 以一个非 root 进程同时提供 SMTP/25 和 POP3S/995。两种协议共享归档和 TLS
证书，分别限制连接数，并继续由 systemd 禁止主动 `connect(2)`。这意味着 SMTP 解析器、
POP 解析器和 POP 凭据处于同一信任边界；对于单管理员、低流量服务，这一取舍优先保持
代码和部署简单。

可选 Go 配置只用于 SMTP 收件策略，放在被版本控制忽略的
`internal/config/custom_local.go`，部署时与程序一起编译。POP 首版使用内置的单一随机
app password 认证；环境配置缺失或不完整时认证全部失败。

## SMTP 策略

SMTP 分两阶段决策。RCPT 阶段只提供 envelope 信息，适合低成本地址规则；完整 DATA
进入有上限的 unpublished staging file 后才运行消息规则。只有规则接受且文件持久化后
才返回 `250`。规则拒绝时直接在当前 SMTP 会话返回 5xx，绝不先接受再产生 backscatter。

当前公共接口轮廓：

```go
type SMTPPolicy interface {
    EvaluateRecipient(RecipientRequest) RecipientDecision
    EvaluateMessage(MessageRequest) MessageDecision
}

type RecipientRequest struct {
    Connection ConnectionInfo
    EnvelopeFrom Address // 可表示 null reverse-path
    Recipient Address
}

type RecipientAction int
const (
    RecipientRejectPolicy RecipientAction = iota // 映射为 550 5.7.1
    RecipientRejectUnknown                       // 映射为 550 5.1.1
    RecipientTempFail      // 映射为 451 4.7.1
    RecipientAccept
)

type RecipientDecision struct {
    Action RecipientAction
    Reason string // 只进结构化日志，不直接成为 SMTP 文本
}

type MessageRequest interface {
    Metadata() MessageMetadata
    Header() mail.Header
    Open() (io.ReadCloser, error) // 读取有硬上限的 staging message
}

type MessageAction int
const (
    MessageTempFail MessageAction = iota // 映射为 451 4.7.1
    MessageRejectPolicy                 // 映射为 550 5.7.1
    MessageAccept
)

type MessageDecision struct {
    Action MessageAction
    Reason string
}
```

首版不允许 policy 自选数字状态码，避免再次产生协议不准确；也不提供 silent discard，
因为它会向发送端谎报成功。若以后需要隔离区，应增加明确的 `MessageQuarantine`，仍以
`250` 接受并写入独立 append-only 树。

`MessageMetadata` 只包含已经规范化的值：远端 IP、EHLO、是否 TLS、envelope-from、全部
envelope recipients、接收时间和字节数。不要把原始命令行或日志对象
直接交给策略。邮件头有单独上限；正文仅在规则调用 `Open` 时读取。

规则在 SMTP 进程内运行，因此必须是本地、确定且有界的。systemd 继续禁止出站
`connect(2)`；DNSBL、HTTP API 或复杂扫描器若将来需要，应作为显式 sidecar 设计，而不是
悄悄扩大 SMTP 进程权限。

## POP3S 身份

POP3S 使用 995 端口 implicit TLS，不提供明文 POP3 或 STLS。首版只有一个 mailbox，
所有成功认证的客户端看见整个 archive，不提供 Go 认证 policy 或逐地址授权。

部署生成至少 128 bit 的随机 app password，配置只保存用户名和 SHA-256 摘要。框架同时
hash 用户名和密码并 constant-time 比较；环境配置缺失或不完整时全部认证失败。USER
响应不泄漏用户名是否存在；每次失败 PASS 产生 fail2ban 可匹配的结构化日志。

## POP3 快照与删除语义

- 认证成功取得固定的 `{last_id,total_octets,deleted_ranges,commit}` 快照，O(1) 引用
  不可变的删除区间，不扫描或复制逐邮件元数据；
- 支持并发认证会话，每个会话的 POP3 message number 从 1 到存活邮件数；通过累计
  删除数二分映射到永久 archive ID，UIDL 继续由该永久 ID 编码，跨会话保持稳定；
- 实现 `CAPA`、`USER`、`PASS`、`STAT`、`LIST`、`UIDL`、`RETR`、`TOP`、`DELE`、`NOOP`、
  `RSET`、`QUIT`；
- `DELE` 只标记本会话的邮件，立即从 `STAT`、`LIST`、`UIDL` 中排除；后续访问该
  message number 返回错误，剩余邮件在本会话中不重新编号；
- `RSET` 撤销全部标记，断线或超时不提交删除；正常 `QUIT` 将标记批量提交 Git；
- 批量删除合并到最新邮箱状态，重复删除视为成功，新收邮件不受影响。Git 事务失败
  返回 `-ERR` 并关闭连接；引用已更新时删除可能已生效，重启从 HEAD 读取权威状态；
- 当前树中的删除不移除祖先提交中的原文；`last_id` 不回退，旧 ID 不复用；
- 读取快照提交中的原文，因此其他客户端删除后，本会话仍能读取；
- 客户端可配置为保留服务器邮件，也可下载后删除；已读、下载时间和客户端游标
  仍由客户端自行管理；
- 消息在写入时规范化为 CRLF，Git 保存原始字节，`LIST` octet count 与 `RETR` 一致；
- `STAT` 为 O(1)，单封序号定位为 O(log R)，完整列表遍历为 O(N+R)；R 为删除区间数，
  N 为快照存活邮件数。这些是内存索引成本，不包括读取 Git 对象的 I/O 和进程开销。

首版只实现 accept、reject 和 tempfail，不提供 silent discard 或 quarantine。
