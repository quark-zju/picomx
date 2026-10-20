# picomx

面向个人域名的轻量 self-host 邮件服务（早期原型）。

项目目标不是重新实现一套完整的群件系统，而是提供几个边界清晰的小工具：

- 用自己的域名收信，允许为每个网站使用不同地址；
- 将原始邮件存入 bare Git 仓库，可删除当前邮件并保留历史；
- 以尽量少的依赖和协议状态降低攻击面；
- 发信由用户选择的 MUA、outbound MTA 或 SMTP relay 完成。

当前仓库尚处于早期阶段。已经确定的范围和仍需选择的事项见
[docs/design.md](docs/design.md)。POP3S、身份与 SMTP policy 接口见
[docs/policy-api.md](docs/policy-api.md)。

当前可运行范围包括入站 SMTP 存档和支持删除的 POP3S：

- 只接受 `PICOMX_DOMAINS` 中的域名；默认接收这些域名下任意 `@` 前缀的地址；
- 不实现 SMTP AUTH，也不会向外部域 relay；
- 将每封信按递增且不复用的 ID 保存到有界 fanout 文件树，收信和删除均提交 Git；
- POP3S 使用单一随机 app password，支持稳定 UIDL、并发会话快照和批量删除；
- SMTP 收件和完整消息规则可由可选的本地 Go policy 配置；
- STARTTLS 和 POP3S 共享一张支持通配符查找与自动热加载的证书；
- 支持 systemd socket activation、结构化日志和可选 fail2ban 规则；
- Go 代码仅使用标准库，运行时通过本地 Git 保存历史。

picomx 是永久 receive-only 服务，不会实现出站队列、DKIM 签名或向 Gmail 投递。
发信程序与 picomx 只通过“使用同一个邮件域名”发生关系，不共享消息队列或状态。

## 开发

需要 Go 1.24 或更高版本，以及 Git 2.36 或更高版本。部署服务器也必须安装 Git。

```sh
make test
make build
# 可选：创建并编辑本机 SMTP policy
make config
```

本机开发可绕过 systemd，在非特权端口监听：

```sh
go run ./cmd/picomxd \
  --listen 127.0.0.1:2525 \
  --hostname mx.mail.example \
  --domains mail.example \
  --archive-root ./messages
```

不传 `--listen` 时，服务要求 systemd 传入监听 socket。公网部署的基本步骤是：

```sh
make deploy
# 记下首次部署时只显示一次的 POP3 app password
sudoedit /etc/picomx/picomx.env
sudo systemctl enable --now picomx.socket
journalctl -u picomx.service -f
```

如果本机无法使用 TCP/25，可以为 `picomx.socket` 创建本地 systemd override，例如将 SMTP
改为 2525（POP3S 固定为 995）：

```sh
make override-systemd SMTP_PORT=2525
sudo systemctl restart picomx.socket
```

该规则会生成并打开编辑 `/etc/systemd/system/picomx.socket.d/override.conf`。其中空的
`ListenStream=` 用于清除原 unit 的监听列表，请保留。改用非标准 SMTP 端口后，公网其他
邮件服务器通常仍无法直接向本机投递；如果 25 端口不可用，可考虑使用外部邮件服务（例如
purelymail）作为 relay，并让 picomx 在可用的非标准端口接收本地流量。

`make deploy` 只安装程序、配置样例和 systemd unit，不会替你修改 DNS，也不会自动启动
服务。下面是一套可直接套用的公网配置（假设服务器公网 IPv4 为 `203.0.113.10`，
收信域为 `example.net`，SMTP/POP3S 主机名为 `mx.example.net`）。

### 1. 配置 DNS

在 `example.net` 的 DNS 区域添加：

```dns
mx.example.net.  IN A   203.0.113.10
example.net.     IN MX  10 mx.example.net.
@                IN TXT "v=spf1 -all"
_dmarc           IN TXT "v=DMARC1; p=reject;"
```

如果服务器使用 IPv6，同时添加正确的 `AAAA` 记录；如果 IPv6 不可用，不要添加会把
邮件引到错误地址的 `AAAA`。MX 的右侧必须是主机名，不能直接写 IP。DNS 生效后可用
`dig +short MX example.net` 和 `dig +short A mx.example.net` 检查。

`PICOMX_DOMAINS` 中写的是用户地址的域名（例如 `alice@example.net` 的
`example.net`），`PICOMX_HOSTNAME` 写的是 SMTP 服务主机名（`mx.example.net`）。
只有前者决定 picomx 接受哪些 RCPT TO 地址；后者用于 SMTP EHLO，也应当与 MX 指向的
主机名一致。不要把 `mx.example.net` 填进 `PICOMX_DOMAINS`，除非你确实要接收该域名
下的地址。

### 2. 准备证书

