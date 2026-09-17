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

jjpay 支付网关的 Go 客户端。只做四件事：签名、验签、超时与有限重试、请求响应结构体。
**只依赖标准库**——不引 gin、不引 gorm、不引任何日志库。

下面这份 README 就是接入所需的全部。

> 本仓库由上游自动同步，PR 已关闭；issue 照常，见 [CONTRIBUTING](CONTRIBUTING.md)。

```bash
go get github.com/Star-Flex/jjpay-go
```

```go
import "github.com/Star-Flex/jjpay-go"   // 包名是 jjpay
```

---

## 一、三步接入

### 第 1 步 · 建客户端（进程启动时建一个，复用）

```go
c, err := jjpay.NewFromEnv()
if err != nil {
    log.Fatal(err) // 配置不合法当场炸，别留到第一次收款
}
```

读三个环境变量：

| | |
|---|---|
| `JJPAY_BASE_URL` | jjpay 的根地址，如 `https://pay.example.com` |
| `JJPAY_APP_ID` | 后台创建接入方时生成，不是秘密 |
| `JJPAY_APP_SECRET` | 只显示一次，丢了只能重置 |

要在代码里传也行，**显式的赢**：

```go
c, err := jjpay.New(jjpay.Config{
    BaseURL: "https://pay.example.com", // 留空则回退到 JJPAY_BASE_URL
    AppID:   "app_k7m2qx9b4t",
    Secret:  os.Getenv("MY_OWN_SECRET_NAME"),
})
```

本包不带内置默认域名。地址属于部署，不属于代码——换域名、在测试和生产之间切，
都只该改一个环境变量，而不是升级依赖重新编译。

`Client` 并发安全。可选项：`HTTPClient`（自带连接池 / 代理）、`Timeout`（默认 15s）、
`SkewWindow`（默认 ±5 分钟）、`ReadRetries`（默认 2，负数关闭）、`Logger`。

`Logger` 只有一个方法 `Printf(string, ...any)`，`*log.Logger` 直接满足。传了之后
本包会在**每次重试前**打一行——那是唯一会被静默吞掉的事件，一次成功的调用背后
可能藏着两次失败。不传就什么都不打；本包没有包级 logger，也不会写 stdout / stderr。

### 第 2 步 · 下单，把 `CheckoutURL` 给用户

```go
resp, err := c.CreateOrder(ctx, jjpay.CreateOrderReq{
    OutTradeNo: "A2026080200123",           // 你自己的单号，商户内唯一
    Subject:    "基础版 包月",
    TotalMinor: 1990,                       // 分。jjpay 收到的就是最终应付额
    Attach:     "plan_id=7",                // 原样回传给你
    NotifyURL:  "https://your-app.example/pay/notify",
    ReturnURL:  "https://your-app.example/orders/123",
})
if err != nil { return err }
// 302 到 resp.CheckoutURL，或把它渲染成按钮
```

### 第 3 步 · 收异步通知（这一步才是"钱到了"的真相）

```go
mux := http.NewServeMux()

// 【把 OnError 接上】默认是 nil：验签失败只回一个 401，你这边什么都看不到。
// 而"商户端点一直 401"在 jjpay 那侧的表现是通知重试 10 次后进死信——
// 等你发现时钱已经收了一天了。
notify := c.MiddlewareWithOptions(jjpay.NotifyOptions{
    OnError: func(r *http.Request, err error) {
        log.Printf("jjpay 通知验签失败 path=%s ip=%s err=%v", r.URL.Path, r.RemoteAddr, err)
    },
})

mux.Handle("/pay/notify", notify(http.HandlerFunc(   // c 就是第 1 步那只 Client
    func(w http.ResponseWriter, r *http.Request) {
        evt := jjpay.EventFrom(r.Context()) // 已验签、已解析

        switch evt.Event {
        case jjpay.EventPaySucceeded:
            // 幂等地开通服务（见下文第三节）
        case jjpay.EventRefundSucceeded:
        case jjpay.EventRefundFailed:
        case jjpay.EventOrderClosed:
        }

        io.WriteString(w, "SUCCESS") // 必须回 200 + SUCCESS，否则 jjpay 会重试 10 次
    },
)))
```

**应答规范**：HTTP 200 且 body 为 `SUCCESS`（大小写不敏感、允许前后空白），
或 JSON `{"code":"SUCCESS"}`。其余一律视为失败并按阶梯重试
（0/15/60/300/900/1800/3600/10800/21600/43200 秒，共 10 次约 24 小时，之后进死信）。

