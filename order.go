package jjpay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Item 收银台展示用的订单明细。
//
// 行小计 = UnitMinor × Qty。Qty 省略或 ≤0 一律当 1 —— 只想说"这一行多少钱"
// 而不想拆单价时，给 UnitMinor 一个数就够了。
type Item struct {
	Name      string `json:"name"`
	UnitMinor int64  `json:"unit_minor"` // 单价，币种最小单位
	Qty       int    `json:"qty"`
}

// CreateOrderReq 下单请求。
type CreateOrderReq struct {
	// OutTradeNo 商户单号，商户内唯一。它同时是幂等键，见 CreateOrder。
	OutTradeNo string `json:"out_trade_no"`
	// Subject 订单标题，≤128。
	Subject string `json:"subject"`
	// TotalMinor 应付金额，币种最小单位（CNY 即分），必须 > 0。jjpay 收到的
	// 就是最终应付额，优惠券/折扣/差价一律在商户侧算完再传。
	TotalMinor int64 `json:"total_minor"`
	// Currency ISO 4217 币种码，留空即 CNY。目前渠道只收 CNY，传其它码会返回 10001。
	Currency string `json:"currency,omitempty"`

	Description string `json:"description,omitempty"` // ≤256
	Items       []Item `json:"items,omitempty"`
	// OriginalMinor 原价，DiscountLabel 是收银台账单上优惠那一行的名称
	// （≤16，留空显示「优惠」）。
	//
	// 优惠额 = OriginalMinor − TotalMinor，由服务端算。传两个价而不是
	// "价 + 折扣额"，是因为两个价都是商户系统里现成的事实，差额是推导值 ——
	// 让调用方自己减一遍，就多一次算错的机会。
	// 0 或等于 TotalMinor 都表示这一单没有优惠，账单只画明细行。
	//
	// 两者只是展示，TotalMinor 仍是唯一要收的钱：下单给渠道的是它，
	// 查单比对的是它，可退上限也是它 —— 退款永远按实付退，与原价无关。
	//
	// 给了 OriginalMinor 就必须 ≥ TotalMinor，否则返回 10001。带 Items 时
	// 明细是按原价列的，Σ(UnitMinor×Qty) 要等于 OriginalMinor（没给原价时
	// 等于 TotalMinor）。不带 Items 只给 OriginalMinor 也可以，
	// 账单画成「原价 / 优惠 / 合计」三行。
	OriginalMinor int64  `json:"original_minor,omitempty"`
	DiscountLabel string `json:"discount_label,omitempty"`
	// Attach 商户透传字段，≤128，原样出现在异步通知里。
	Attach string `json:"attach,omitempty"`
	// ExpireMinutes 有效期，默认取服务端配置，上限 120。
	ExpireMinutes int `json:"expire_minutes,omitempty"`
	// NotifyURL 覆盖后台配置的回调地址。下单时会快照进订单，之后改后台配置
	// 不影响在途老单。
	NotifyURL string `json:"notify_url,omitempty"`
	// ReturnURL 用户支付完的回跳地址。回跳不代表支付成功，落地页必须
	// 自己查单确认。
	ReturnURL string `json:"return_url,omitempty"`
}

// CreateOrderResp 下单响应。
type CreateOrderResp struct {
	TradeNo    string `json:"trade_no"`
	OutTradeNo string `json:"out_trade_no"`
	// CheckoutURL 收银台链接，直接给用户跳。
	CheckoutURL string      `json:"checkout_url"`
	ExpireAt    time.Time   `json:"expire_at"`
	Status      OrderStatus `json:"status"`
}

// Order 查单响应。
//
// 可空的时间字段（PaidAt/ClosedAt）在服务端为 null 时是零值，用
// IsZero() 判断，不要判 nil。
type Order struct {
	TradeNo    string `json:"trade_no"`
	OutTradeNo string `json:"out_trade_no"`
	Subject    string `json:"subject"`
	// TotalMinor 应付金额；RefundedMinor 已退金额。单位都是 Currency 的最小单位。
	// 退款不占用 Status：0=未退，0<x<TotalMinor=部分退，==TotalMinor=全退。
	TotalMinor    int64       `json:"total_minor"`
	RefundedMinor int64       `json:"refunded_minor"`
	Currency      string      `json:"currency"`
	Status        OrderStatus `json:"status"`
	StatusText    string      `json:"status_text"`
	Channel       Channel     `json:"channel"`
	Method        Method      `json:"method"`
	// ChannelTradeNo 渠道侧单号（微信 transaction_id / 支付宝 trade_no）。
	ChannelTradeNo string    `json:"channel_trade_no"`
	PaidAt         time.Time `json:"paid_at"`
	ExpireAt       time.Time `json:"expire_at"`
	ClosedAt       time.Time `json:"closed_at"`
	Attach         string    `json:"attach"`
	CreatedAt      time.Time `json:"created_at"`
}

