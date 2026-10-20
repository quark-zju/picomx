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
                         +--> picomx --> Git archive --> optional Git backup
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
1/001-s123.eml
1/3ff-s456.eml
2/001/000-s789.eml
```

邮件先以 `0600` 权限写入 `tmp/` 并执行 fsync。收信和批量删除均由同一把锁串行处理，
通过常驻 `git fast-import --quiet` 写入 blob、tree、commit，无需 index 或工作目录。
每个 commit 显式以前一个已确认的 HEAD 为父提交，将新邮件、批量路径删除和新 state
放入同一个提交。发送 checkpoint，等待其后的 progress 响应，再核对实际 HEAD 等于
预期的新提交，才发布内存状态和协议成功响应；仅有 progress 不足以排除引用更新失败。

Git 的 committed 对象和引用启用 fsync。进程退出或故障后以 HEAD 为准，未引用对象
不会成为可见邮件，不需要工作目录恢复日志。发生不确定的写入失败时拒绝继续写入，
必须修复并重启；已提交但响应中断时，SMTP 重试可能产生重复邮件。

读取复用另一个常驻 `git cat-file --batch-command` 进程。`info` 获取 tree OID，
`contents` 读取其原始二进制 tree；解析 mode、文件名、原始 object ID，支持 SHA-1
和 SHA-256。`fast-import cat-blob` 只支持 blob，传入 tree 会报错并退出，不用于列目录。
正文同样通过 reader 按长度直接从管道向 POP 客户端输出，不创建正文临时文件。读取
期间持有 reader 专用锁，直到关闭 reader；writer 使用独立锁，SMTP 提交可继续进行。
提前关闭时丢弃剩余正文并消费分隔符，再释放 reader。删除在状态锁之外解析路径，
取得状态锁后若 HEAD 已更新则重试，避免等候 reader 时阻塞新投递。
完整 `LIST`、`UIDL` 逐叶目录枚举，大小来自文件名，不读取正文、不逐封启动进程。

共享目录 LRU 最多保存 64 个叶目录，每个至多 1024 封邮件的 ID、大小、路径。缓存键为
目录路径和 tree OID：快照固定提交，旧会话不会看到新内容；未变的目录跨提交复用。
命中时只需 `info` 查询 OID，不再读取和解析整个 tree；未命中用同一 reader 读取 tree。
列出大邮箱时只保留有界缓存和当前目录的列表，不建立全部邮件的数组。

子进程空闲 30 秒后关闭 stdin，以 EOF 正常结束；下次操作按需重新启动。空闲回调带
版本号，避免旧计时器关闭刚被重新使用的进程。写入异常时先 kill importer 再关闭 stdin，
避免 EOF 最终更新引用时重试发布之前失败的事务。reader 协议错误时回收 reader；下次
读取可以重新启动。服务关闭时显式停止计时器并回收子进程。

Git 协议操作和单次管道读/排空设置 30 秒 watchdog；超时终止整个 Git 进程组，
避免子进程持有管道或继续写入。reader 可重新创建，写入不确定时维持 fail-closed。
网络写入不占用 Git IO watchdog，由 POP3 写超时约束。

默认每 6 小时或累计 256 次提交后请求维护，周期可通过 env/flag 调整。维护以
TryLock 取得 reader、state、writer 锁；忙时一分钟后重试，不拿着 writer 锁等待正文。
取得锁后关闭常驻子进程，执行前台 `git -c gc.autoDetach=false gc --prune=never`；
非 auto GC 不受 `gc.auto=0` 限制。最长 5 分钟，取消/超时回收整个进程组，失败记录日志。
相同 HEAD 已维护过时跳过；不重写 commit、不删除可达历史，读写随后按需重建子进程。
GC 需要额外空间，不能代替入站预算或磁盘监控；外部维护仍需先停服。

每日收信预算默认 50 MiB、UTC 自然日，env/flag 可调。以归档字节数计费，单封至少
4 KiB，限制小邮件的提交/对象数量。暂存阶段在内存预留预算，所有连接共享；
`{day, charged_bytes}` 单独保存在 `<archive-root>.rate-limit.json`，不进入 Git。
文件权限 0600，写入临时文件并 fsync、原子 rename、fsync 父目录；先扣额度，再提交
Git 邮件。不引入跨文件/Git 的事务日志，异常或 Git 失败时允许多扣一次，保证不会
漏计已发布邮件。计数文件更新失败时停止发布，重启后重新读取；损坏文件拒绝启动。
未提交预留在暂存失败/策略拒绝时释放，已持久化扣额不因删除/重启/HEAD 回退返还，
时钟回拨不重置已记录日期。读取兼容旧 state 的 daily_receipt，仅在外部文件缺失时
转存一次；正常写入不再携带该字段。DATA 前预检查，
实际流式暂存继续检查，超额 SMTP 返回 452；不能仅信任发件人声明的 SIZE。

`state` 格式版本 2 保存 `last_id`、存活消息总 octet 数，以及
`directories: {"1": 10, "2": 20, "2/001": 20, ...}`，记录每个非空目录下的存活总数，
包含子目录后代；删除最后一封邮件时移除相应空目录计数，不保存逐邮件删除区间。
文件名 `xxx-sSIZE.eml` 中的 SIZE 是原文不可变的字节数。新 ID 始终使用 `last_id+1`。

运行时从目录计数构建按路径排序的叶目录累计计数索引。更新时复制目录 metadata 并
重建索引；认证快照共享不可变索引和提交 ID，登录 O(1)。给定 POP3 序号 k，二分找到
所属叶目录 O(log L)，再在至多 1024 项的有界列表中定位，L 为非空叶目录数。完整列表
枚举 O(N+L)，首次需读取目录对象；不在登录时做 O(N) 的重新编号。

批量删除先按 ID 排序、去重，在最新快照中查找路径和文件名大小；已删除 ID 不重复
扣减，存活 ID 从 tree 中删除，并更新其祖先目录计数和总字节数。一次事务仅复制、
重建一次索引；排序 O(D log D)，metadata 重建 O(L log L)，另有逐邮件查找及 Git 成本。
旧快照引用原索引，后续更新不修改旧版本。Git tree/pack 提交成本独立于索引复杂度。

多个 POP3 会话可以同时认证。消息编号只在各自会话内连续、固定，UIDL 由永久 ID
生成；读取来自快照对应的 Git 提交，即使其他会话删除文件也能继续读取。旧会话
提交删除时合并到最新状态，已删除 ID 视为成功，不覆盖新收到的邮件。历史只追加，
所以旧快照引用的提交仍可从 HEAD 到达，不需要额外的会话 ref。删除提交记录已认证
会话用户名，包含控制字符时进行转义，避免伪造说明行；口令不进入提交。POP3 当前
没有客户端 agent 元数据，邮件自身的 User-Agent/X-Mailer 不用于标识删除者。

启动直接读取 HEAD 的 state 并构建目录索引，成本 O(L log L)，不再扫描 O(N) 封邮件
或逐 blob 查询大小。假定仓库仅由 picomx 一致写入；在读取目录/正文时保留协议 framing、
规范路径、计数和大小检查。若缺失 HEAD/ref 且有 Git 对象、引用或日志，拒绝自动重导
legacy，避免复活旧邮件和回退 last_id；正常启动不扫描对象数据库。
原始非 Git、版本 1 文件存档首次启动时导入新文件名格式，保留原文副本；后续以 Git
中的 state 为准。未部署的中间 Git 格式不提供迁移；已有 `.git/` 工作目录会被拒绝。
备份包含完整 Git 历史，邮件访问通过 POP3 / Git 完成。

运行时要求 Git 2.36 或更高版本，Go 代码仍只使用标准库。只执行本地 Git builtins，
禁用 hooks、fsmonitor、提交签名、换行过滤和隐式自动维护，并隔离继承的 Git 环境变量及
系统/用户级配置。systemd unit 无需放宽权限，继续禁止出站 `connect(2)`；远程备份
由服务之外的程序执行，周期压缩在服务内协调。仓库要求单个守护进程写入，外部修改应先停服，禁止重写历史。

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
