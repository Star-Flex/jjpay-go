<p align="center">
  <img src="logo.svg" width="96" height="96" alt="">
</p>

<h1 align="center">jjpay Go SDK</h1>

<p align="center">
  <a href="https://github.com/Star-Flex/jjpay-go/actions/workflows/test.yml"><img src="https://github.com/Star-Flex/jjpay-go/actions/workflows/test.yml/badge.svg" alt="test"></a>
  <a href="https://github.com/Star-Flex/jjpay-go/actions/workflows/lint.yml"><img src="https://github.com/Star-Flex/jjpay-go/actions/workflows/lint.yml/badge.svg" alt="lint"></a>
  <a href="https://github.com/Star-Flex/jjpay-go/actions/workflows/codeql.yml"><img src="https://github.com/Star-Flex/jjpay-go/actions/workflows/codeql.yml/badge.svg" alt="codeql"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/Star-Flex/jjpay-go"><img src="https://api.scorecard.dev/projects/github.com/Star-Flex/jjpay-go/badge" alt="OpenSSF Scorecard"></a>
  <a href="https://pkg.go.dev/github.com/Star-Flex/jjpay-go"><img src="https://pkg.go.dev/badge/github.com/Star-Flex/jjpay-go.svg" alt="Go Reference"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/go-1.22%2B-00ADD8" alt="Go 1.22+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License"></a>
</p>

jjpay 支付网关的 Go SDK，支持下单、查单、关单、退款，以及支付通知和回跳结果的验签。SDK 仅依赖 Go 标准库，可与现有 Web 框架配合使用。

本文介绍 API 用法。为已有支付系统接入 jjpay，请同时阅读[接入手册](INTEGRATION-PLAYBOOK.md)，了解订单迁移、金额换算、通知处理和上线检查。

本仓库由上游同步维护。问题反馈与贡献方式见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 安装

需要 Go 1.22 或更高版本。

```bash
go get github.com/Star-Flex/jjpay-go
```

```go
import "github.com/Star-Flex/jjpay-go" // 包名为 jjpay
```

## 使用 AI 辅助接入