---

### 第 3½ 步 · 回跳页（可选）：签名过的结果直接画，不必查单

用户付完从收银台跳回你的 `return_url` 时，地址上带着 jjpay 签名过的结果：

```go
res, err := c.VerifyReturn(r.URL.Query())
if err != nil {
    // 不是收银台送回来的、被改过、或链接太旧。
    // 【必须 return】err != nil 时 res 是 nil，往下走就是空指针。
    // 当作"没带结果"处理：拿 trade_no 去查单。
    http.Redirect(w, r, "/orders?checking=1", http.StatusFound)
    return
}
if res.Paid() {
    // 画「支付成功 ¥19.90」；res.TradeNo / OutTradeNo / TotalMinor / PaidAt
}
```

**只准展示，不准发货**。回跳和通知是两条独立链路——用户付完关掉浏览器就没有回跳；
反过来，旧链接被收藏 / 转发也不该再当凭证（SDK 默认只认 ±5 分钟内的，要严格一次性自己记 `res.Nonce`）。
开通服务永远在第 3 步里做。

## 二、gin 的两行胶水

主 SDK 刻意**不提供** gin 中间件——那会把 gin 拖进每个接入方的依赖树。gin 用户自己包一下：

```go
// pay 是第 1 步建好的 *jjpay.Client
func GinVerify(pay *jjpay.Client) gin.HandlerFunc {
    return func(c *gin.Context) {
        body, err := io.ReadAll(io.LimitReader(c.Request.Body, 256<<10))
        if err != nil {
            c.AbortWithStatus(http.StatusUnauthorized)
            return
        }
        evt, err := pay.Verify(c.Request.Header, body)
        if err != nil {
            log.Printf("jjpay 通知验签失败: %v", err) // 一定要打，否则线上 401 毫无线索
            c.AbortWithStatus(http.StatusUnauthorized)
            return
        }
        c.Request = c.Request.WithContext(jjpay.WithEvent(c.Request.Context(), evt))
        c.Next()
    }
}

r.POST("/pay/notify", GinVerify(pay), func(c *gin.Context) {
    evt := jjpay.EventFrom(c.Request.Context())
    // …幂等处理…
    c.String(200, "SUCCESS")
})
```

echo / chi / fiber 同理：**拿到 `http.Header` 和原始 body 字节，调 `(*Client).Verify`**，就这两件事。

**别用自由函数版本的 `Middleware(secret)` / `Verify(header, body, secret)`**，除非你手上真的没有
`*Client`：那个版本要求你在 handler 这一侧再读一遍密钥，而读空的后果是**每一条真实通知都被 401**
——jjpay 按阶梯重试 10 次约 24 小时后进死信，你这边什么日志都没有。
`Middleware("")` 会直接 panic 正是为了不让这件事发生在运行期。

---

## 三、商户侧要做的事

### 1. 验签（SDK 帮你做了，但你得真的接上）

不验签的回调端点 = **任何人都能给你发"已支付"**。用 `Middleware` 或 `Verify`，
并且**不要**在验签之前读取/相信 body 里的任何字段。

`Verify` 除了签名还卡**时间戳窗口（默认 ±5 分钟）**：签名本身不会过期，
截获一条合法通知无限重放的话，每次都验签通过——窗口是唯一挡得住重放的那道闸。
不要为了"省事"把窗口调很大；服务器时钟没校准才是要修的东西。

### 2. 幂等（SDK 帮不了你）

同一事件**可能收到多次**（投递重试、极端情况下的重复投递）。

**先把去重键取对。** 一个支付单只会成功一次，但**可以退很多次**：

| 事件 | 去重键 |
|---|---|
| `pay.succeeded` / `order.closed` | `trade_no` + `event` |
| `refund.succeeded` / `refund.failed` | **`refund_no`** + `event` |

拿 `trade_no + event` 去给退款事件去重，同一支付单的第二笔退款会被当成重复投递
**直接丢掉**——那是一笔真实发生、你却没入账的退款。
（一张表接多个 App 的话，键里还要带上你自己的 App 标识。）

**再把去重和业务放进同一个事务。**