为 `mx.example.net` 申请一张包含该名字的证书（例如 ACME/Let's Encrypt），并确保
证书和私钥最终能被 `picomx` 用户读取。证书目录按主机名组织，文件名固定为：

```text
/etc/picosrv/certs/mx.example.net/fullchain.pem
/etc/picosrv/certs/mx.example.net/privkey.pem
```

也可以把证书放在父域目录（例如 `example.net/`）；picomx 会从精确主机名目录逐级
向父域查找，但证书仍必须覆盖所有配置的 TLS 主机名。SMTP STARTTLS 和 POP3S/995
共享这张证书。若 POP3S 使用另一个名字，例如 `pop.example.net`，DNS、证书和下面的
`PICOMX_POP3_HOSTNAME`、`PICOMX_TLS_HOSTNAMES` 都必须一并加入。

### 3. 填写运行配置

编辑 `make deploy` 安装的 `/etc/picomx/picomx.env`：

```ini
PICOMX_HOSTNAME=mx.example.net
PICOMX_DOMAINS=example.net
PICOMX_ARCHIVE_ROOT=/var/lib/picomx/messages
PICOMX_CERT_DIR=/etc/picosrv/certs
PICOMX_POP3_HOSTNAME=mx.example.net
PICOMX_TLS_HOSTNAMES=mx.example.net
```

`make deploy` 首次部署时会显示一次 POP3 app password，并把用户名和密码摘要写入
配置；请立即保存密码。运行用户需要能读取证书私钥；若复用 picosrv 证书目录，部署后运行
`make facl` 为 `picomx` 用户添加只读 ACL（可用 `CERT_DIR=/path/to/certs make facl`
覆盖证书目录）。systemd 创建 `/var/lib/picomx`；picomx 自动在
`/var/lib/picomx/messages` 初始化 bare Git 仓库，权限默认为仅 `picomx` 用户可访问。
已有文件存档会在首次启动时导入 Git，后续收信和删除都在其历史上追加提交。

### 4. 放通端口并启动

在云防火墙和主机防火墙中只放通 TCP/25（其他邮件服务器投递）和 TCP/995（自己的
POP3S 客户端）；不要放通未使用的明文 POP3/110、IMAP 或 submission 端口。然后启动：

```sh
sudo systemctl enable --now picomx.socket
sudo systemctl status picomx.socket picomx.service
sudo journalctl -u picomx.service -f
```

### 5. 验证收信和取信

先确认端口和 TLS：

```sh
openssl s_client -connect mx.example.net:995 -servername mx.example.net
openssl s_client -starttls smtp -connect mx.example.net:25 -servername mx.example.net
```

向 `anything@example.net` 发一封测试邮件；日志中应看到 SMTP 接受并发布消息。POP
客户端应设置为：服务器 `mx.example.net`、端口 `995`、SSL/TLS 为“连接时启用”（隐式
TLS），用户名和部署时生成的 app password；不要选择明文 POP3 或 STLS。客户端可以选择保留服务器邮件，或下载后删除。`DELE` 只在本会话中标记，
正常 `QUIT` 时才批量提交删除；`RSET` 撤销标记，断线不删除。删除后原文仍保留在 Git 历史中。
删除提交的说明正文记录 `POP3 user: <已认证用户名>`，Git committer 仍为 picomx。
目前 POP3 没有可用的客户端 User-Agent 信息，不会把邮件中的发件客户端头当作登录客户端。

如果证书、端口或收件域不匹配，优先依次检查 `dig` 结果、云/主机防火墙、
`systemctl status` 和 `journalctl`；修改证书后服务会自动热加载，无需重启。

重复 POP 认证失败的可选封禁规则通过 `make deploy-fail2ban` 安装。

## 每日收信预算

默认按 UTC 自然日限制为 50 MiB（52,428,800 字节），通过 `PICOMX_MAX_DAILY_BYTES`
或 `--max-daily-bytes` 调整。按最终归档邮件的字节数扣额度，包含服务器添加的头部；
每封至少扣 4 KiB，限制大量小邮件产生的 Git 对象开销。预算是整个归档共享的。
接收正文时预留额度，约束并发暂存。当天用量保存为归档目录同级的独立 JSON 文件：
默认 `/var/lib/picomx/messages.rate-limit.json`（即归档路径加 `.rate-limit.json`），
权限 `0600`，通过临时文件、fsync、原子替换和父目录 fsync 更新，不进入 Git 提交。
先持久化扣额，再提交邮件；Git 失败或异常中断可能多计一次，但不会漏计已发布邮件。
策略拒绝、暂存失败或提交前断线释放预留；删除、重启不返还已扣额度。UTC 换日刷新，
时钟回拨不重置已记录日期。旧版 Git state 若有用量，在独立文件缺失时自动转存一次；
随后只以独立文件为准。新 Git state 不再写入计数，历史提交保持原样。
已有非 Git 归档从启用此功能后的新投递开始计费。

