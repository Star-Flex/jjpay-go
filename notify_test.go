package jjpay

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// notifyBody 末尾那个 payer_ref 是「故意留的」：服务端已经不发它了，这里让它继续
// 出现，是要钉住「通知里来了个 SDK 不认识的字段时，照常解析、签名照常过」——
// 服务端加字段不该把老版本 SDK 打挂。别顺手把它「清理」掉。
const notifyBody = `{"event":"pay.succeeded","trade_no":"P20260802143012K7M2QX9B4T",` +
	`"out_trade_no":"RQ2026080200123","total_minor":1990,"channel":"wechat","method":"native",` +
	`"channel_trade_no":"4200001234202608021234567890","paid_at":"2026-08-02T14:31:05+08:00",` +
	`"attach":"plan_id=7","payer_ref":"u_8842"}`

// notifyHeaders 造一份 jjpay 会发出的通知头。
func notifyHeaders(event, body string, at time.Time, secret string) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	nonce := "3f9a1c2b4d5e6f708192a3b4c5d6e7f8"
	h := http.Header{}
	h.Set(HeaderEvent, event)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderSignature, computeSign(secret, notifyPayload(event, ts, nonce, hashBody([]byte(body)))))
	return h
}

func TestVerify_PaySucceeded(t *testing.T) {
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret)

	evt, err := Verify(h, []byte(notifyBody), testSecret)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if evt.Event != EventPaySucceeded {
		t.Fatalf("event = %q", evt.Event)
	}
	if evt.TradeNo != "P20260802143012K7M2QX9B4T" || evt.OutTradeNo != "RQ2026080200123" {
		t.Fatalf("单号解析不对: %+v", evt)
	}
	if evt.TotalMinor != 1990 {
		t.Fatalf("total_minor = %d", evt.TotalMinor)
	}
	if evt.Channel != ChannelWechat || evt.Method != MethodWechatNative {
		t.Fatalf("channel/method = %s/%s", evt.Channel, evt.Method)
	}
	if evt.ChannelTradeNo == "" || evt.PaidAt.IsZero() {
		t.Fatalf("渠道单号/支付时间没解析出来: %+v", evt)
	}
	if evt.Attach != "plan_id=7" {
		t.Fatalf("透传字段不对: %+v", evt)
	}
	if string(evt.Raw) != notifyBody {
		t.Fatal("Raw 应是原始 body")
	}
	if evt.Nonce == "" || evt.Timestamp.IsZero() {
		t.Fatal("Nonce/Timestamp 应带出来")
	}
}

func TestVerify_RefundSucceeded(t *testing.T) {
	const body = `{"event":"refund.succeeded","refund_no":"R2026","out_refund_no":"RF001",` +
		`"trade_no":"P2026","out_trade_no":"RQ001","amount_minor":500,"refunded_minor":500,` +
		`"total_minor":1990,"finished_at":"2026-08-03T09:20:00+08:00"}`
	h := notifyHeaders("refund.succeeded", body, time.Now(), testSecret)

	evt, err := Verify(h, []byte(body), testSecret)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if evt.Event != EventRefundSucceeded {
		t.Fatalf("event = %q", evt.Event)
	}
	if evt.RefundNo != "R2026" || evt.OutRefundNo != "RF001" {
		t.Fatalf("退款单号不对: %+v", evt)
	}
	if evt.AmountMinor != 500 || evt.RefundedMinor != 500 || evt.TotalMinor != 1990 {
		t.Fatalf("金额不对: %+v", evt)
	}
	if evt.FinishedAt.IsZero() {
		t.Fatal("finished_at 没解析出来")
	}
}

