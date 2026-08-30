# picomail 设计草案

## 目标

picomail 服务一个管理员、少量域名和低邮件量。它偏向“可审计的小组件”，不追求
Webmail、日历、通讯录、邮件列表、多租户管理或与现有 MTA 的插件兼容性。

典型地址为 `shop-name@example.net`。首版对配置域名采用 catch-all：合法 local-part
都可收件，但不会把 local-part 当文件路径。这样不需要为每个购物网站预先创建账户，
泄漏后也容易根据 `To` / `Delivered-To` 定位来源。

## 协议边界

```text
Internet MTA --SMTP/25--> picomaild --> append-only archive --> git / notmuch

本地写信工具 --> durable outbox --> DKIM signer --> SMTP/MX --> Gmail 等收件方
```

“不用 POP3/IMAP”只适用于用户设备与邮件库之间的同步。公网邮件服务器之间收发邮件
仍然必须使用 SMTP。Git 同步的是关闭连接后已经原子落盘的文件，不同步临时文件，也
不承载投递协议。

入站与出站可以在同一个小二进制中共享配置和存储代码，但应使用不同监听面和不同
systemd 权限。入站服务只监听公网 25 端口，不提供认证邮件提交，避免成为 open relay。
出站入口默认仅接受本机 stdin 或 Unix socket，不对公网开放。

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

## 出站与 Gmail 可投递性

直接投递到 Gmail 不只取决于 SMTP 客户端。上线前至少需要：

- 稳定公网 IP；正向 A/AAAA 与反向 PTR 一致；25 端口可出站；
- envelope-from 的 SPF；From 域对齐的 DKIM；DMARC 记录；
- opportunistic TLS、合规 RFC 5322 格式、稳定 EHLO 主机名；
- 持久 outbox、指数退避、区分 4xx/5xx、退信保存和重复投递防护；
- 低投诉率与逐步建立的 IP/域名信誉。

因此首版不提供“收到本地请求就同步直发”的假实现：没有 durable queue 会在进程崩溃或
对方 4xx 时丢信；没有 DKIM 与域名配置也无法完成预期的 Gmail 投递目标。出站实现将是
下一个独立里程碑。

## 刻意不做

- POP3、IMAP、Webmail、JMAP；
- SMTP AUTH 或公网 submission 端口；
- 服务器端全文索引（交给 notmuch）；
- 数据库、消息去重、自动分类、入站内容反垃圾；
- 任意转发规则和多租户管理面。

## 尚需管理员选择

以下信息不阻塞入站核心和存储实现，但会阻塞真正公网部署或出站：

1. 收发信域名、MX/EHLO 主机名，以及服务器是否有可设置 PTR 的固定 IPv4；
2. 是坚持直接投递 Gmail，还是允许用一个轻量 SMTP relay 提升初期可投递性；
3. catch-all 是否长期保留，还是以后只允许显式地址/带随机令牌的地址；
4. Git 同步的拓扑（服务器 push、客户端 pull，或客户端经 SSH pull）以及邮件静态加密需求。
