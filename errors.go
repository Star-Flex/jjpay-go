package jjpay

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Code 是响应信封里的业务码。
//
// HTTP 状态码恒为 200，业务结果只看这个 code。所以 SDK 里凡是判断成败
// 一律看 Code，不要去看 HTTP 状态——非 200 只可能是网关/代理出的岔子，
// 那种情况 SDK 返回 *HTTPError。
type Code int64

const (
	// 通用段
	CodeSuccess    Code = 10000
	CodeParamErr   Code = 10001
	CodeServerBusy Code = 10002
	CodeNotFound   Code = 10003 // 未在公开错误码表中列出

	// 商户鉴权段
	CodeAppNotFound   Code = 20001
	CodeSignMismatch  Code = 20002
	CodeTimestampSkew Code = 20003
	CodeNonceReplay   Code = 20004
	CodeAppDisabled   Code = 20005
	CodeRateLimited   Code = 20006 // 未在公开错误码表中列出

	// 支付单段
	CodeOrderConflict     Code = 30001
	CodeOrderNotFound     Code = 30002
	CodeOrderExpired      Code = 30003
	CodeOrderStatusDenied Code = 30004
	CodeTokenInvalid      Code = 30005

	// 渠道下单段
	CodeMethodDisabled    Code = 40001
	CodeNoChannelAccount  Code = 40002
	CodeChannelPrepayFail Code = 40003
	CodeOpenIDRequired    Code = 40004

	// 退款段
	CodeRefundOrderNotPaid Code = 60001
	CodeRefundExceedTotal  Code = 60002
	CodeRefundNoConflict   Code = 60003
	CodeRefundChannelFail  Code = 60004
	CodeRefundNotFound     Code = 60005 // 未在公开错误码表中列出
)

// codeText 给哨兵错误一句人话。真要展示给终端用户请用响应里的 Msg
// （服务端下发的文案），这里只是为了 err.Error() 好读。
func codeText(c Code) string {
	switch c {
	case CodeSuccess:
		return "success"
	case CodeParamErr:
		return "参数错误"
	case CodeServerBusy:
		return "系统繁忙"
	case CodeNotFound:
		return "记录不存在"
	case CodeAppNotFound:
		return "鉴权失败（app 不存在）"
	case CodeSignMismatch:
		return "鉴权失败（签名不匹配）"
	case CodeTimestampSkew:
		return "请求时间戳超出允许范围"
	case CodeNonceReplay:
		return "请求重复（nonce 重放）"
	case CodeAppDisabled:
		return "接入方已停用"
	case CodeRateLimited:
		return "请求过于频繁"
	case CodeOrderConflict:
		return "商户单号已存在且订单信息不一致"
	case CodeOrderNotFound:
		return "支付单不存在"
	case CodeOrderExpired:
		return "支付单已过期"
	case CodeOrderStatusDenied:
		return "当前订单状态不允许该操作"
	case CodeTokenInvalid:
		return "支付链接已失效"
	case CodeMethodDisabled:
		return "该支付方式未开启"
	case CodeNoChannelAccount:
		return "没有可用的收款账户"
	case CodeChannelPrepayFail:
		return "渠道下单失败"
	case CodeOpenIDRequired:
		return "缺少微信用户标识"
	case CodeRefundOrderNotPaid:
		return "原订单未支付，不能退款"
	case CodeRefundExceedTotal:
		return "退款累计金额超出原订单"
	case CodeRefundNoConflict:
		return "退款单号已存在且退款信息不一致"
	case CodeRefundChannelFail:
		return "渠道退款失败"
	case CodeRefundNotFound:
		return "退款单不存在"
	default:
		return "未知错误"
	}
}

// APIError 是服务端返回的业务错误（code != 10000）。
//
// 判别一律用 errors.Is 配下面的哨兵，别去比 Msg——Msg 是给人看的，随时会
// 被改文案：
//
//	if errors.Is(err, jjpay.ErrOrderConflict) { … }
//
// 本包没登记过的新错误码同样会被包成 *APIError，不会被吞成"成功"。
type APIError struct {
	Code Code            // 业务码
	Msg  string          // 服务端文案
	Data json.RawMessage // 出错时的字段级说明（validator 中文翻译），可能为空
}

func (e *APIError) Error() string {
	msg := e.Msg
	if msg == "" {
		msg = codeText(e.Code)
	}
	return fmt.Sprintf("jjpay: [%d] %s", e.Code, oneLine(msg, maxMsgRunes))
}

// Is 让 errors.Is 只按 Code 匹配，Msg/Data 不参与——哨兵不带这些。
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	return ok && t.Code == e.Code
}

func sentinel(c Code) *APIError { return &APIError{Code: c, Msg: codeText(c)} }

