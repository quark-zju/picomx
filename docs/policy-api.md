# picomx 策略接口草案

本文是待评审设计，不是兼容性承诺。目标是让本机管理员用 Go 编写收件、反垃圾和
POP3S 可见性规则，同时让协议实现继续负责状态机、错误码、资源限制与秘密处理。

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

建议的公共接口轮廓：

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
    RecipientAccept RecipientAction = iota
    RecipientRejectUnknown // 映射为 550 5.1.1
    RecipientRejectPolicy  // 映射为 550 5.7.1
    RecipientTempFail      // 映射为 451 4.7.1
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
    MessageAccept MessageAction = iota
    MessageRejectPolicy // 映射为 550 5.7.1
    MessageTempFail     // 映射为 451 4.7.1
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
envelope recipients、接收时间、字节数和内部 message ID。不要把原始命令行或日志对象
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

## POP3 的 append-only 语义

- 认证成功只取得 `{last ID, total octets}` 快照，不扫描或复制邮件元数据；
- archive ID 直接作为稳定的 message number，UIDL 使用其可打印编码；
- 实现 `CAPA`、`USER`、`PASS`、`STAT`、`LIST`、`UIDL`、`RETR`、`TOP`、`NOOP`、`RSET`、
  `QUIT`；
- `DELE` 始终返回 `-ERR archive is read-only`，`RSET` 因而是无操作；
- 客户端必须配置为“在服务器保留邮件”；
- 不保存已读、下载时间或每客户端游标，所有同步状态留给客户端；
- archive 中的消息在写入时规范化为 CRLF，POP 的 `LIST` octet count 与 `RETR` 一致；
- `STAT`、带参数的 `LIST`/`UIDL` 和单封读取均为常数或有界目录访问；只有客户端明确
  请求不带参数的 `LIST`/`UIDL` 时才遍历或生成全部邮件项。

## 待确认项

1. anti-spam 首版是否只需 accept/reject/tempfail，还是必须有 append-only quarantine。