func TestVerify_BadSignature(t *testing.T) {
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret)
	h.Set(HeaderSignature, strings.Repeat("0", 64))

	if _, err := Verify(h, []byte(notifyBody), testSecret); !errors.Is(err, ErrNotifySign) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	// 拿别的 App 的密钥签的通知不能过——否则串号即串账。
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), "别的 app 的密钥")

	if _, err := Verify(h, []byte(notifyBody), testSecret); !errors.Is(err, ErrNotifySign) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_TamperedBody(t *testing.T) {
	// 把金额从 1990 改成 1，签名不变 —— 这正是验签要挡住的攻击。
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret)
	tampered := strings.Replace(notifyBody, `"total_minor":1990`, `"total_minor":1`, 1)
	if tampered == notifyBody {
		t.Fatal("测试数据没改动")
	}

	if _, err := Verify(h, []byte(tampered), testSecret); !errors.Is(err, ErrNotifySign) {
		t.Fatalf("篡改 body 应被拒: %v", err)
	}
}

func TestVerify_EventHeaderSwapped(t *testing.T) {
	// event 进签名，所以把头改成 refund.succeeded 会验签失败。
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret)
	h.Set(HeaderEvent, "refund.succeeded")

	if _, err := Verify(h, []byte(notifyBody), testSecret); !errors.Is(err, ErrNotifySign) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_EventHeaderBodyMismatch(t *testing.T) {
	// 头与 body 各自自洽但互相不一致（签名是按头那份算的）——两端必有一方出错，拒。
	const body = `{"event":"refund.failed","trade_no":"P1"}`
	h := notifyHeaders("pay.succeeded", body, time.Now(), testSecret)

	if _, err := Verify(h, []byte(body), testSecret); !errors.Is(err, ErrNotifySign) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_MissingHeaders(t *testing.T) {
	for _, drop := range []string{HeaderEvent, HeaderTimestamp, HeaderNonce, HeaderSignature} {
		t.Run(drop, func(t *testing.T) {
			h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret)
			h.Del(drop)
			if _, err := Verify(h, []byte(notifyBody), testSecret); !errors.Is(err, ErrMissingHeader) {
				t.Fatalf("缺 %s 应被拒: %v", drop, err)
			}
		})
	}
	// 完全没头（比如有人直接 curl 你的回调端点）。
	if _, err := Verify(http.Header{}, []byte(notifyBody), testSecret); !errors.Is(err, ErrMissingHeader) {
		t.Fatalf("err = %v", err)
	}
}

// TestVerify_TimestampWindow 只验签挡不住重放：签名不会过期，截获一条合法
// 通知无限重放，每次都验签通过。时间戳窗口是划定重放边界的那道闸。
func TestVerify_TimestampWindow(t *testing.T) {
	now := time.Unix(1785000000, 0)
	cases := []struct {
		name  string
		shift time.Duration
		ok    bool
	}{
		{"当前", 0, true},
		{"过去 4 分钟", -4 * time.Minute, true},
		{"未来 4 分钟", 4 * time.Minute, true},
		{"边界 300s", -300 * time.Second, true},
		{"过去 6 分钟", -6 * time.Minute, false},
		// 未来方向同样要卡：客户端时钟快很多时，一个签名会在未来长期有效。
		{"未来 6 分钟", 6 * time.Minute, false},
		{"重放一小时前的", -time.Hour, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := notifyHeaders("pay.succeeded", notifyBody, now.Add(c.shift), testSecret)
			_, err := VerifyWithOptions(h, []byte(notifyBody), NotifyOptions{
				Secret: testSecret,
				Now:    func() time.Time { return now },
			})
			if c.ok && err != nil {
				t.Fatalf("应通过却失败: %v", err)
			}
			if !c.ok && !errors.Is(err, ErrTimestampWindow) {
				t.Fatalf("应因超窗被拒: %v", err)
			}
		})
	}
}

func TestVerify_CustomSkewWindow(t *testing.T) {
	now := time.Unix(1785000000, 0)
	h := notifyHeaders("pay.succeeded", notifyBody, now.Add(-30*time.Minute), testSecret)
	opts := NotifyOptions{Secret: testSecret, SkewWindow: time.Hour, Now: func() time.Time { return now }}
	if _, err := VerifyWithOptions(h, []byte(notifyBody), opts); err != nil {
		t.Fatalf("窗口放宽到 1h 后应通过: %v", err)
	}
}

