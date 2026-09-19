package jjpay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// EventType 异步通知的事件名。
type EventType string

const (
	EventPaySucceeded    EventType = "pay.succeeded"    // 支付单转为 paid
	EventOrderClosed     EventType = "order.closed"     // 支付单转为 closed（超时或主动关）
	EventRefundSucceeded EventType = "refund.succeeded" // 退款成功
	EventRefundFailed    EventType = "refund.failed"    // 退款失败
	// EventRefundStalled 退款卡在渠道侧、需要人工介入。
	//
	// 【它不是终态】钱已经离开商户账户、还没落到用户手里，退款单仍是"处理中"。
	// 收到它之后一定还会收到一条 refund.succeeded 或 refund.failed。
	//
	// 目前只有微信会出现：退往用户原路时银行拒收（卡作废或冻结），钱停在微信侧，
	// 要由网关侧的人去渠道后台决定去向。支付宝不会——它退卡失败会自动退到
	// 用户的支付宝余额。
	//
	// 收到它该做的事：别再等这笔退款自己好（可能要几小时到几天），
	// 把单号记下来告诉自己的客服。切勿据此判定退款失败去做二次补偿——
	// 那笔钱后面仍可能到用户手里。
	EventRefundStalled EventType = "refund.stalled"
)

// Event 是一条已验签的异步通知。
//
// 所有事件共用一个结构体：字段按事件类型部分填充，用不到的留零值。判断先看
// Event 字段再取相应字段，别对着零值猜。
type Event struct {
	Event EventType `json:"event"`

	// —— 支付类（pay.succeeded / order.closed）——
	TradeNo        string  `json:"trade_no,omitempty"`
	OutTradeNo     string  `json:"out_trade_no,omitempty"`
	TotalMinor     int64   `json:"total_minor,omitempty"`
	Currency       string  `json:"currency,omitempty"`
	Channel        Channel `json:"channel,omitempty"`
	Method         Method  `json:"method,omitempty"`
	ChannelTradeNo string  `json:"channel_trade_no,omitempty"`
	// PaidAt / ClosedAt 按事件类型二选一：pay.succeeded 给前者，
	// order.closed 给后者。omitempty 对 struct 类型不生效，所以这两个
	// 字段在没值时会原样序列化成 0001-01-01T00:00:00Z——用 IsZero() 判，别判 nil。
	PaidAt   time.Time `json:"paid_at"`
	ClosedAt time.Time `json:"closed_at"`
	Attach   string    `json:"attach,omitempty"`

	// —— 退款类（refund.succeeded / refund.failed / refund.stalled）——
	RefundNo    string `json:"refund_no,omitempty"`
	OutRefundNo string `json:"out_refund_no,omitempty"`
	// AmountMinor 本次退款金额；RefundedMinor 该支付单累计已退。
	AmountMinor   int64 `json:"amount_minor,omitempty"`
	RefundedMinor int64 `json:"refunded_minor,omitempty"`
	// FinishedAt 只在终态事件里有值。refund.stalled 给的是 StalledAt——
	// 那笔退款还没结束，给它一个"完成时刻"会让人以为这就是结果。
	FinishedAt time.Time `json:"finished_at"`
	FailReason string    `json:"fail_reason,omitempty"`
	// StalledAt / StalledReason 只在 refund.stalled 里有值，见 EventRefundStalled。
	// StalledReason 是渠道原话，可以直接给客服看。
	StalledAt     time.Time `json:"stalled_at"`
	StalledReason string    `json:"stalled_reason,omitempty"`

	// Raw 是验签通过的原始 body。字段不够用时自己解，别再去读 r.Body。
	Raw []byte `json:"-"`
	// Timestamp / Nonce 来自请求头，已参与签名。
	Timestamp time.Time `json:"-"`
	Nonce     string    `json:"-"`
}

