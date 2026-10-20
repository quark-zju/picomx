# picomx 设计草案

## 目标

picomx 服务一个管理员、少量域名和低邮件量。它偏向“可审计的小组件”，不追求
Webmail、日历、通讯录、邮件列表、多租户管理或与现有 MTA 的插件兼容性。

典型地址为 `shop-name@example.net`。默认采用“全收地址”（catch-all）模式：
只要 `@` 后的域名在配置中，`@` 前的地址部分（邮件术语为 local-part）
可以是任意合法字符串。例如配置 `example.net` 后，
`shop-name@example.net` 和 `random-token@example.net` 都会收件。picomx 不会把
这部分当作文件路径。这样不需要为每个网站预先创建账户，地址泄漏后也容易
根据 `To` / `Delivered-To` 定位来源。

## 协议边界

```text
Internet MTA --SMTP/25--+
                         +--> picomx --> Git archive --> optional backup / notmuch
MUA <--POP3S/995---------+

MUA --> 独立 outbound MTA 或 SMTP relay --> Gmail 等收件方
```

“不用 POP3/IMAP”只适用于用户设备与邮件库之间的同步。公网邮件服务器之间收发邮件
仍然必须使用 SMTP。POP3S 是用户访问存档的协议；如果管理员另行使用 Git 备份，
它应同步完整 Git 历史，不同步临时文件，也不承载投递协议。

picomx 监听 SMTP/25 和 POP3S/995，但不提供认证邮件提交，避免成为 open relay。出站
服务与 picomx 不共享代码、进程、队列或权限；即使出站工具配置错误，也不会扩大公网
入站服务的协议面。

SMTP 和 POP3S 由同一个非 root 进程提供，共享归档和 TLS 证书。该选择接受 SMTP
解析器与 POP 凭据位于同一信任边界，以换取更小的代码和部署面。两种协议分别限制
连接数，避免 POP 登录攻击耗尽 SMTP 接收能力。进程仍禁止主动建立网络连接。

## TLS 证书

SMTP STARTTLS 和 POP3S 使用同一张固定证书。配置给出证书目录和全部服务 hostname；
picomx 从主 hostname 开始逐级尝试完整域名和父域目录，例如依次查找
`mx.example.net/`、`example.net/`。候选证书必须通过 `VerifyHostname` 覆盖所有配置的
hostname，因此可以复用父域目录中的通配符证书，但不依赖客户端发送 SNI。

进程缓存一张已经解析的证书。后台定期读取 PEM 并比较内容 hash；内容变化时只有在
证书、私钥匹配且证书当前有效并覆盖全部 hostname 后才原子替换，失败时继续使用旧值。
同时按剩余 30 天、7 天、1 天和已经过期记录限频结构化日志。picomx 只负责发现和热
加载续期结果，不负责 ACME 申请或续期。

## 存储布局

存档根目录本身是 bare Git 仓库。物理布局为：

```text
messages/
  HEAD
  config
  objects/
  refs/
  tmp/
```

Git 提交中的逻辑树沿用有界 fanout 和递增、不复用的 archive ID。ID 以 base-1024
digit 编码；第一层记录 digit 数，任意 ID 目录最多约 1024 项：

```text
state
1/001.eml
1/3ff.eml
2/001/000.eml
```

邮件先以 `0600` 权限写入 `tmp/` 并执行 fsync。收信和批量删除均由同一把锁串行处理，
通过常驻 `git fast-import --quiet` 写入 blob、tree、commit，无需 index 或工作目录。
每个 commit 显式以前一个已确认的 HEAD 为父提交，将新邮件、批量路径删除和新 state
放入同一个提交。发送 checkpoint，等待其后的 progress 响应，再核对实际 HEAD 等于
预期的新提交，才发布内存状态和协议成功响应；仅有 progress 不足以排除引用更新失败。

Git 的 committed 对象和引用启用 fsync。进程退出或故障后以 HEAD 为准，未引用对象
不会成为可见邮件，不需要工作目录恢复日志。发生不确定的写入失败时拒绝继续写入，
必须修复并重启；已提交但响应中断时，SMTP 重试可能产生重复邮件。

Git 命令通道串行化；读取用 `ls <commit> <path>` 找到 blob，再用 cat-blob 按长度读取
完整原文，放进 `0600` 的临时文件并立即 unlink，随后向 POP 客户端流式输出，避免慢
客户端占用 Git 通道、阻塞收信。只查大小使用独立的 `cat-file -s`，不消费整封正文。
进程空闲 30 秒后关闭 stdin，以 EOF 正常结束；下次操作重新启动。空闲回调带版本号，
避免旧计时器关闭刚被重新使用的进程。异常时先 kill 再关闭 stdin，避免 EOF 最终更新
引用时重试发布之前失败的事务。服务关闭时显式停止计时器并回收子进程。