// Paid 是否已支付（终态）。
func (o *Order) Paid() bool { return o.Status == OrderPaid }

// CloseOrderResp 关单响应。
type CloseOrderResp struct {
	TradeNo string      `json:"trade_no"`
	Status  OrderStatus `json:"status"`
}

const (
	pathOrders  = "/openapi/v1/orders"
	pathRefunds = "/openapi/v1/refunds"
)

// CreateOrder 创建支付单，返回收银台链接。
//
// # 幂等
//
// 幂等键是 (app_id, out_trade_no)。同一 OutTradeNo 重复调用：
//
//   - 参数与已有单一致（判定字段：TotalMinor + Subject + NotifyURL）→
//     返回已有的那一单，含同一个 CheckoutURL，不是错误；
//   - 参数不一致（尤其金额不同）→ 返回 ErrOrderConflict，
//     且服务端绝不改动已有单。
//
// # 不重试
//
// 写操作永不自动重试：SDK 分不清"没建成单"和"建成了但回执丢了"，自作主张
// 重试会让已经成功的请求看起来像失败。要重试请用同一个 OutTradeNo 再调一次，
// 那是幂等的。
func (c *Client) CreateOrder(ctx context.Context, req CreateOrderReq) (*CreateOrderResp, error) {
	var out CreateOrderResp
	err := c.do(ctx, call{method: "POST", path: pathOrders, body: req, out: &out, retry: false})
	if err == nil {
		err = sameOrder("out_trade_no", req.OutTradeNo, out.OutTradeNo)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryOrder 按 jjpay 单号查单。
//
// 服务端可能在此惰性回源向渠道查一次，所以这个调用比纯读
// 库慢，别拿它当轮询高频打。付款结果的主路径是异步通知，查单是补偿。
//
// 读操作，允许有限重试（网络错误与 5xx/429）。
func (c *Client) QueryOrder(ctx context.Context, tradeNo string) (*Order, error) {
	if strings.TrimSpace(tradeNo) == "" {
		return nil, errors.New("jjpay: tradeNo 不能为空")
	}
	var out Order
	err := c.do(ctx, call{
		method: "GET",
		path:   pathOrders + "/" + url.PathEscape(tradeNo),
		out:    &out,
		retry:  true,
	})
	if err == nil {
		err = sameOrder("trade_no", tradeNo, out.TradeNo)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryOrderByOutTradeNo 按商户单号查单，走 ?out_trade_no= 形态。
//
// 读操作，允许有限重试。
func (c *Client) QueryOrderByOutTradeNo(ctx context.Context, outTradeNo string) (*Order, error) {
	if strings.TrimSpace(outTradeNo) == "" {
		return nil, errors.New("jjpay: outTradeNo 不能为空")
	}
	q := url.Values{"out_trade_no": {outTradeNo}}
	var out Order
	err := c.do(ctx, call{
		method: "GET",
		path:   pathOrders + "?" + q.Encode(),
		out:    &out,
		retry:  true,
	})
	if err == nil {
		err = sameOrder("out_trade_no", outTradeNo, out.OutTradeNo)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseOrder 关闭支付单。
//
//   - 已支付的单不能关，返回 ErrOrderStatusDenied——要退钱请用 Refund；
//   - 已关闭的单再关是幂等成功。
//
// 写操作，不自动重试（同 CreateOrder：重试请调用方自己发起，关单本身幂等）。
func (c *Client) CloseOrder(ctx context.Context, tradeNo string) (*CloseOrderResp, error) {
	if strings.TrimSpace(tradeNo) == "" {
		return nil, errors.New("jjpay: tradeNo 不能为空")
	}
	var out CloseOrderResp
	err := c.do(ctx, call{
		method: "POST",
		path:   pathOrders + "/" + url.PathEscape(tradeNo) + "/close",
		out:    &out,
		retry:  false,
	})
	if err == nil {
		err = sameOrder("trade_no", tradeNo, out.TradeNo)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// sameOrder 核对回执说的是不是本次问的那一笔支付单。理由见 sameRefund。
func sameOrder(field, want, got string) error {
	if want == "" || got == "" || want == got {
		return nil
	}
	return fmt.Errorf("%w: 回执说的不是本次请求的那一笔（%s 要 %q，回执 %q）",
		ErrBadResponse, field, want, got)
}
