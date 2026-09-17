package jjpay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RefundReq 退款请求。
//
// TradeNo 与 OutTradeNo 二选一指明要退哪一单。
type RefundReq struct {
	TradeNo    string `json:"trade_no,omitempty"`
	OutTradeNo string `json:"out_trade_no,omitempty"`
	// OutRefundNo 商户退款单号，商户内唯一，同时是幂等键。
	OutRefundNo string `json:"out_refund_no"`
	// AmountMinor 本次退款金额（分）。部分退就填部分金额；可多次退，
	// 服务端保证累计不超原单（超了返回 ErrRefundExceedTotal）。
	AmountMinor int64  `json:"amount_minor"`
	Reason      string `json:"reason,omitempty"`
	NotifyURL   string `json:"notify_url,omitempty"`
}

// Refund 退款单。
//
// 查退款比下退款多返回 ChannelRefundNo / FinishedAt / FailReason，同步下单
// 时这三个字段为空。
type Refund struct {
	RefundNo    string `json:"refund_no"`
	OutRefundNo string `json:"out_refund_no"`
	TradeNo     string `json:"trade_no"`
	OutTradeNo  string `json:"out_trade_no,omitempty"`
	// AmountMinor 本退款单的金额（分）。
	AmountMinor int64 `json:"amount_minor"`
	// TotalMinor 原支付单的应付总额；RefundedMinor 该支付单累计已退。
	// 两者都来自原单，拿它们就能当场判断还能退多少（TotalMinor-RefundedMinor），
	// 不必为了对账再查一次单。
	TotalMinor    int64        `json:"total_minor"`
	RefundedMinor int64        `json:"refunded_minor"`
	Currency      string       `json:"currency"`
	Status        RefundStatus `json:"status"`
	StatusText    string       `json:"status_text"`
	CreatedAt     time.Time    `json:"created_at"`

	ChannelRefundNo string `json:"channel_refund_no,omitempty"`
	// FinishedAt 终态时间，未终结时是零值——用 IsZero() 判，别判 nil。
	FinishedAt time.Time `json:"finished_at"`
	FailReason string    `json:"fail_reason,omitempty"`
}

// Refund 发起退款。
//
// # 同步返回的是"受理"，不是"到账"
//
// 拿到 Status == RefundProcessing 只说明 jjpay 收下了这笔退款请求。真实
// 结果经 refund.succeeded / refund.failed 异步通知，或由 QueryRefund 查。
// 不要凭同步返回就给用户记账退款成功。
//
// # 幂等
//
// 幂等键是 (app_id, out_refund_no)。同一 OutRefundNo 重复调用返回同一张
// 退款单，不会重复退钱；单号相同但金额等参数不一致返回 ErrRefundNoConflict。
//
// # 不重试
//
// 同 CreateOrder：写操作永不自动重试。超时了用同一个 OutRefundNo 再调一次。
func (c *Client) Refund(ctx context.Context, req RefundReq) (*Refund, error) {
	var out Refund
	err := c.do(ctx, call{method: "POST", path: pathRefunds, body: req, out: &out, retry: false})
	if err == nil {
		err = sameRefund("out_refund_no", req.OutRefundNo, out.OutRefundNo)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryRefund 按 jjpay 退款单号查退款。读操作，允许有限重试。
func (c *Client) QueryRefund(ctx context.Context, refundNo string) (*Refund, error) {
	if strings.TrimSpace(refundNo) == "" {
		return nil, errors.New("jjpay: refundNo 不能为空")
	}
	var out Refund
	err := c.do(ctx, call{
		method: "GET",
		path:   pathRefunds + "/" + url.PathEscape(refundNo),
		out:    &out,
		retry:  true,
	})
	if err == nil {
		err = sameRefund("refund_no", refundNo, out.RefundNo)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryRefundByOutRefundNo 按商户退款单号查退款。读操作，允许有限重试。
func (c *Client) QueryRefundByOutRefundNo(ctx context.Context, outRefundNo string) (*Refund, error) {
	if strings.TrimSpace(outRefundNo) == "" {
		return nil, errors.New("jjpay: outRefundNo 不能为空")
	}
	q := url.Values{"out_refund_no": {outRefundNo}}
	var out Refund
	err := c.do(ctx, call{
		method: "GET",
		path:   pathRefunds + "?" + q.Encode(),
		out:    &out,
		retry:  true,
	})
	if err == nil {
		err = sameRefund("out_refund_no", outRefundNo, out.OutRefundNo)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// sameRefund 核对回执说的是不是本次问的那一笔退款。
// 应答签名的待签串不含 method/path，一条合法回执理论上可以被搬到另一次请求上
// 当答复；比对单号就能识破，而"本次问的是哪一笔"只有 SDK 这里知道。
func sameRefund(field, want, got string) error {
	if want == "" || got == "" || want == got {
		return nil
	}
	return fmt.Errorf("%w: 回执说的不是本次请求的那一笔（%s 要 %q，回执 %q）",
		ErrBadResponse, field, want, got)
}