`state` 格式版本 2 保存 `last_id`、存活消息总 octet 数，以及按 ID 升序排列的
`deleted_ranges: [{start,end,acc_count}, ...]`。区间包含两端，互不重叠，相邻区间合并；
`acc_count` 是截至当前区间累计删除的数量。已有普通文件存档会在开启删除前导入 Git，
保留原文副本；版本 1 的 state 在读取时规范化，下一次提交持久化为版本 2。
现有 ID 与 UIDL 保持不变；分配新 ID 始终使用 `last_id+1`。

认证成功取得 `{last_id,total_octets,deleted_ranges,commit}` 快照。区间数组不可变，
会话共享同一版本，登录是 O(1)，没有逐邮件编号数组。存活数是 `last_id-累计删除数`。
给定 POP3 序号 k，二分查找最后一个满足 `end-acc_count < k` 的区间，永久 ID 为
`k+acc_count`（没有区间则为 k），内存定位复杂度 O(log R)，R 为删除区间数。
完整列表顺序遍历存活 ID 并整段跳过删除区间，遍历复杂度 O(N+R)。

批量删除先按 ID 排序、去重，整理为新删除区间，再与旧区间线性合并并重算累计数，
生成新的数组；旧快照不受影响。排序成本 O(D log D)，已排序的批次跳过排序；合并与
遍历待删除 ID 的成本为 O(R+D)，D 为本次请求删除的邮件数，另有逐文件及 Git 操作。
Git tree/pack 提交成本独立于内存索引复杂度，不能据此声称整个投递或删除都是对数时间。

多个 POP3 会话可以同时认证。消息编号只在各自会话内连续、固定，UIDL 由永久 ID
生成；读取来自快照对应的 Git 提交，即使其他会话删除文件也能继续读取。旧会话
提交删除时合并到最新状态，已删除 ID 视为成功，不覆盖新收到的邮件。历史只追加，
所以旧快照引用的提交仍可从 HEAD 到达，不需要额外的会话 ref。

启动时验证 Git 树与 state 的存活数和字节数，按规范路径顺序线性遍历，成本 O(N+R)，
但不在内存保留逐邮件列表。bare 仓库中不创建邮件工作副本，临时文件不进入提交。
原有带工作目录的仓库需要管理员停服并克隆为 bare 后指定新目录；不自动转换或删除
已有 .git。notmuch 可在服务之外对检出或导出的文件树索引；备份包含完整 Git 历史。

运行时要求 Git 2.36 或更高版本，Go 代码仍只使用标准库。只执行本地 Git builtins，
禁用 hooks、fsmonitor、提交签名、换行过滤和自动维护，并隔离继承的 Git 环境变量及
系统/用户级配置。systemd unit 无需放宽权限，继续禁止出站 `connect(2)`；备份和维护
由服务之外的程序执行。仓库要求单个守护进程写入，外部修改应先停服，禁止重写历史。

## 入站 SMTP 的首版约束

- 只接收配置域名的 RCPT，不转发到任意外部地址；
- 支持 EHLO/HELO、MAIL FROM、RCPT TO、DATA、RSET、NOOP、QUIT；
- 对命令行、DATA 行、总邮件大小、收件人数、并发连接和空闲时间设硬上限；
- DATA 成功响应只在 Git checkpoint 与 HEAD 核对成功之后返回；
- 保存原始 RFC 5322 内容，并在前面添加 `Return-Path`、`Delivered-To` 和本机
  `Received` 投递头；
- JSON 结构化日志只记录 envelope、大小、远端地址和结果，不记录正文；
- 由 systemd socket activation 持有 25 和 995 端口，服务进程不使用 root。

全收地址模式会增加垃圾邮件与磁盘耗尽风险。“暂不做收信反垃圾”不应取消资源限制；
首版仍需拒绝未知域、过大邮件、过多收件人和超时连接。后续可以增加地址令牌或显式地址表，
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

## 部署时配置

1. 在 `PICOMX_DOMAINS` 中设置收信域名，例如 `example.net`。它决定 picomx
   允许哪些 `@` 后缀，不是服务器主机名。
2. 为收信域名设置 MX 记录，使它指向 SMTP 服务器主机名，例如
   `example.net MX 10 mx.example.net`；同时将 `PICOMX_HOSTNAME` 设为
   `mx.example.net`，用于 SMTP EHLO 和投递头。两个名字相关，但用途不同。
3. 设置 POP 客户端连接的主机名、共享证书目录和证书需要覆盖的名字。
   POP 主机名可以与 SMTP 主机名相同，也可以分开。
4. 默认接收配置域名下的任意地址。如果只想允许某些 `@` 前缀，或只允许
   符合自定义规则的地址，运行 `make config` 并实现本地收件人策略
   （SMTP recipient policy）。`PICOMX_DOMAINS` 仍是最外层边界：自定义规则不能接收
   配置以外的域名。

Git 归档是存储的一部分；远程备份可以在服务之外执行。
邮件静态加密不在当前设计范围内。

POP3S 与可编程策略的边界见 [policy-api.md](policy-api.md)。