// NotifyOptions 验签的可调项。除测试外一般只需要 Secret。
type NotifyOptions struct {
	// Secret 该 App 的 app_secret，必填。
	Secret string
	// SkewWindow 允许的时间戳偏差，默认 ±5 分钟。
	//
	// 不要把它调得很大来"省事"：签名是真的、只是被录下来重放时，时间戳
	// 窗口是唯一挡得住的那道闸。
	SkewWindow time.Duration
	// Now 取当前时间，测试注入用；nil 即 time.Now。
	Now func() time.Time
	// OnError 中间件验签失败时的回调，用于打日志。nil 则静默。
	// 强烈建议接上——否则回调端点静默回 401，排查时毫无线索。
	OnError func(r *http.Request, err error)
}

// maxNotifyBytes 通知 body 的读取上限。通知体只有十几个字段，256 KiB 绰绰
// 有余；不设闸等于把"一个请求撑爆进程"的按钮放在公网上。
const maxNotifyBytes = 256 << 10

// Verify 验证一条异步通知并解析出事件（默认 ±5 分钟时间戳窗口）。
//
// 框架无关：把请求头与原始 body 字节交进来即可，gin / echo / chi 都能
// 两行接上（见 README）。
//
// 校验顺序：头齐全 → 时间戳在窗口内 → 签名一致（hmac.Equal 常数时间）。
// 任一失败返回错误，此时不要处理这条通知。
//
// 待签串：{EVENT}\n{TIMESTAMP}\n{NONCE}\n{SHA256_HEX(BODY)}。
func Verify(header http.Header, body []byte, secret string) (*Event, error) {
	return VerifyWithOptions(header, body, NotifyOptions{Secret: secret})
}

// VerifyWithOptions 同 Verify，可调时间戳窗口与时钟。
func VerifyWithOptions(header http.Header, body []byte, opts NotifyOptions) (*Event, error) {
	if opts.Secret == "" {
		return nil, fmt.Errorf("jjpay: NotifyOptions.Secret 不能为空")
	}
	window := opts.SkewWindow
	if window <= 0 {
		window = DefaultSkewWindow
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	event := header.Get(HeaderEvent)
	ts := header.Get(HeaderTimestamp)
	nonce := header.Get(HeaderNonce)
	sig := header.Get(HeaderSignature)
	if event == "" || ts == "" || nonce == "" || sig == "" {
		return nil, fmt.Errorf("%w: 需要 %s/%s/%s/%s", ErrMissingHeader,
			HeaderEvent, HeaderTimestamp, HeaderNonce, HeaderSignature)
	}

	// 时间戳先于签名校验：过期的通知连算 HMAC 都不必。
	if err := checkSkew(ts, now(), window); err != nil {
		return nil, err
	}
	if !verifySign(opts.Secret, notifyPayload(event, ts, nonce, hashBody(body)), sig) {
		return nil, ErrNotifySign
	}

	var evt Event
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil, fmt.Errorf("%w: 通知 body 解析失败: %w", ErrBadResponse, err)
	}
	// body 里的 event 与头不一致说明两端有一方出了问题，宁可拒。
	if evt.Event != "" && string(evt.Event) != event {
		return nil, fmt.Errorf("%w: 事件类型与签名头不一致（头 %q，body %q）", ErrNotifySign, event, evt.Event)
	}
	evt.Event = EventType(event)
	evt.Raw = body
	evt.Nonce = nonce
	if sec, err := strconv.ParseInt(ts, 10, 64); err == nil {
		evt.Timestamp = time.Unix(sec, 0)
	}
	return &evt, nil
}

type ctxKey struct{}

// WithEvent 把已验签的事件放进 context。自己写框架胶水时用。
func WithEvent(ctx context.Context, evt *Event) context.Context {
	return context.WithValue(ctx, ctxKey{}, evt)
}

// EventFrom 从 context 取出已验签的事件；没有则返回 nil。
//
// 只有经过 Middleware（或自己调 WithEvent）的请求才有值。
func EventFrom(ctx context.Context) *Event {
	evt, _ := ctx.Value(ctxKey{}).(*Event)
	return evt
}

