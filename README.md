# picomx

面向个人域名的轻量 self-host 邮件服务（早期原型）。

项目目标不是重新实现一套完整的群件系统，而是提供几个边界清晰的小工具：

- 用自己的域名收信，允许为每个网站使用不同地址；
- 将原始邮件存入 append-only 文件树，可用 Git 同步、用 notmuch 索引；
- 以尽量少的依赖和协议状态降低攻击面；
- 发信由用户选择的 MUA、outbound MTA 或 SMTP relay 完成。

当前仓库尚处于第一个可运行阶段。已经确定的范围和仍需选择的事项见
[docs/design.md](docs/design.md)。

当前可运行范围是入站 SMTP 存档：

- 只接受 `PICOMX_DOMAINS` 中域名的 catch-all 地址；
- 不实现 SMTP AUTH，也不会向外部域 relay；
- 将每封信原子发布为 `messages/YYYY/MM/*.eml`，发布后不再修改；
- 支持 STARTTLS、systemd socket activation 和结构化日志；
- 仅使用 Go 标准库。

picomx 是永久 receive-only 服务，不会实现出站队列、DKIM 签名或向 Gmail 投递。
发信程序与 picomx 只通过“使用同一个邮件域名”发生关系，不共享消息队列或状态。

## 开发

需要 Go 1.24 或更高版本。

```sh
make test
make build
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
sudoedit /etc/picomx/picomx.env
sudo systemctl enable --now picomx.socket
journalctl -u picomx.service -f
```

`make deploy` 只安装程序、配置样例和 systemd unit，不会替你修改 DNS，也不会自动启动
服务。启动前至少要把配置样例中的主机名、收件域和 TLS 证书路径改成真实值，并配置
域名的 MX 记录。运行用户需要能读取证书私钥；邮件存档由 systemd 创建在
`/var/lib/picomx/messages`，权限默认为仅 `picomx` 用户可访问。

## notmuch 与 Git

将 notmuch 的 database path 指向 `messages/` 后运行 `notmuch new` 即可索引。存档不是
Maildir，因此 notmuch tags 只存在索引中，不会反写为文件名 flags。

Git 只应同步年月目录。`messages/tmp/` 保存未发布写入并已由 `.gitignore` 排除；notmuch
数据库也不应提交。服务端仓库如何安全地 push/pull 仍由部署者选择，当前守护进程不会
执行 Git 命令或持有 Git 凭据。
