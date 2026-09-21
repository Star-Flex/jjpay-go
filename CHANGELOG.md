# 变更记录

版本采用 `MAJOR.MINOR.PATCH` 格式。v0.x 阶段可能包含不兼容变更，升级前请阅读对应版本的说明。

## v0.1.6

- 请求签名的 `PATH` 不再包含 `BaseURL` 中的路径前缀。例如，配置为 `https://pay.example.com/api` 时，请求仍发送到带 `/api` 的地址，签名使用 `/openapi/v1/...`。这使签名路径与反向代理剥除前缀后的服务端路径一致。**带路径前缀的配置与 v0.1.5 不兼容，需配套升级网关；不带前缀的配置不受影响。**
- 移除 `LimitMethod` 和 `CreateOrderReq.LimitMethods`。支付方式由管理后台的应用渠道配置控制；需要不同配置时，可使用不同应用。引用被移除字段的代码需要调整。`Channel` 和 `Method` 类型仍用于查单响应和通知事件。

## v0.1.5

- 新增 `refund.stalled` 事件（`EventRefundStalled`），以及 `Event.StalledAt`、`Event.StalledReason` 字段。该事件表示退款仍在处理中，需要人工跟进，不能据此判定失败或再次退款。旧版 SDK 可以解析通知，但需要增加事件处理分支。
- 移除 `CreateOrderReq.PayerRef` 和通知中的 `payer_ref` 字段。业务关联信息统一使用 `Attach`。引用被移除字段的代码需要调整；旧版 SDK 仍可解析含有未知字段的通知。
- `CreateOrderReq` 新增 `OriginalMinor` 和 `DiscountLabel`，用于展示原价与优惠。支付及退款仍以 `TotalMinor` 为依据。非零原价不得小于实付金额；商品明细合计必须等于原价，未设置原价时等于实付金额，否则返回 `10001`。
- 增加参数上限：单个金额不超过 1 亿元，`Qty` 不超过 10000，`Items` 不超过 100 行，`Item.Name` 不超过 64 字符。超过限制返回 `10001`。
- 明确 `Item.Qty` 省略或小于等于 0 时按 1 计算。

## v0.1.3、v0.1.4

无面向调用方的改动。

## v0.1.2

- 移除本仓库的 Dependabot 配置。依赖与工作流更新统一在上游维护后同步。

## v0.1.1

- 修复错误包装，保留底层错误，支持通过 `errors.Is` 和 `errors.As` 获取原因。
- 增加 golangci-lint、govulncheck、CodeQL、OpenSSF Scorecard，以及不低于 90% 的覆盖率检查。
- GitHub Actions 依赖固定到提交 SHA。
- 更新问题反馈说明，明确本仓库不接收 Pull Request。

## v0.1.0

首个公开版本。

### 接口

- 创建、查询和关闭支付单，支持按网关单号或商户单号查询。
- 发起和查询退款，支持按网关退款单号或商户退款单号查询。
- 异步通知验签：`(*Client).Verify`、`(*Client).Middleware`、`EventFrom`。
- 回跳结果验签：`(*Client).VerifyReturn`。
- `SignNotify`：生成通知签名头，用于测试通知处理逻辑。

### 配置

- 支持显式 `Config` 和环境变量，显式配置优先；可使用 `NewFromEnv()` 初始化。
- 网关地址由接入方配置，不提供默认域名。
- 仅依赖 Go 标准库。
- 可选 `Logger`，接口为 `Printf(string, ...any)`，在重试前记录日志。

### 请求与响应处理

- 核对响应单号与请求单号，不一致时返回 `ErrBadResponse`。
- 禁止自动跟随 HTTP 重定向，避免签名头被转发到其他地址。
- 成功响应缺少有效 `data` 时返回错误。
- 读操作对传输错误、单次超时、HTTP 5xx / 429 和业务码 `10002` / `20006` 进行有限重试；写操作不自动重试。
- 限制错误文本长度并清理控制字符；未签名的鉴权错误使用本地文案。
