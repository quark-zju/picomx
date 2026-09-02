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

## POP3S 身份与可见性

POP3S 使用 995 端口 implicit TLS，不提供明文 POP3 或 STLS。认证成功后返回一个稳定的
identity 和一个只负责授权的 mailbox view：

```go
type POP3Policy interface {
    Authenticate(AuthRequest) AuthDecision
}

type AuthRequest struct {
    Username   string
    Password   PasswordCheck
    RemoteIP   netip.Addr
    TLSVersion uint16
}

type PasswordCheck interface {
    // 比较服务端计算的 SHA-256 与配置中的摘要，全程 constant-time。
    // 只适用于至少 128 bit 的随机 app password，不适用于人类短密码。
    MatchesSHA256(expected [32]byte) bool
}

type AuthAction int
const (
    AuthDeny AuthAction = iota
    AuthAllow
    AuthTempFail
)

type AuthDecision struct {
    Action   AuthAction
    Identity string
    View     MailboxView // 仅 AuthAllow 时使用
    Reason   string
}

type MailboxView interface {
    Allows(MessageMetadata) bool
}
```

配置代码看不到可打印、可记录的原始密码，只能要求框架做一种受支持的安全比较。轻量
首版建议生成高熵 app password，并在配置中保存 SHA-256 摘要；若必须支持人类密码，另加
Argon2id PHC verifier，并接受 `golang.org/x/crypto` 这一项经过审计的依赖，不自行实现
密码 KDF。

`MailboxView` 通常按 envelope recipient 判断，而不是按可伪造的原始 `To`/`Cc` 头判断：

```go
type myView struct{}

func (myView) Allows(m MessageMetadata) bool {
    return slices.ContainsFunc(m.Recipients, func(a Address) bool {
        return a.Domain == "example.net" &&
            (a.Local == "me" || strings.HasPrefix(a.Local, "shop-"))
    })
}
```

一封信有多个 envelope recipients 时，只要其中一个地址被 view 允许就可见。身份验证和
逐封授权分开，使一个身份可看多个地址、多个身份也可看同一封邮件，而不复制 `.eml`。

## POP3 的 append-only 语义

- 认证成功时取得不可变快照；本次连接内 message number 不变化，新邮件下次连接可见；
- `UIDL` 使用不可变相对路径的 SHA-256（64 个十六进制字符），跨会话稳定且不泄漏路径；
- 实现 `CAPA`、`USER`、`PASS`、`STAT`、`LIST`、`UIDL`、`RETR`、`TOP`、`NOOP`、`RSET`、
  `QUIT`；
- `DELE` 始终返回 `-ERR archive is read-only`，`RSET` 因而是无操作；
- 客户端必须配置为“在服务器保留邮件”；
- 不保存已读、下载时间或每客户端游标，所有同步状态留给客户端；
- archive 中的消息在写入时规范化为 CRLF，POP 的 `LIST` octet count 与 `RETR` 一致。

POP 进程启动时扫描不可变年月目录，只从文件开头由 picomx 生成、位于本机 `Received`
头之前的 `Delivered-To` 块重建内存元数据，忽略原始邮件中可能伪造的同名头。之后只重扫
目录 mtime 变化的月份并解析新文件，不引入数据库或可变服务端索引。每个会话在认证完成
时复制符合 `MailboxView` 的轻量元数据切片，打开邮件时再次验证路径仍位于 archive root
且是 regular file。

## 待确认项

1. 是否接受 POP3S 永久 read-only，`DELE` 总是失败；
2. 是否只支持随机 app password，还是首版就引入 Argon2id 支持人类密码；
3. anti-spam 首版是否只需 accept/reject/tempfail，还是必须有 append-only quarantine；
4. 地址授权是否始终以 SMTP envelope recipient 为准。