// 业务错误哨兵。用 errors.Is 判别。
var (
	ErrParamInvalid = sentinel(CodeParamErr)
	ErrServerBusy   = sentinel(CodeServerBusy)
	ErrNotFound     = sentinel(CodeNotFound)

	ErrAppNotFound   = sentinel(CodeAppNotFound)
	ErrSignMismatch  = sentinel(CodeSignMismatch)
	ErrTimestampSkew = sentinel(CodeTimestampSkew)
	ErrNonceReplay   = sentinel(CodeNonceReplay)
	ErrAppDisabled   = sentinel(CodeAppDisabled)
	ErrRateLimited   = sentinel(CodeRateLimited)

	// ErrOrderConflict 同一 out_trade_no 重复下单、但参数与已有单不一致
	// （尤其金额）。此时服务端绝不改动已有单。
	// 参数一致的重复下单不是错误，会直接返回已有单，见 CreateOrder 的注释。
	ErrOrderConflict     = sentinel(CodeOrderConflict)
	ErrOrderNotFound     = sentinel(CodeOrderNotFound)
	ErrOrderExpired      = sentinel(CodeOrderExpired)
	ErrOrderStatusDenied = sentinel(CodeOrderStatusDenied)
	ErrTokenInvalid      = sentinel(CodeTokenInvalid)

	ErrMethodDisabled    = sentinel(CodeMethodDisabled)
	ErrNoChannelAccount  = sentinel(CodeNoChannelAccount)
	ErrChannelPrepayFail = sentinel(CodeChannelPrepayFail)
	ErrOpenIDRequired    = sentinel(CodeOpenIDRequired)

	ErrRefundOrderNotPaid = sentinel(CodeRefundOrderNotPaid)
	ErrRefundExceedTotal  = sentinel(CodeRefundExceedTotal)
	ErrRefundNoConflict   = sentinel(CodeRefundNoConflict)
	ErrRefundChannelFail  = sentinel(CodeRefundChannelFail)
	ErrRefundNotFound     = sentinel(CodeRefundNotFound)
)

// 传输层与验签错误哨兵（不是业务码）。
var (
	// ErrResponseSign 响应验签失败：signature 对不上、或服务端根本没签。
	// 命中它说明回执不可信，不要拿 data 当真。
	ErrResponseSign = errors.New("jjpay: 响应验签失败")
	// ErrNotifySign 通知验签失败。
	ErrNotifySign = errors.New("jjpay: 通知验签失败")
	// ErrReturnSign 回跳参数验签失败：不是 jjpay 送回来的，或被改过。当作没带结果，去查单
	ErrReturnSign = errors.New("jjpay: 回跳参数验签失败")
	// ErrReturnMissing 回跳地址上没有 jjpay 的结果参数（商户没经收银台回跳、或链接被裁过）
	ErrReturnMissing = errors.New("jjpay: 缺少回跳结果参数")
	// ErrTimestampWindow 时间戳超出允许窗口（默认 ±5 分钟）。
	// 只验签不验时间戳挡不住重放——签名是真的，只是被人录下来重放了。
	ErrTimestampWindow = errors.New("jjpay: 时间戳超出允许窗口")
	// ErrMissingHeader 缺少必需的签名头。
	ErrMissingHeader = errors.New("jjpay: 缺少签名头")
	// ErrBadResponse 响应不是合法的 {code,msg,data} 信封。
	ErrBadResponse = errors.New("jjpay: 响应格式非法")
)

// HTTPError 非 200 的 HTTP 响应。
//
// 正常情况下 jjpay 的业务响应恒为 200，所以拿到它基本意味着请求没走到
// 应用层（网关 502、限流 429、路径写错 404 之类）。
type HTTPError struct {
	StatusCode int
	Body       string // 截断后的响应体，便于排查
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("jjpay: HTTP %d: %s", e.StatusCode, oneLine(e.Body, maxMsgRunes))
}

// maxMsgRunes 错误文本里允许保留的最大字符数。Msg / Body 来自对端而错误文本
// 要进日志：超长会刷屏，换行能伪造出额外的日志条目。
const maxMsgRunes = 200

// oneLine 把对端来的文本压成安全的一行：控制字符换成空格，超长截断。
// 不做 HTML/JSON 转义——它的去处是日志，不是页面。
func oneLine(s string, maxRunes int) string {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			// 丢掉，不留痕：ANSI 转义序列能改写终端里已经打出来的内容
		default:
			b.WriteRune(r)
		}
		n++
	}
	return strings.TrimSpace(b.String())
}

// CodeOf 取出错误里的业务码；不是业务错误时返回 0。
func CodeOf(err error) Code {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return 0
}
