package jjpay

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// ReturnResult 收银台把用户送回你的 return_url 时附带的、签名过的订单结果。
//
// 这是 jjpay 查单确认后签的，验签通过说明确实是 jjpay 给的、
// 没被改过，可以直接画支付成功 ¥19.90而不必再查一次单。但回跳和异步通知是两条
// 独立链路：用户付完关掉浏览器，回跳就没有了。开通服务 / 发货仍以通知或查单为准。
type ReturnResult struct {
	TradeNo    string
	OutTradeNo string
	// Status pending / paying / paid / closed
	Status     OrderStatus
	TotalMinor int64
	Currency   string
	// PaidAt 已支付时有，否则零值
	PaidAt    time.Time
	Timestamp time.Time
	Nonce     string
}

// Paid 是否已支付。
func (r *ReturnResult) Paid() bool { return r.Status == OrderPaid }

// ReturnOptions VerifyReturnWithOptions 的选项。
type ReturnOptions struct {
	// Secret 该 App 的 app_secret，必填。
	Secret string
	// SkewWindow 允许的时间戳偏差，默认 ±5 分钟。回跳链接可能被用户收藏、转发，
	// 时间窗是唯一挡得住"拿旧链接当凭证"的那道闸；要严格一次性可再记 Nonce。
	SkewWindow time.Duration
	// Now 取当前时间，测试注入用；nil 即 time.Now。
	Now func() time.Time
}

// VerifyReturn 验证回跳地址上的结果参数（默认 ±5 分钟时间戳窗口）。
//
//	res, err := jjpay.VerifyReturn(r.URL.Query(), secret)
//	if err != nil { /* 当作没带结果：查单 */ }
//	if res.Paid() { /* 画成功页 */ }
//
// 校验顺序：参数齐全 → 时间戳在窗口内 → 签名一致（hmac.Equal 常数时间）。
// 任一失败返回错误，此时不要用地址上的任何字段。
func VerifyReturn(query url.Values, secret string) (*ReturnResult, error) {
	return VerifyReturnWithOptions(query, ReturnOptions{Secret: secret})
}

// VerifyReturnWithOptions 带选项的 VerifyReturn。
func VerifyReturnWithOptions(query url.Values, opts ReturnOptions) (*ReturnResult, error) {
	if opts.Secret == "" {
		return nil, fmt.Errorf("jjpay: ReturnOptions.Secret 不能为空")
	}
	window := opts.SkewWindow
	if window <= 0 {
		window = DefaultSkewWindow
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	tradeNo := query.Get("trade_no")
	outTradeNo := query.Get("out_trade_no")
	status := query.Get("status")
	total := query.Get("total_minor")
	currency := query.Get("currency")
	paidAt := query.Get("paid_at")
	ts := query.Get("timestamp")
	nonce := query.Get("nonce")
	sig := query.Get("sign")
	if tradeNo == "" || status == "" || total == "" || ts == "" || nonce == "" || sig == "" {
		return nil, fmt.Errorf("%w: 需要 trade_no/status/total_minor/timestamp/nonce/sign", ErrReturnMissing)
	}

	if err := checkSkew(ts, now(), window); err != nil {
		return nil, err
	}
	if !verifySign(opts.Secret, returnPayload(tradeNo, outTradeNo, status, total, currency, paidAt, ts, nonce), sig) {
		return nil, ErrReturnSign
	}

	res := &ReturnResult{TradeNo: tradeNo, OutTradeNo: outTradeNo, Status: OrderStatus(status), Currency: currency, Nonce: nonce}
	var err error
	if res.TotalMinor, err = strconv.ParseInt(total, 10, 64); err != nil {
		return nil, fmt.Errorf("%w: total_minor 非法 %q", ErrBadResponse, total)
	}
	if sec, err := strconv.ParseInt(ts, 10, 64); err == nil {
		res.Timestamp = time.Unix(sec, 0)
	}
	if paidAt != "" {
		if res.PaidAt, err = time.Parse(time.RFC3339, paidAt); err != nil {
			return nil, fmt.Errorf("%w: paid_at 非法 %q", ErrBadResponse, paidAt)
		}
	}
	return res, nil
}

// VerifyReturn 用本 Client 的凭据验回跳结果，等价于 VerifyReturn(query, secret)。
func (c *Client) VerifyReturn(query url.Values) (*ReturnResult, error) {
	return VerifyReturnWithOptions(query, ReturnOptions{
		Secret:     c.secret,
		SkewWindow: c.skew,
		Now:        c.now,
	})
}
