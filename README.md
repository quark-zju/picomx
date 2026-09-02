# picomx

面向个人域名的轻量 self-host 邮件服务（早期原型）。

项目目标不是重新实现一套完整的群件系统，而是提供几个边界清晰的小工具：

- 用自己的域名收信，允许为每个网站使用不同地址；
- 将原始邮件存入 append-only 文件树，可用 notmuch 索引；
- 以尽量少的依赖和协议状态降低攻击面；
- 发信由用户选择的 MUA、outbound MTA 或 SMTP relay 完成。

当前仓库尚处于早期阶段。已经确定的范围和仍需选择的事项见
[docs/design.md](docs/design.md)。POP3S、身份与 SMTP policy 接口见
[docs/policy-api.md](docs/policy-api.md)。

当前可运行范围包括入站 SMTP 存档和只读 POP3S：

- 只接受 `PICOMX_DOMAINS` 中的域名；默认接收这些域名下任意 `@` 前缀的地址；
- 不实现 SMTP AUTH，也不会向外部域 relay；
- 将每封信按连续 ID 原子发布到有界 fanout 文件树，发布后不再修改；
- POP3S 使用单一随机 app password，支持稳定 UIDL，不修改或删除归档；
- SMTP 收件和完整消息规则可由可选的本地 Go policy 配置；
- STARTTLS 和 POP3S 共享一张支持通配符查找与自动热加载的证书；
- 支持 systemd socket activation、结构化日志和可选 fail2ban 规则；
- 仅使用 Go 标准库。

picomx 是永久 receive-only 服务，不会实现出站队列、DKIM 签名或向 Gmail 投递。
发信程序与 picomx 只通过“使用同一个邮件域名”发生关系，不共享消息队列或状态。

## 开发

需要 Go 1.24 或更高版本。

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
覆盖证书目录）。邮件存档由 systemd 创建在
`/var/lib/picomx/messages`，权限默认为仅 `picomx` 用户可访问。

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
TLS），用户名和部署时生成的 app password；不要选择明文 POP3 或 STLS。客户端必须
启用“在服务器保留邮件”，因为 picomx 是只读归档，`DELE` 会明确失败。

如果证书、端口或收件域不匹配，优先依次检查 `dig` 结果、云/主机防火墙、
`systemctl status` 和 `journalctl`；修改证书后服务会自动热加载，无需重启。

重复 POP 认证失败的可选封禁规则通过 `make deploy-fail2ban` 安装。POP 客户端必须启用
“在服务器保留邮件”；`DELE` 会明确失败。

## notmuch 与 Git

将 notmuch 的 database path 指向 `messages/` 后运行 `notmuch new` 即可索引。存档不是
Maildir，因此 notmuch tags 只存在索引中，不会反写为文件名 flags。

Git 只应同步已发布的 ID 目录和 `state`。`messages/tmp/` 保存未发布写入并已由
`.gitignore` 排除；notmuch 数据库也不应提交。当前守护进程不会执行 Git 命令或持有
Git 凭据；有了 POP3S 后，Git 同步只是可选的归档传输方式。