可以让能够读取项目代码的 AI 编程助手协助接入。将下面这段提示词复制到你的项目会话中，助手即可根据 [README](https://github.com/Star-Flex/jjpay-go/blob/main/README.md) 和[接入指南](https://github.com/Star-Flex/jjpay-go/blob/main/INTEGRATION-PLAYBOOK.md) 分析现有代码并完成接入。

```text
请帮我在当前项目中接入 jjpay Go SDK。先完整阅读以下文档，并检查项目现有的订单、支付、退款和配置实现：

API 用法：https://github.com/Star-Flex/jjpay-go/blob/main/README.md
接入指南：https://github.com/Star-Flex/jjpay-go/blob/main/INTEGRATION-PLAYBOOK.md

根据现有代码说明接入方案；新增还是替换支付渠道、金额换算、退款记账等无法从项目中确定的事项，先向我确认。随后按项目约定完成实现和测试，重点检查通知验签、订单与金额核对、幂等处理，以及历史订单的原路退款。

凭据从环境变量或项目现有的密钥管理方式读取，不要把真实密钥写入代码、日志或对话。完成后说明修改内容、测试结果，以及还需要我配置或联调的事项。
```

接入完成后，仍需按指南进行联调和上线检查。文档链接指向当前说明；使用已安装的旧版 SDK 时，请同时核对[版本变更记录](CHANGELOG.md)。

## 快速接入

以下示例展示主要调用方式，业务处理函数需由接入方实现。

### 1. 初始化客户端

在进程启动时创建客户端并复用。`Client` 可供多个 goroutine 并发调用。

```go
c, err := jjpay.NewFromEnv()
if err != nil {
    log.Fatal(err) // 配置不完整或格式有误时停止启动
}
```

| 环境变量 | 说明 |
|---|---|
| `JJPAY_BASE_URL` | 网关地址，例如 `https://pay.example.com/api`。请保留管理员提供的路径前缀；该前缀不参与签名 |
| `JJPAY_APP_ID` | 在管理后台创建应用时生成的应用标识 |
| `JJPAY_APP_SECRET` | 应用密钥，仅在创建或重置时显示。通过环境变量或密钥管理服务注入，不要写入源码或日志 |

也可以通过 `Config` 传入配置。`BaseURL`、`AppID`、`Secret` 未设置时，分别读取对应的环境变量：

```go
c, err := jjpay.New(jjpay.Config{
    BaseURL: "https://pay.example.com/api",
    AppID:   "your_app_id",
    Secret:  os.Getenv("JJPAY_APP_SECRET"),
})
```

SDK 不提供默认网关地址。测试和生产环境应分别配置地址与凭据。

其他可选配置：

| 配置项 | 说明 |
|---|---|
| `HTTPClient` | 自定义 HTTP 客户端，用于设置连接池、代理等 |
| `Timeout` | 单次请求超时，默认 15 秒 |
| `SkewWindow` | 验签允许的时间偏差，默认 ±5 分钟 |
| `ReadRetries` | 读操作的重试次数，默认 2 次；负数表示关闭 |
| `Logger` | 重试日志，需实现 `Printf(string, ...any)`；可直接使用 `*log.Logger` |

配置 `Logger` 后，SDK 会在每次重试前记录日志。未配置时，SDK 不输出日志。

### 2. 创建订单

```go
resp, err := c.CreateOrder(ctx, jjpay.CreateOrderReq{
    OutTradeNo: "example-order-001", // 商户订单号，在当前应用内唯一
    Subject:    "基础版服务（月付）",
    TotalMinor: 1990,                // 应付金额：1990 分，即 19.90 元
    Attach:     "plan_id=7",         // 业务标记，随异步通知原样返回
    NotifyURL:  "https://your-app.example/pay/notify",
    ReturnURL:  "https://your-app.example/orders/123",
})
if err != nil {
    return err
}
// 将用户重定向至 resp.CheckoutURL，或在页面中提供支付链接。
```

如需显示商品明细和优惠，可增加以下字段：

```go
jjpay.CreateOrderReq{
    TotalMinor:    1490,
    OriginalMinor: 1990,      // 原价；优惠金额由服务端计算
    DiscountLabel: "年付立减", // 留空时显示“优惠”
    Items: []jjpay.Item{
        {Name: "基础版托管", UnitMinor: 1590, Qty: 1},
        {Name: "快照备份", UnitMinor: 200, Qty: 2},
    },
    // 其余字段同上。
}
```

传入 `Items` 时，`Σ(UnitMinor × Qty)` 必须等于 `OriginalMinor`；未设置原价时，必须等于 `TotalMinor`。`Qty` 省略或小于等于 0 时按 1 计算。金额不一致返回 `10001`。

`OriginalMinor` 为 0 或等于 `TotalMinor` 时表示无优惠；其他非零值必须大于 `TotalMinor`。明细与优惠字段用于收银台展示，支付和退款均以 `TotalMinor` 为金额依据。

### 3. 接收异步通知

使用客户端的验签中间件接收通知，并配置 `OnError` 记录验签失败的原因。中间件会限制请求体大小、验证签名和时间戳，再将解析后的事件放入请求上下文。

```go
mux := http.NewServeMux()
notify := c.MiddlewareWithOptions(jjpay.NotifyOptions{
    OnError: func(r *http.Request, err error) {
        log.Printf("jjpay 通知验签失败 path=%s err=%v", r.URL.Path, err)
    },
})

mux.Handle("/pay/notify", notify(http.HandlerFunc(
    func(w http.ResponseWriter, r *http.Request) {
        evt := jjpay.EventFrom(r.Context())

        // handleEvent 由接入方实现：核对订单、金额和支付来源，
        // 按事件类型处理业务，并保证重复通知不会重复执行。
        if err := handleEvent(r.Context(), evt); err != nil {
            http.Error(w, "notification processing failed", http.StatusInternalServerError)
            return
        }
        io.WriteString(w, "SUCCESS")
    },
)))
```

`handleEvent` 应处理 `pay.succeeded`、`order.closed`、`refund.succeeded`、`refund.failed` 和 `refund.stalled`。业务处理成功或确认已处理过该事件后，再返回成功应答；处理失败时不要返回 `SUCCESS`。

成功应答为 HTTP 200，正文为 `SUCCESS`（大小写不敏感，允许前后空白），也可返回 JSON `{"code":"SUCCESS"}`。其他应答视为投递失败。网关按 `0/15/60/300/900/1800/3600/10800/21600/43200` 秒的间隔安排投递，共 10 次，约 24 小时；仍失败的通知进入死信队列，需由管理员处理。

### 4. 展示回跳结果（可选）

收银台跳转到 `ReturnURL` 时，会附带签名后的订单结果。服务端验证通过后，可用它展示支付状态：

```go
res, err := c.VerifyReturn(r.URL.Query())
if err != nil {
    // 验签失败时 res 为 nil。转到订单页，由服务端查询当前用户的订单。
    http.Redirect(w, r, "/orders?checking=1", http.StatusFound)
    return
}
if res.Paid() {
    // 使用 res.TradeNo、res.TotalMinor、res.PaidAt 等字段展示结果。
}
```

回跳结果仅用于展示，发货或开通服务应以异步通知或服务端查单结果为依据。用户关闭浏览器后可能不会触发回跳，旧链接也可能被再次访问。SDK 默认校验 ±5 分钟的时间窗口；需要限制链接只能使用一次时，由接入方记录并校验 `res.Nonce`。

## 与 Web 框架集成

SDK 不依赖特定框架。Gin、Echo、Chi、Fiber 等框架均可将请求头和原始请求体传给 `(*Client).Verify`。以下是 Gin 示例：

```go
func GinVerify(pay *jjpay.Client) gin.HandlerFunc {
    return func(c *gin.Context) {
        body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10))
        if err != nil {
            log.Printf("jjpay 通知读取失败: %v", err)
            c.AbortWithStatus(http.StatusUnauthorized)
            return
        }
        evt, err := pay.Verify(c.Request.Header, body)
        if err != nil {
            log.Printf("jjpay 通知验签失败: %v", err)
            c.AbortWithStatus(http.StatusUnauthorized)
            return
        }
        c.Request = c.Request.WithContext(jjpay.WithEvent(c.Request.Context(), evt))
        c.Next()
    }
}

r.POST("/pay/notify", GinVerify(pay), func(c *gin.Context) {
    evt := jjpay.EventFrom(c.Request.Context())
    if err := handleEvent(c.Request.Context(), evt); err != nil {
        c.Status(http.StatusInternalServerError)
        return
    }
    c.String(http.StatusOK, "SUCCESS")
})
```

优先使用 `Client` 的方法，复用初始化时已校验的密钥。包级函数 `Middleware(secret)` 和 `Verify(header, body, secret)` 适用于不持有客户端的场景，需要单独传入密钥；`Middleware("")` 会 panic。

## 业务处理要求

### 验签与订单核对

只有验签通过后，才能使用通知中的业务字段。时间戳校验用于限制旧通知的重放，请保持服务器时间同步，不要通过扩大时间窗口解决时钟偏差。

处理支付成功事件前，还需核对本地订单是否存在、所属应用和支付来源是否匹配，以及金额和币种是否一致。签名有效不代表通知可以用于任意本地订单。

### 幂等与事务

同一事件可能被重复投递。建议使用以下去重键；多个应用共用一张表时，键中还应包含应用标识。

| 事件 | 去重键 |
|---|---|
| `pay.succeeded` / `order.closed` | `trade_no` + `event` |
| `refund.succeeded` / `refund.failed` / `refund.stalled` | `refund_no` + `event` |

同一支付单可以发生多笔退款，因此退款事件不能仅按支付单号去重。

去重记录与业务更新应在同一个数据库事务内完成，并由唯一约束或带状态条件的更新保证并发安全。以下为事务处理示意，SQL 占位符需按所用驱动调整，去重字段需建立唯一约束：

```go
func handle(evt *jjpay.Event) error {
    key := evt.TradeNo
    if evt.RefundNo != "" {
        key = evt.RefundNo
    }

    tx, err := db.Begin()
    if err != nil {
        return err
    }
    defer tx.Rollback()

    res, err := tx.Exec(`INSERT INTO pay_event_seen(biz_no, event) VALUES($1, $2)
                         ON CONFLICT DO NOTHING`, key, evt.Event)
    if err != nil {
        return err
    }
    if n, err := res.RowsAffected(); err != nil {
        return err
    } else if n == 0 {
        return nil // 已处理过该事件
    }

    // deliver 由接入方实现，订单核对与业务更新均使用当前事务。
    if err := deliver(tx, evt); err != nil {
        return err
    }
    return tx.Commit()
}
```

应先检查数据库错误，再判断受影响行数。数据库故障不能视为重复通知。业务处理失败时回滚事务，使后续重试仍可继续处理。

需要调用外部系统时，在事务内写入待执行任务，再由任务队列或 outbox 执行，避免将网络调用放入数据库事务。

### 响应单号校验

下单、查单、关单和退款相关方法会核对响应中的单号；单号与本次请求不一致时返回 `ErrBadResponse`。响应签名用于验证内容来源和完整性，单号校验用于确认响应与请求的对应关系。

## API 参考

| 方法 | 接口或用途 | 自动重试 |
|---|---|---|
| `CreateOrder(ctx, CreateOrderReq) (*CreateOrderResp, error)` | `POST /openapi/v1/orders` | 否 |
| `QueryOrder(ctx, tradeNo) (*Order, error)` | `GET /openapi/v1/orders/{trade_no}` | 是 |
| `QueryOrderByOutTradeNo(ctx, outTradeNo) (*Order, error)` | `GET /openapi/v1/orders?out_trade_no=` | 是 |
| `CloseOrder(ctx, tradeNo) (*CloseOrderResp, error)` | `POST /openapi/v1/orders/{trade_no}/close` | 否 |
| `Refund(ctx, RefundReq) (*Refund, error)` | `POST /openapi/v1/refunds` | 否 |
| `QueryRefund(ctx, refundNo) (*Refund, error)` | `GET /openapi/v1/refunds/{refund_no}` | 是 |
| `QueryRefundByOutRefundNo(ctx, outRefundNo) (*Refund, error)` | `GET /openapi/v1/refunds?out_refund_no=` | 是 |
| `(*Client).Verify(header, body) (*Event, error)` | 通知验签 | — |
| `(*Client).Middleware() func(http.Handler) http.Handler` | 通知验签中间件 | — |
| `(*Client).MiddlewareWithOptions(NotifyOptions) func(http.Handler) http.Handler` | 带错误回调等选项的验签中间件 | — |
| `(*Client).VerifyReturn(query) (*ReturnResult, error)` | 回跳结果验签 | — |
| `SignNotify(secret, event, body, at) (http.Header, error)` | 生成通知签名头，用于接入方的回调测试 | — |

签名计算等底层函数不对外导出。测试通知处理逻辑时，可用 `SignNotify` 生成合法签名，并将同一份请求体发送给处理函数。

### 枚举与金额

- 渠道：`ChannelAlipay`、`ChannelWechat`、`ChannelMock`。
- 支付方式：`MethodAlipayPage`、`MethodAlipayWap`、`MethodWechatNative`、`MethodWechatH5`、`MethodWechatJSAPI`。
- 订单状态：`OrderPending`、`OrderPaying`、`OrderPaid`、`OrderClosed`。
- 退款状态为整数枚举：`RefundProcessing = 1`、`RefundSucceeded = 2`、`RefundFailed = 3`。

金额字段使用 `int64`，以币种最小单位表示，字段名带 `Minor` 后缀。`Currency` 留空时为 CNY，单位为分。金额换算应使用整数或精确十进制计算，避免浮点误差；显示人民币金额时再将分转换为元。

### 重试与幂等

写操作（下单、退款、关单）不自动重试。超时或网络错误发生时，请求可能已经成功，应先查单确认，或使用相同单号和参数重试。不要为一次结果未明的退款生成新的退款单号。

读操作默认最多重试 2 次，退避时间为 200ms、400ms。可重试的错误包括传输错误、单次请求超时、HTTP 5xx / 429，以及业务码 `10002`（系统繁忙）和 `20006`（限流）。其他业务错误、验签失败和调用上下文取消不重试。

| 接口 | 幂等键 | 参数一致时 | 冲突或状态不允许时 |
|---|---|---|---|
| `CreateOrder` | `(app_id, out_trade_no)` | 返回已有订单和同一个 `CheckoutURL` | `ErrOrderConflict`，不修改已有订单 |
| `Refund` | `(app_id, out_refund_no)` | 返回同一退款单 | `ErrRefundNoConflict` |
| `CloseOrder` | 支付单号 | 已关闭的订单再次关闭仍返回成功 | 已支付的订单返回 `ErrOrderStatusDenied` |

下单参数一致性按 `TotalMinor`、`Subject`、`NotifyURL` 判断。重复下单可能返回已关闭的订单，请检查 `resp.Status`，不要继续展示已失效的支付链接。

### 退款状态与事件

退款接口调用成功表示已获得有效响应，退款结果仍需根据 `Status` 判断：

| 状态 | 含义 | 处理方式 |
|---|---|---|
| `RefundSucceeded` | 退款成功 | 更新本地退款结果 |
| `RefundProcessing` | 处理中 | 等待异步通知，或通过查退款接口确认 |
| `RefundFailed` | 退款失败 | 记录原因并按业务规则处理 |

同步响应已返回成功时，网关仍会发送终态通知。同步结果和异步通知应按同一退款单幂等处理。

`refund.stalled` 表示退款仍在处理中，需要人工跟进，不能据此判定退款失败。记录 `RefundNo` 和 `StalledReason` 并通知负责人员；在最终结果确认前，不要重复退款或再次补偿。

### 错误处理

业务错误类型为 `*APIError`。使用 `errors.Is` 与预定义错误值判断错误类型，避免依赖错误文案：

```go
resp, err := c.CreateOrder(ctx, req)
switch {
case err == nil:
    // 使用 resp 继续处理。
case errors.Is(err, jjpay.ErrOrderConflict):
    // 核对同一订单号的金额、标题和通知地址是否发生变化。
case errors.Is(err, jjpay.ErrAppDisabled):
    // 应用已停用，联系网关管理员。
default:
    // 按错误类型处理；网络错误或超时时先确认订单状态。
}
```

完整错误列表见 [errors.go](errors.go)，`CodeOf(err)` 可读取业务错误码。SDK 未登记的业务错误码仍返回 `*APIError`。

其他常见错误：

| 错误 | 含义 |
|---|---|
| `*HTTPError` | HTTP 状态码不是 200，例如代理返回 502 或路径不存在 |
| `ErrResponseSign` | 响应验签失败，无法确认响应内容；应通过后续查单确认业务结果 |
| `ErrTimestampWindow` | 验签时间戳超出允许窗口，应检查时间同步与请求延迟 |
| `ErrBadResponse` | 响应缺少有效数据、无法解析，或响应单号与请求不一致等 |

SDK 校验响应的 `X-Jjpay-Timestamp`、`X-Jjpay-Nonce` 和 `X-Jjpay-Signature`。服务端鉴权失败的响应可能不带签名，SDK 仅对鉴权错误码段（20001–20099）保留该例外，以便返回 `ErrSignMismatch`、`ErrTimestampSkew` 等错误。其他未签名响应均按验签失败处理。

## 开发与反馈

```bash
go test ./...
```

签名测试使用固定输入和期望值，并与服务端共享部分测试向量，用于检查协议兼容性。

提交问题前，请移除密钥、真实订单号、支付链接和客户信息。安全问题请按 [SECURITY.md](SECURITY.md) 私密报告。
