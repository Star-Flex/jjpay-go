package jjpay

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// 验签函数吃的是公网上任何人都能构造的字节。这几条模糊测试盯两件事：
//
//  1. 任意输入都不许 panic —— 回调端点 panic 等于把一个 DoS 按钮放在公网上；
//  2. 任意输入都不许被判为"通过" —— 除非签名真的对。
//
// 跑法：go test -fuzz=FuzzVerify -fuzztime=30s

const fuzzSecret = "sk_fuzz_0123456789abcdef"

func FuzzVerifyNotify(f *testing.F) {
	body := []byte(`{"event":"pay.succeeded","trade_no":"P1","out_trade_no":"A1","total_minor":1990}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	good := computeSign(fuzzSecret, notifyPayload("pay.succeeded", ts, "n0123456789abcdef", hashBody(body)))

	f.Add("pay.succeeded", ts, "n0123456789abcdef", good, body)
	f.Add("", "", "", "", []byte(""))
	f.Add("pay.succeeded", "not-a-number", "n", "zz", []byte("{"))
	f.Add("refund.succeeded", "-1", "\x00", good, []byte(`{"event":"pay.succeeded"}`))

	f.Fuzz(func(t *testing.T, event, ts, nonce, sig string, body []byte) {
		h := http.Header{}
		h.Set(HeaderEvent, event)
		h.Set(HeaderTimestamp, ts)
		h.Set(HeaderNonce, nonce)
		h.Set(HeaderSignature, sig)

		evt, err := VerifyWithOptions(h, body, NotifyOptions{
			Secret: fuzzSecret,
			Now:    func() time.Time { return time.Unix(1785000000, 0) },
		})
		if err != nil {
			if evt != nil {
				t.Fatal("出错时不许同时返回事件——调用方拿到非 nil 会当成验过签的")
			}
			return
		}
		// 判为通过，那签名就必须真的对得上。
		want := computeSign(fuzzSecret, notifyPayload(event, ts, nonce, hashBody(body)))
		if !verifySign(fuzzSecret, notifyPayload(event, ts, nonce, hashBody(body)), sig) {
			t.Fatalf("放行了一条签名对不上的通知：sig=%q want=%q", sig, want)
		}
	})
}

func FuzzVerifyReturn(f *testing.F) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	f.Add("P1", "A1", "paid", "1990", "CNY", "", ts, "n0123456789abcdef", "deadbeef")
	f.Add("", "", "", "", "", "", "", "", "")
	f.Add("P1", "A1", "paid", "999999999999999999999", "CNY", "not-a-time", "0", "n", "x")

	f.Fuzz(func(t *testing.T, tradeNo, outTradeNo, status, total, currency, paidAt, ts, nonce, sig string) {
		q := url.Values{
			"trade_no": {tradeNo}, "out_trade_no": {outTradeNo}, "status": {status},
			"total_minor": {total}, "currency": {currency}, "paid_at": {paidAt},
			"timestamp": {ts}, "nonce": {nonce}, "sign": {sig},
		}
		res, err := VerifyReturnWithOptions(q, ReturnOptions{
			Secret: fuzzSecret,
			Now:    func() time.Time { return time.Unix(1785000000, 0) },
		})
		if err != nil {
			if res != nil {
				t.Fatal("出错时不许同时返回结果")
			}
			return
		}
		p := returnPayload(tradeNo, outTradeNo, status, total, currency, paidAt, ts, nonce)
		if !verifySign(fuzzSecret, p, sig) {
			t.Fatal("放行了一条签名对不上的回跳结果")
		}
	})
}