```go
func handle(evt *jjpay.Event) error {
    key := evt.TradeNo            // 支付类
    if evt.RefundNo != "" {
        key = evt.RefundNo        // 退款类
    }

    tx, err := db.Begin()
    if err != nil {
        return err                // 别回 SUCCESS，让 jjpay 重投
    }
    defer tx.Rollback()

    res, err := tx.Exec(`INSERT INTO pay_event_seen(biz_no, event) VALUES(?,?)
                         ON CONFLICT DO NOTHING`, key, evt.Event)
    if err != nil {
        return err                // 【数据库出错 ≠ 重复】不能只看 RowsAffected
    }
    if n, err := res.RowsAffected(); err != nil {
        return err
    } else if n == 0 {
        return nil                // 真的处理过了 → 回 SUCCESS
    }

    if err := deliver(tx, evt); err != nil {   // 开通服务也在这个事务里
        return err
    }
    return tx.Commit()
}
```

两个点，少哪个都会丢业务：

1. **不能"先写已处理、再去开通服务"。** 中间失败的话，去重记录已经落下了，
   jjpay 重投的那几次全部被挡在门外，而服务一次都没开通——**一笔收了钱没发货的单**，
   且没有任何告警。放进同一个事务，失败就一起回滚，下一次重投才救得回来。
2. **`RowsAffected == 0` 不等于"重复"。** 数据库连接断了、约束冲突以外的错误，
   `Exec` 返回的是 `err`，不是 0 行。把 `err` 当成"处理过了"回一个 SUCCESS，
   这条通知就再也不会来了。

开通服务要调外部系统（发短信、通知第三方）时，事务里只写一条待办，
由你自己的 outbox / 任务队列去投——**别把网络调用放进数据库事务**。

「先查一下有没有处理过，再处理」的两步式写法在并发下会重复发货——两个请求同时查到"没处理过"。
必须让数据库的唯一约束来裁决。

### 3. 核对回执——这一条 SDK 已经替你做了

`CreateOrder` / `QueryOrder` / `QueryOrderByOutTradeNo` / `CloseOrder` /
`Refund` / `QueryRefund` 都会在返回前核对**回执里的单号是不是本次请求的那一个**，
对不上返回 `ErrBadResponse`。你不需要再写一遍。

应答签名证明的是"这段内容是 jjpay 发的"，不证明"是回答哪一次请求的"——
待签串里不含 method 与 path（微信支付 APIv3 的应答验签同样如此）。
比对回执里的单号就能识破，SDK 已经替你比了。

你仍然要做的是别拿 `status` 当发货依据的唯一来源：发货以异步通知或查单为准，
回跳页只展示。

---

## 四、API 一览

枚举：`ChannelAlipay / ChannelWechat / ChannelMock`、
`MethodAlipayPage / MethodAlipayWap / MethodWechatNative / MethodWechatH5 / MethodWechatJSAPI`、
`OrderPending / OrderPaying / OrderPaid / OrderClosed`。

**只有 `RefundStatus` 是整数**（`RefundProcessing`=1 / `RefundSucceeded`=2 / `RefundFailed`=3），
它是退款单自己的状态，与上面那三组对外枚举不同源。写 `switch` 时注意别当成字符串。

| 方法 | 接口 | 重试 |
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
| `(*Client).VerifyReturn(query) (*ReturnResult, error)` | 回跳结果验签 | — |
| `SignNotify(secret, event, body, at) (http.Header, error)` | **造一条通知的签名头，给你写自己的回调测试用** | — |

签名原语（HMAC 计算、待签串拼接）不导出。需要自己造一条能通过验签的通知来测试
自己的 handler 时，用 `SignNotify`。

### 金额

**一律 `int64` 币种最小单位（CNY 即分），字段名带 `Minor` 后缀，币种走 `Currency`（留空即 CNY）**。本包不提供以元为单位的便捷参数——
浮点的元是让人算错钱的邀请。要展示成元请在渲染层除 100。

### 写操作永不自动重试

`CreateOrder` / `Refund` / `CloseOrder` 超时或网络错误时，SDK **原样把错误抛给你**，
一次都不重试。

因为 SDK 无法区分"没建成单"和"建成了但回执丢了"。自作主张重试会让**其实已经成功**的
请求看起来像失败。正确的重试姿势是**你自己用同一个 `OutTradeNo` / `OutRefundNo` 再调一次**——
那是幂等的：要么拿回原单，要么拿到 `ErrOrderConflict`（说明这个单号被不同参数占了，
那是业务 bug，不是网络抖动）。