func TestVerify_BadTimestampFormat(t *testing.T) {
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret)
	h.Set(HeaderTimestamp, "not-a-number")
	if _, err := Verify(h, []byte(notifyBody), testSecret); !errors.Is(err, ErrTimestampWindow) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_EmptySecretRejected(t *testing.T) {
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), "")
	if _, err := Verify(h, []byte(notifyBody), ""); err == nil {
		t.Fatal("空密钥必须报错，不能用空串去验签")
	}
}

// —— 中间件 ——

func TestMiddleware_LegalNotifyReachesHandler(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		evt := EventFrom(r.Context())
		if evt == nil {
			t.Fatal("下游没拿到 Event")
		}
		if evt.Event != EventPaySucceeded || evt.TradeNo == "" {
			t.Fatalf("Event 不对: %+v", evt)
		}
		// body 应被回填，下游还能自己读一遍。
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != notifyBody {
			t.Fatalf("下游读 body：err=%v body=%q", err, b)
		}
		_, _ = io.WriteString(w, "SUCCESS")
	})

	srv := httptest.NewServer(Middleware(testSecret)(next))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(notifyBody))
	for k, v := range notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret) {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !called {
		t.Fatal("合法通知没走到下游")
	}
	if resp.StatusCode != http.StatusOK || string(body) != "SUCCESS" {
		t.Fatalf("应答 = %d %q", resp.StatusCode, body)
	}
}

func TestMiddleware_Rejects401AndNeverCallsNext(t *testing.T) {
	type tc struct {
		name   string
		mutate func(h http.Header)
		body   string
	}
	list := []tc{
		{"签名错", func(h http.Header) { h.Set(HeaderSignature, strings.Repeat("f", 64)) }, notifyBody},
		{"缺签名头", func(h http.Header) { h.Del(HeaderSignature) }, notifyBody},
		{"缺事件头", func(h http.Header) { h.Del(HeaderEvent) }, notifyBody},
		{"缺时间戳", func(h http.Header) { h.Del(HeaderTimestamp) }, notifyBody},
		{"完全裸奔", func(h http.Header) {
			h.Del(HeaderEvent)
			h.Del(HeaderTimestamp)
			h.Del(HeaderNonce)
			h.Del(HeaderSignature)
		}, notifyBody},
		{"body 被改", nil, strings.Replace(notifyBody, `"total_minor":1990`, `"total_minor":1`, 1)},
	}

	for _, c := range list {
		t.Run(c.name, func(t *testing.T) {
			called := false
			var gotErr error
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
			mw := MiddlewareWithOptions(NotifyOptions{
				Secret:  testSecret,
				OnError: func(_ *http.Request, err error) { gotErr = err },
			})

			// 头永远按原始 body 签，用改过的 body 发出去。
			h := notifyHeaders("pay.succeeded", notifyBody, time.Now(), testSecret)
			if c.mutate != nil {
				c.mutate(h)
			}
			r := httptest.NewRequest(http.MethodPost, "/pay/notify", strings.NewReader(c.body))
			for k, v := range h {
				r.Header[k] = v
			}
			w := httptest.NewRecorder()
			mw(next).ServeHTTP(w, r)

			if called {
				t.Fatal("非法通知走到了下游——这就是「任何人都能发已支付」")
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d, want 401", w.Code)
			}
			if gotErr == nil {
				t.Fatal("OnError 没被调用，线上排查会一点线索都没有")
			}
		})
	}
}

func TestMiddleware_TimestampReplayRejected(t *testing.T) {
	// 一条一小时前的合法通知被原样重放：签名是真的，只有时间戳能拦。
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h := notifyHeaders("pay.succeeded", notifyBody, time.Now().Add(-time.Hour), testSecret)

	r := httptest.NewRequest(http.MethodPost, "/pay/notify", strings.NewReader(notifyBody))
	for k, v := range h {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	Middleware(testSecret)(next).ServeHTTP(w, r)

	if called || w.Code != http.StatusUnauthorized {
		t.Fatalf("重放应被拒：called=%v code=%d", called, w.Code)
	}
}

func TestEventFrom_NilWhenAbsent(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if EventFrom(r.Context()) != nil {
		t.Fatal("没走中间件却拿到了 Event")
	}
}
