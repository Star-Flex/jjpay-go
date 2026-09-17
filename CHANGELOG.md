# 变更记录

本包遵循 [语义化版本](https://semver.org/lang/zh-CN/)。**v0.x 不承诺向后兼容**——
在第一个接入方真正上线之前，契约还允许调整；到 v1.0.0 才冻结。

## v0.1.3

- README 加上 logo 与居中的标题、徽章

## v0.1.2

- 移除本仓库里的 `dependabot.yml`。本仓库的 PR 是关闭的，Dependabot 在这里开不出
  PR；而且这里的工作流是从上游复制过来的，下一次同步会整棵树覆盖，在这里升级没有
  意义。依赖更新发生在上游。

## v0.1.1

- 错误链修好了：`ErrBadResponse` 这一类错误此前只把哨兵包进 `%w`，底层的
  `json.SyntaxError` 之类用的是 `%v`，链断在中间。现在两段都是 `%w`，
  `errors.Is` / `errors.As` 能一路取到真正的原因
- CI 补齐：golangci-lint、govulncheck（每周一次，扫的是标准库的 CVE）、
  CodeQL、OpenSSF Scorecard、覆盖率不低于 90% 的门禁
- 所有 GitHub Action 钉在 commit SHA 上，不用可变 tag
- 文档：PR 已在仓库设置里关闭，措辞改对

## v0.1.0

首个公开版本。

**接口**

- 下单、查单（按 `trade_no` / 按 `out_trade_no`）、关单
- 退款、查退款（按 `refund_no` / 按 `out_refund_no`）
- 异步通知验签：`(*Client).Verify` / `(*Client).Middleware` / `EventFrom`
- 回跳结果验签：`(*Client).VerifyReturn`
- `SignNotify`：造一条通知的签名头，给你写自己的回调测试用

**配置**

- 网关地址与凭据按「显式 `Config` 字段 > 环境变量」两层取值，`NewFromEnv()`
- **不带内置默认域名**：地址属于部署不属于代码
- 只依赖标准库（`deps_test.go` 读 `go.mod` 把这条钉成门禁）
- 可选 `Logger`（`Printf(string, ...any)`，`*log.Logger` 直接满足）：
  只在重试前打一行。不传则完全静默，包里没有全局 logger

**默认就替你挡住的几件事**

- **回执必须是你问的那一笔**：六个接口都核对回执里的单号，对不上返回
  `ErrBadResponse`。应答签名不含 method/path，这一层不做就得每个接入方各写一遍
- **不跟重定向**：签名头会跟着 302 原样转发到第三方主机
- **成功就必须有 data**：`data` 缺失或 `null` 返回错误，不拿零值冒充答案
- **读操作会重试**：传输层失败、单次超时、5xx/429，以及业务码 10002 / 20006
  （jjpay 的业务响应 HTTP 恒为 200，只认 429 会漏掉服务端自己的限流）。
  写操作**永不自动重试**
- **错误文本是有界的一行**：对端文案进日志前压掉控制字符、截断；未签名的鉴权
  响应用本地文案，不回显对端字符串