读操作（查单、查退款）会对下面这些做有限重试（默认 2 次，指数退避 200ms / 400ms）：

- 传输层失败（连不上、连接被断、超时）
- HTTP 5xx 与 429
- 业务码 `10002`（系统繁忙）与 `20006`（限流）——**这两个必须单列**，
  因为 jjpay 的业务响应 HTTP 恒为 200，只认 429 的话服务端自己的限流一次都不会被重试

其余业务错误码、验签失败、`context` 取消**一次都不重试**——重试只是把同一个错误再犯一遍。

### 幂等语义

| 接口 | 幂等键 | 参数一致 | 参数不一致 |
|---|---|---|---|
| `CreateOrder` | `(app_id, out_trade_no)` | 返回**已有单**（同一个 `CheckoutURL`），`err == nil` | `ErrOrderConflict`，**绝不改动已有单** |
| `Refund` | `(app_id, out_refund_no)` | 返回同一张退款单，不重复退 | `ErrRefundNoConflict` |
| `CloseOrder` | 单号 | 已关闭再关 = 成功 | 已支付的单 → `ErrOrderStatusDenied`（要退钱用 `Refund`） |

一致性的判定字段是 `TotalMinor` + `Subject` + `NotifyURL`，其余字段不同视为一致。

### 退款的同步返回可能已经是终态，按 `Status` 判

`Status == RefundSucceeded` 就是真退成功了（支付宝的退款是同步接口，通常走这条），
可以当场记账。`Status == RefundProcessing` 才是只收下了、结果未定——微信原路退回
银行卡要 T+1~3 天，通常是这一档，真实结果走 `refund.succeeded` / `refund.failed`
通知，或用 `QueryRefund` 查。

**不要凭「调用没报错」就记账退款成功**，那和 `Status` 是两回事。

无论同步返回哪一档，终态都会再推一条通知（同步已成功也推，防的是响应写回途中
断连）。按 `RefundNo` 幂等即可，别把它当成第二笔退款。

### 错误处理

业务错误一律是 `*APIError`，用哨兵 + `errors.Is` 判别，别比对文案：

```go
resp, err := c.CreateOrder(ctx, req)
switch {
case err == nil:
    // 成功
case errors.Is(err, jjpay.ErrOrderConflict):
    // 单号撞了且参数不一致 —— 你的业务 bug，查为什么同一个单号金额变了
case errors.Is(err, jjpay.ErrAppDisabled):
    // App 被停用了，联系 jjpay 管理员
default:
    // 网络/超时/5xx：用同一个 out_trade_no 重试，或让用户重新发起
}
```

哨兵清单见 `errors.go`。另外三类非业务错误：

- `*HTTPError` —— 非 200 的 HTTP 响应（网关 502、路径写错等）。jjpay 的业务响应恒为 200。
- `ErrResponseSign` —— **响应验签失败**。SDK 会校验每个响应的签名
  （`X-Jjpay-Timestamp` / `X-Jjpay-Nonce` / `X-Jjpay-Signature`），
  防中间人改回执。命中它说明回执不可信，**不要把它当"失败"处理，要当"不知道"处理**：去查单。
- `ErrTimestampWindow` —— 时间戳超窗。通常是**你的服务器时钟没校准**，先看 NTP。

> **一个例外**：服务端对**鉴权没过**的响应刻意不签名（签了等于给攻击者一个"密钥对不对"的
> 预言机）。这类响应里没有任何值钱的数据，所以 SDK 会把真实错误码透出来
> （`ErrSignMismatch` / `ErrTimestampSkew` / `ErrAppDisabled` …），而不是笼统地报
> `ErrResponseSign`——否则你联调时看到的永远是"响应验签失败"，猜不到是密钥配错了。
> 这条只开给鉴权段（20001~20099）：**未签名的成功响应或其它业务响应一律按验签失败处理**。

`CodeOf(err)` 取业务码；SDK 没登记过的新错误码同样会返回 `*APIError`，不会被吞成成功。

---

## 五、开发

```bash
go test ./...
```

签名部分有已知向量测试：给定固定的 method / path / timestamp / nonce / body / secret，
断言签出确定的 hex 串。其中几组与服务端共用同一份向量，任何一方改了算法都会当场红。

这些期望值是已发布契约的一部分，别为了让测试变绿去改它们。
