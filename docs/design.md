# picomx 设计草案

## 目标

picomx 服务一个管理员、少量域名和低邮件量。它偏向“可审计的小组件”，不追求
Webmail、日历、通讯录、邮件列表、多租户管理或与现有 MTA 的插件兼容性。

典型地址为 `shop-name@example.net`。首版对配置域名采用 catch-all：合法 local-part
都可收件，但不会把 local-part 当文件路径。这样不需要为每个购物网站预先创建账户，
泄漏后也容易根据 `To` / `Delivered-To` 定位来源。

## 协议边界

```text
Internet MTA --SMTP/25--+
                         +--> picomx --> append-only archive --> git / notmuch
MUA <--POP3S/995---------+

MUA --> 独立 outbound MTA 或 SMTP relay --> Gmail 等收件方
```

“不用 POP3/IMAP”只适用于用户设备与邮件库之间的同步。公网邮件服务器之间收发邮件
仍然必须使用 SMTP。Git 同步的是关闭连接后已经原子落盘的文件，不同步临时文件，也
不承载投递协议。

picomx 监听 SMTP/25 和 POP3S/995，但不提供认证邮件提交，避免成为 open relay。出站
服务与 picomx 不共享代码、进程、队列或权限；即使出站工具配置错误，也不会扩大公网
入站服务的协议面。

SMTP 和 POP3S 由同一个非 root 进程提供，共享归档和 TLS 证书。该选择接受 SMTP
解析器与 POP 凭据位于同一信任边界，以换取更小的代码和部署面。两种协议分别限制
连接数，避免 POP 登录攻击耗尽 SMTP 接收能力。进程仍禁止主动建立网络连接。

## 存储布局

存档按年月分区，不保存可变的已读状态：

```text
messages/
  tmp/
  2026/
    08/
      1788070123.12345_1.host.eml
```

邮件先以 `0600` 权限写入同一文件系统的 `tmp/` 并执行 `fsync`，再用 hard link 原子
发布到年月目录。hard link 在目标已存在时失败，因此不会像 rename 那样覆盖已有文件。
成功发布后的文件不改名、不改内容、不表达已读状态。文件名由时间、进程号、单调计数
和主机名组成，不使用主题、地址或 Message-ID。

notmuch 可以递归索引任意“一封邮件一个文件”的树，不要求 Maildir；只是这种布局不能
把 notmuch tags 同步为文件名 flags，符合存档不可变的目标。Git 仓库提交年月目录，
`.gitignore` 忽略 `tmp/`，避免同步半封邮件。notmuch 数据库不应提交：它可重建、
经常变化，并包含可还原的邮件内容。

## 入站 SMTP 的首版约束

- 只接收配置域名的 RCPT，不转发到任意外部地址；
- 支持 EHLO/HELO、MAIL FROM、RCPT TO、DATA、RSET、NOOP、QUIT；
- 对命令行、DATA 行、总邮件大小、收件人数、并发连接和空闲时间设硬上限；
- DATA 成功响应只在文件原子持久化之后返回；
- 保存原始 RFC 5322 内容，并在前面添加 `Return-Path`、`Delivered-To` 和本机
  `Received` 投递头；
- JSON 结构化日志只记录 envelope、大小、远端地址和结果，不记录正文；
- 由 systemd socket activation 持有 25 端口，服务进程不使用 root。

catch-all 会增加垃圾邮件与磁盘耗尽风险。“暂不做收信反垃圾”不应取消资源限制；首版
仍需拒绝未知域、过大邮件、过多收件人和超时连接。后续可以增加地址令牌或显式地址表，
而不改变磁盘格式。

## 外部发信边界

picomx 不实现 outbound SMTP。用户可以按需求选择：

- MUA 直接连接可信 SMTP relay：最少运维，适合人工写信；
- OpenSMTPD outbound-only：配置较小，具备持久队列，DKIM 可交给独立 filter；
- Postfix outbound-only：队列、重试、退信与运维生态最成熟，配置面相对较大；
- msmtp：适合同步提交到 relay，但本身不作为可靠的持久队列；
- dma：小型本地 MTA，适合通过 smarthost 发送。

如果希望使用 `shop-name@example.net` 一类自定义发件地址，relay 应验证并授权整个
`example.net` 域，而不是只授权一个 mailbox，并为该域生成对齐的 DKIM 签名。SPF、
DKIM、DMARC、PTR、队列重试和 Gmail 可投递性全部属于所选 outbound 系统的责任。

将 outbound 从 picomx 删除是安全边界，不只是延期：picomx 不读取 DKIM 私钥、不持有
relay 凭据、不解析本地 submission，也不需要出站网络权限。随附的 systemd unit 进一步
禁止 `connect(2)`，使服务即使出现意外代码路径也不能建立出站连接。

## 刻意不做

- 明文 POP3、IMAP、Webmail、JMAP；
- outbound SMTP、DKIM 签名、SMTP AUTH 或公网 submission 端口；
- 服务器端全文索引（交给 notmuch）；
- 数据库、消息去重、自动分类、入站内容反垃圾；
- 任意转发规则和多租户管理面。

## 尚需管理员选择

以下信息不阻塞入站核心和存储实现，但会阻塞真正公网部署：

1. 收信域名与 MX/EHLO 主机名；
2. catch-all 是否长期保留，还是以后只允许显式地址/带随机令牌的地址；
3. Git 同步的拓扑（服务器 push、客户端 pull，或客户端经 SSH pull）以及邮件静态加密需求。

POP3S 与可编程策略的拟议边界见 [policy-api.md](policy-api.md)，尚未实现。