// Middleware 返回标准库风格的验签中间件：
//
//	mux.Handle("/pay/notify", jjpay.Middleware(secret)(myHandler))
//
// 验签通过则把事件放进 request context（用 EventFrom 取）、把 body 回填给下游
// 再调用它；验签失败直接 401，下游一次都不会被调用。
//
// 下游必须回 HTTP 200 且 body 为 SUCCESS（或 JSON {"code":"SUCCESS"}），
// 否则会被重试。幂等仍要自己做。
//
// secret 为空会 panic：放行的话每一条真实通知都会被 401，而调用方拿不到任何
// 线索。有 Client 就用 (*Client).Middleware，可以完全避开这个口子。
func Middleware(secret string) func(http.Handler) http.Handler {
	return MiddlewareWithOptions(NotifyOptions{Secret: secret})
}

// MiddlewareWithOptions 同 Middleware，可调窗口、时钟与错误回调。
// opts.Secret 为空同样 panic，理由见 Middleware。
func MiddlewareWithOptions(opts NotifyOptions) func(http.Handler) http.Handler {
	if opts.Secret == "" {
		panic("jjpay: 通知验签的 secret 为空。放行的话每一条真实通知都会被 401，" +
			"而商户侧看不到任何线索——检查 JJPAY_APP_SECRET 是否读到了值")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNotifyBytes))
			if err != nil {
				reject(w, r, opts, fmt.Errorf("jjpay: 读取通知 body 失败: %w", err))
				return
			}
			evt, err := VerifyWithOptions(r.Header, body, opts)
			if err != nil {
				reject(w, r, opts, err)
				return
			}
			// 回填 body，下游想自己解也行。
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			next.ServeHTTP(w, r.WithContext(WithEvent(r.Context(), evt)))
		})
	}
}

// reject 统一回 401。响应体刻意笼统，不告诉对端是哪一步没过。
func reject(w http.ResponseWriter, r *http.Request, opts NotifyOptions, err error) {
	if opts.OnError != nil {
		opts.OnError(r, err)
	}
	http.Error(w, "invalid signature", http.StatusUnauthorized)
}

// Client 侧的便捷入口：secret 由 New 校验过，不必在 handler 那侧再读一遍。

// Verify 用本 Client 的凭据验一条异步通知，等价于 Verify(header, body, secret)。
func (c *Client) Verify(header http.Header, body []byte) (*Event, error) {
	return VerifyWithOptions(header, body, c.notifyOptions(NotifyOptions{}))
}

// Middleware 用本 Client 的凭据返回验签中间件，等价于 Middleware(secret)。
//
//	mux.Handle("/pay/notify", c.Middleware()(myHandler))
func (c *Client) Middleware() func(http.Handler) http.Handler {
	return MiddlewareWithOptions(c.notifyOptions(NotifyOptions{}))
}

// MiddlewareWithOptions 同 Middleware，可再给 OnError 等选项。
// opts.Secret 与 opts.SkewWindow 若留空，用 Client 上的值。
func (c *Client) MiddlewareWithOptions(opts NotifyOptions) func(http.Handler) http.Handler {
	return MiddlewareWithOptions(c.notifyOptions(opts))
}

func (c *Client) notifyOptions(opts NotifyOptions) NotifyOptions {
	if opts.Secret == "" {
		opts.Secret = c.secret
	}
	if opts.SkewWindow <= 0 {
		opts.SkewWindow = c.skew
	}
	if opts.Now == nil {
		opts.Now = c.now
	}
	return opts
}

// SignNotify 造出一条通知该带的签名头，专门给商户写自己的通知处理测试用。
//
//	h, _ := jjpay.SignNotify(secret, jjpay.EventPaySucceeded, body, time.Now())
//	req := httptest.NewRequest("POST", "/pay/notify", bytes.NewReader(body))
//	req.Header = h
//	myNotifyHandler.ServeHTTP(rec, req) // 走真实验签路径
//
// body 必须是最终发出去的那串字节：签的是它的哈希，序列化一次签一次。
func SignNotify(secret string, event EventType, body []byte, at time.Time) (http.Header, error) {
	if secret == "" {
		return nil, fmt.Errorf("jjpay: SignNotify 的 secret 不能为空")
	}
	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	h := http.Header{}
	h.Set(HeaderEvent, string(event))
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderSignature, computeSign(secret, notifyPayload(string(event), ts, nonce, hashBody(body))))
	return h, nil
}
