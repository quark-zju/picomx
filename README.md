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

`make deploy` 只安装程序、配置样例和 systemd unit，不会替你修改 DNS，也不会自动启动
服务。启动前至少要把配置样例中的主机名、收件域和证书目录改成真实值，并配置域名的
MX 记录。运行用户需要能读取证书私钥；若复用 picosrv 证书目录，可为 `picomx` 用户
添加只读 ACL。邮件存档由 systemd 创建在
`/var/lib/picomx/messages`，权限默认为仅 `picomx` 用户可访问。

重复 POP 认证失败的可选封禁规则通过 `make deploy-fail2ban` 安装。POP 客户端必须启用
“在服务器保留邮件”；`DELE` 会明确失败。

## notmuch 与 Git

将 notmuch 的 database path 指向 `messages/` 后运行 `notmuch new` 即可索引。存档不是
Maildir，因此 notmuch tags 只存在索引中，不会反写为文件名 flags。

Git 只应同步已发布的 ID 目录和 `state`。`messages/tmp/` 保存未发布写入并已由
`.gitignore` 排除；notmuch 数据库也不应提交。当前守护进程不会执行 Git 命令或持有
Git 凭据；有了 POP3S 后，Git 同步只是可选的归档传输方式。