已耗尽时在 DATA 的 `354` 前返回 `452`；流式接收期间超额也返回 `452` 并关闭连接，
发送方可稍后重试。每日预算限制归档增长速度，不是磁盘剩余空间保证；Git metadata、
临时文件和历史仍占空间，需保留运维余量。

## bare Git 与历史恢复

`messages/` 本身是 bare Git 仓库，没有工作目录；`state` 和 `.eml` 路径存在于 Git
提交的树中。邮件按原始字节保存，不经过换行转换或 clean/smudge 过滤。文件名包含
大小，例如 `1/001-s123.eml` 是 ID 1、123 字节。`state` 维护各目录存活邮件数和总字节数；
`last_id` 始终递增，删除全部邮件也不会重用旧 ID。
物理目录中的 `tmp/` 只存放未发布邮件，不进入提交。

```sh
sudo -u picomx git --git-dir=/var/lib/picomx/messages show HEAD:state
sudo -u picomx git --git-dir=/var/lib/picomx/messages ls-tree -r HEAD
```

守护进程通过 `fast-import` 写入提交，每次变更执行 checkpoint 并确认 HEAD 更新后才
返回成功。另一个常驻 `cat-file --batch-command` 读取 tree 和正文；二者空闲 30 秒后
通过 EOF 退出，下次操作按需启动。正文直接走 Git 管道，关闭 reader 时丢弃未读部分并
恢复协议边界，不创建正文临时文件。目录 LRU 缓存最多 64 个叶目录，每个至多 1024 封；
以目录路径和 tree OID 为键，内容未变时可跨提交复用。`LIST` 从文件名取得大小，
列目录、单封大小查询和读取正文均复用 reader，不启动逐次 `ls-tree` / `cat-file -s`。
reader 和 writer 使用独立锁和空闲计时器；慢 POP3 正文传输不阻塞 SMTP 提交，等待
reader 的删除请求也不会占用状态锁。普通 Git 操作超时为 30 秒，reader 失败后可重建，
写入结果不确定时拒绝继续写入并要求重启。

服务默认每 6 小时、或累计 256 次提交后尝试 Git 压缩；通过
`PICOMX_GIT_MAINTENANCE_INTERVAL` / `--git-maintenance-interval` 调整周期。reader 或
writer 正忙时延后；执行时暂停 Git 访问、关闭持久进程，再前台运行
`git -c gc.autoDetach=false gc --prune=never`，最长 5 分钟。它将 loose objects 打入 pack
并压缩历史，保持 HEAD、UIDL 和已删除邮件的可达历史；需要额外临时磁盘空间。
Git 只做本地操作，禁用 hooks、fsmonitor、提交签名和隐式自动维护，不持有 Git 凭据；
周期维护由服务协调，不创建脱离协调的后台 GC。
systemd 的 `ProtectSystem=strict`、`ProtectHome=yes` 和出站 `connect(2)` 禁令保持不变。

已有普通文件存档会在首次启动时导入 bare 仓库，保留原 `.eml` 文件供管理员确认；
后续以 Git 中的 state 为准，不再读取或修改这些旧副本。确认迁移和备份后可自行
清理旧副本。迁移入口仅支持原始非 Git、版本 1 文件存档；未部署的中间 Git 格式
不提供迁移。已有 `.git/` 的工作目录会被拒绝，不自动转换或删除。

查看某封已删除邮件的历史并导出原文（以下示例对应 ID 123、456 字节；将路径和 `COMMIT` 替换为实际值）：

```sh
sudo -u picomx git --git-dir=/var/lib/picomx/messages log --oneline -- 1/07b-s456.eml
umask 077
sudo -u picomx git --git-dir=/var/lib/picomx/messages show COMMIT:1/07b-s456.eml > recovered-123.eml
```

仓库由单个 picomx 进程管理。外部写入、恢复和 Git 维护应先停止服务；不要重置或改写
历史，以免丢失原文和复用旧 UIDL。远程备份由服务之外的程序完成，应包含完整 Git
历史。notmuch 如有需要，可在外部检出或导出的文件树上索引，不直接索引 bare 仓库。

正常启动直接读取 HEAD 中的 state 并构建目录索引，不扫描每封邮件或查询其 blob 大小；
仓库要求只由 picomx 一致写入。路径、响应 framing、目录列表和正文大小在使用时检查。
如果 HEAD/ref 丢失但已有 Git 数据，拒绝重新导入旧文件，需先修复 HEAD。
遇到 Git 事务故障后，排除磁盘或锁文件问题并重启；picomx 从 HEAD 读取权威状态。
提交成功但响应中断时，客户端可能认为操作失败，SMTP 重试可能产生重复邮件。
