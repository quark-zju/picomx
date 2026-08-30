# picomail

面向个人域名的轻量 self-host 邮件服务（早期原型）。

项目目标不是重新实现一套完整的群件系统，而是提供几个边界清晰的小工具：

- 用自己的域名收信，允许为每个网站使用不同地址；
- 将原始邮件存入 append-only 文件树，可用 Git 同步、用 notmuch 索引；
- 以尽量少的依赖和协议状态降低攻击面；
- 最终支持经过 SPF、DKIM、DMARC 对齐的低流量出站邮件。

当前仓库尚处于第一个可运行阶段。已经确定的范围和仍需选择的事项见
[docs/design.md](docs/design.md)。

## 开发

需要 Go 1.24 或更高版本。

```sh
make test
make build
```
