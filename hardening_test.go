package jjpay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sleepingServer 一台只会拖时间的服务端，用来造超时。
func sleepingServer(t *testing.T, d time.Duration, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(d)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPerAttemptTimeoutIsRetried 单次尝试超时必须重试。
//
// SDK 给每次尝试套了自己的超时，那个超时到期同样是
// context.DeadlineExceeded，而 retryable 见到它就返回 false——于是
// 超时这个最该重试的情况一次都不重试，文档却写着读操作会重试两次。
func TestPerAttemptTimeoutIsRetried(t *testing.T) {
	var hits atomic.Int32
	srv := sleepingServer(t, 150*time.Millisecond, &hits)

	c, err := New(Config{BaseURL: srv.URL, AppID: "a", Secret: testSecret, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil } // 别真等退避

	if _, err := c.QueryOrder(context.Background(), "P1"); err == nil {
		t.Fatal("应当以超时失败")
	}
	if got := hits.Load(); got != 1+DefaultReadRetries {
		t.Fatalf("实际请求 %d 次，期望 %d 次（一次首发 + %d 次重试）",
			got, 1+DefaultReadRetries, DefaultReadRetries)
	}
}

// TestCallerCancelStopsImmediately 调用方自己取消，一次都不该再试。
func TestCallerCancelStopsImmediately(t *testing.T) {
	var hits atomic.Int32
	srv := sleepingServer(t, 500*time.Millisecond, &hits)

	c, err := New(Config{BaseURL: srv.URL, AppID: "a", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if _, err := c.QueryOrder(ctx, "P1"); err == nil {
		t.Fatal("应当失败")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("调用方取消后还试了 %d 次，应当只有 1 次", got)
	}
}

// TestSuccessWithoutDataIsRejected 成功响应必须真的带 data。
//
// 放过的话调用方拿到的是零值结构体 + err == nil：
// 金额 0、状态空串，看起来像一笔没付钱的单，而实际上回执根本没说。
func TestSuccessWithoutDataIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]any
	}{
		{"没有 data 字段", map[string]any{"code": CodeSuccess, "msg": "ok"}},
		{"data 是 null", map[string]any{"code": CodeSuccess, "msg": "ok", "data": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, nil)
			fs.respBody = func(*http.Request) any { return tc.env }
			c := mustClient(t, fs.URL)

			_, err := c.QueryOrder(context.Background(), "P1")
			if !errors.Is(err, ErrBadResponse) {
				t.Fatalf("期望 ErrBadResponse，得到 %v", err)
			}
		})
	}
}

// TestReceiptMustBeTheOneWeAsked 回执说的必须是本次请求的那一笔。
//
// 应答签名的待签串里没有 method/path，所以一条合法回执理论上可以被搬到
// 另一次请求上当答复。SDK 手里有"我问的是哪一笔"，这一层该由它兜住。
func TestReceiptMustBeTheOneWeAsked(t *testing.T) {
	t.Run("按 trade_no 查单", func(t *testing.T) {
		fs := newFakeServer(t, map[string]any{"trade_no": "P_OTHER", "out_trade_no": "A1"})
		c := mustClient(t, fs.URL)
		_, err := c.QueryOrder(context.Background(), "P_MINE")
		if !errors.Is(err, ErrBadResponse) {
			t.Fatalf("期望 ErrBadResponse，得到 %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "P_MINE") {
			t.Fatalf("错误里该写明我问的是哪一笔：%v", err)
		}
	})

	t.Run("按 out_trade_no 查单", func(t *testing.T) {
		fs := newFakeServer(t, map[string]any{"trade_no": "P1", "out_trade_no": "A_OTHER"})
		c := mustClient(t, fs.URL)
		if _, err := c.QueryOrderByOutTradeNo(context.Background(), "A_MINE"); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("期望 ErrBadResponse，得到 %v", err)
		}
	})

	t.Run("下单", func(t *testing.T) {
		fs := newFakeServer(t, map[string]any{"trade_no": "P1", "out_trade_no": "A_OTHER", "checkout_url": "https://pay.example.com/pay/x"})
		c := mustClient(t, fs.URL)
		if _, err := c.CreateOrder(context.Background(), CreateOrderReq{OutTradeNo: "A_MINE", Subject: "x", TotalMinor: 1}); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("期望 ErrBadResponse，得到 %v", err)
		}
	})

	t.Run("退款", func(t *testing.T) {
		fs := newFakeServer(t, map[string]any{"refund_no": "R1", "out_refund_no": "OR_OTHER"})
		c := mustClient(t, fs.URL)
		if _, err := c.Refund(context.Background(), RefundReq{OutTradeNo: "A1", OutRefundNo: "OR_MINE", AmountMinor: 1}); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("期望 ErrBadResponse，得到 %v", err)
		}
	})

	t.Run("查退款", func(t *testing.T) {
		fs := newFakeServer(t, map[string]any{"refund_no": "R_OTHER"})
		c := mustClient(t, fs.URL)
		if _, err := c.QueryRefund(context.Background(), "R_MINE"); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("期望 ErrBadResponse，得到 %v", err)
		}
	})

	t.Run("对得上就放行", func(t *testing.T) {
		fs := newFakeServer(t, map[string]any{"trade_no": "P_MINE", "out_trade_no": "A1", "status": "paid"})
		c := mustClient(t, fs.URL)
		o, err := c.QueryOrder(context.Background(), "P_MINE")
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if !o.Paid() {
			t.Fatal("状态没解出来")
		}
	})
}

// TestEscapedBasePathSignsWhatItSends BaseURL 带转义前缀时，签的必须是发出去的那串字节。
//
// fakeServer 按 r.URL.RequestURI() 验签，签错了它会直接 t.Errorf——
// 也就是说这个用例的断言实际在服务端那侧。
func TestEscapedBasePathSignsWhatItSends(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"trade_no": "P1"})
	c, err := New(Config{BaseURL: fs.URL + "/a%20b", AppID: "a", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryOrder(context.Background(), "P1"); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if got := fs.last.Load().Path; got != "/a%20b/openapi/v1/orders/P1" {
		t.Fatalf("发出去的路径 = %q", got)
	}
}

// TestBaseURLRejectsQueryAndFragment 带 query / fragment 的 BaseURL 构造期就该拒。
func TestBaseURLRejectsQueryAndFragment(t *testing.T) {
	for _, u := range []string{
		"https://pay.example.com/?a=1",
		"https://pay.example.com/#frag",
		"https://pay.example.com/?",
	} {
		if _, err := New(Config{BaseURL: u, AppID: "a", Secret: "s"}); err == nil {
			t.Fatalf("%q 应当被拒", u)
		}
	}
}

// TestRedirectIsRefusedAndLocationRedacted 不跟重定向，且跳转地址的 query 不进错误。
//
// 待签串不含 host，而自定义头会跟着 302 原样转发——对方白拿一条窗口期内
// 可重放的合法签名请求。Location 的 query 里常挂一次性 token，而错误文本要进日志。
func TestRedirectIsRefusedAndLocationRedacted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "https://evil.example.com/steal?token=SECRET-TOKEN-HERE")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := mustClient(t, srv.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := c.QueryOrder(context.Background(), "P1")
	if err == nil {
		t.Fatal("应当失败")
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusFound {
		t.Fatalf("期望 302 的 HTTPError，得到 %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN-HERE") {
		t.Fatalf("跳转地址的 query 进了错误文本：%s", err)
	}
	if !strings.Contains(err.Error(), "evil.example.com") {
		t.Fatalf("host 该留着，否则查不出是谁在跳：%s", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("3xx 不该重试，实际请求 %d 次", hits.Load())
	}
}

// TestUnsignedAuthErrorDoesNotEchoServerText 未签名的鉴权响应里那句话不许进错误文本。
//
// 那条响应整个没验过签，msg 是对端随便写的；而错误文本的去处是接入方的日志。
func TestUnsignedAuthErrorDoesNotEchoServerText(t *testing.T) {
	evil := "正常\n2026-09-17 ERROR 伪造的一行日志\x1b[2K"
	fs := newFakeServer(t, nil)
	fs.unsigned = true
	fs.code = CodeSignMismatch
	fs.msg = evil
	c := mustClient(t, fs.URL)

	_, err := c.QueryOrder(context.Background(), "P1")
	if !errors.Is(err, ErrSignMismatch) {
		t.Fatalf("鉴权段错误码该透出来，得到 %v", err)
	}
	if strings.Contains(err.Error(), "伪造的一行日志") {
		t.Fatalf("未签名的文案进了错误：%q", err.Error())
	}
	if strings.ContainsAny(err.Error(), "\n\r\x1b") {
		t.Fatalf("错误文本里有控制字符：%q", err.Error())
	}
}

// TestErrorTextIsOneBoundedLine 已签名的响应文案同样要压成安全的一行。
func TestErrorTextIsOneBoundedLine(t *testing.T) {
	long := strings.Repeat("啊", 5000)
	ae := &APIError{Code: CodeParamErr, Msg: "第一行\n第二行\ttab\x00\x1b[31m" + long}
	got := ae.Error()
	if strings.ContainsAny(got, "\n\r\x00\x1b") {
		t.Fatalf("控制字符没清干净：%q", got)
	}
	if n := len([]rune(got)); n > maxMsgRunes+60 {
		t.Fatalf("错误文本 %d 字，没截断", n)
	}

	he := &HTTPError{StatusCode: 502, Body: "bad\ngateway\n" + long}
	if strings.Contains(he.Error(), "\n") {
		t.Fatalf("HTTPError 没压成一行：%q", he.Error())
	}
}

// TestReadRetriesAndBackoffAreCapped 重试次数与退避都要封顶。
//
// 退避是 200ms 左移，次数一大 time.Duration 就溢出成负数：不等待、瞬间把
// 剩下的次数烧完。封顶比"相信没人会传 100"便宜。
func TestReadRetriesAndBackoffAreCapped(t *testing.T) {
	c, err := New(Config{BaseURL: "https://pay.example.com", AppID: "a", Secret: "s", ReadRetries: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if c.retries != MaxReadRetries {
		t.Fatalf("重试次数 = %d，应当被压到 %d", c.retries, MaxReadRetries)
	}
	for i := 0; i <= 64; i++ {
		d := backoff(i)
		if d < 0 || d > maxBackoff {
			t.Fatalf("backoff(%d) = %s，越界", i, d)
		}
	}
	if backoff(0) != 0 {
		t.Fatalf("首发前不该等，backoff(0) = %s", backoff(0))
	}
	if backoff(1) != 200*time.Millisecond {
		t.Fatalf("backoff(1) = %s，期望 200ms", backoff(1))
	}
}

// TestBackoffRespectsContext 退避等待期间调用方取消，立刻返回。
func TestBackoffRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled，得到 %v", err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("正常等待不该出错：%v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://h.example.com/a/b?token=x#frag", "https://h.example.com/a/b?(已略去)"},
		{"https://h.example.com/a/b", "https://h.example.com/a/b"},
		{"::not a url::", "(无法解析的地址，已略去)"},
		{"/relative/only", "(无法解析的地址，已略去)"},
	} {
		if got := redactURL(tc.in); got != tc.want {
			t.Fatalf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTruncateKeepsRuneBoundary 截断不许把一个字切成两半。
func TestTruncateKeepsRuneBoundary(t *testing.T) {
	s := strings.Repeat("中", 10) // 每个 3 字节
	for n := 1; n < 30; n++ {
		got := truncate(s, n)
		if !strings.ContainsRune(got, '…') && len(s) > n {
			t.Fatalf("truncate(_, %d) 没加省略号: %q", n, got)
		}
		body := strings.TrimSuffix(got, "…")
		if strings.ContainsRune(body, '�') {
			t.Fatalf("truncate(_, %d) 切出了半个字: %q", n, got)
		}
	}
	if truncate("abc", 10) != "abc" {
		t.Fatal("没超长不该动")
	}
}

func mustClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: base, AppID: "app_k7m2qx9b4t", Secret: testSecret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ───────────────────── Client 侧便捷入口 ─────────────────────

// TestClientNotifyEntrypoints (*Client).Verify / Middleware 用的是 Client 自己的凭据。
//
// 这几个入口存在的意义就是"不给接入方第二次读密钥的机会"——自由函数版本要求
// 在 handler 那侧再读一遍环境变量，读空的后果是每条真实通知都被 401。
func TestClientNotifyEntrypoints(t *testing.T) {
	c := mustClient(t, "https://pay.example.com")
	body := []byte(`{"event":"pay.succeeded","trade_no":"P1","out_trade_no":"A1","total_minor":1990}`)
	h, err := SignNotify(testSecret, EventPaySucceeded, body, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Verify", func(t *testing.T) {
		evt, err := c.Verify(h, body)
		if err != nil {
			t.Fatalf("验签应通过: %v", err)
		}
		if evt.TradeNo != "P1" || evt.TotalMinor != 1990 {
			t.Fatalf("解出来不对: %+v", evt)
		}
	})

	t.Run("Middleware 放行并把事件放进 context", func(t *testing.T) {
		var seen *Event
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = EventFrom(r.Context())
			// 下游还能正常读 body
			if b, _ := readAll(r); string(b) != string(body) {
				t.Fatalf("body 没回填")
			}
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/pay/notify", strings.NewReader(string(body)))
		req.Header = h.Clone()
		c.Middleware()(next).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("应当放行，得到 %d", rec.Code)
		}
		if seen == nil || seen.OutTradeNo != "A1" {
			t.Fatalf("context 里没有事件: %+v", seen)
		}
	})

	t.Run("MiddlewareWithOptions 的 OnError 会被调到", func(t *testing.T) {
		var gotErr error
		mw := c.MiddlewareWithOptions(NotifyOptions{
			OnError: func(r *http.Request, err error) { gotErr = err },
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/pay/notify", strings.NewReader(string(body)))
		req.Header = h.Clone()
		req.Header.Set(HeaderSignature, strings.Repeat("0", 64)) // 签名改坏

		called := false
		mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(rec, req)

		if called {
			t.Fatal("验签没过，下游一次都不该被调用")
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("应当 401，得到 %d", rec.Code)
		}
		if gotErr == nil {
			t.Fatal("OnError 没被调到——这正是默认 nil 时线上毫无线索的那个洞")
		}
	})
}

// TestClientVerifyReturn (*Client).VerifyReturn 用 Client 自己的凭据。
func TestClientVerifyReturn(t *testing.T) {
	c := mustClient(t, "https://pay.example.com")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	const nonce = "3f9a1c2b4d5e6f708192a3b4c5d6e7f8"
	q := url.Values{
		"trade_no": {"P1"}, "out_trade_no": {"A1"}, "status": {"paid"},
		"total_minor": {"1990"}, "currency": {"CNY"}, "paid_at": {},
		"timestamp": {ts}, "nonce": {nonce},
	}
	q.Set("sign", computeSign(testSecret,
		returnPayload("P1", "A1", "paid", "1990", "CNY", "", ts, nonce)))

	res, err := c.VerifyReturn(q)
	if err != nil {
		t.Fatalf("验签应通过: %v", err)
	}
	if !res.Paid() || res.TotalMinor != 1990 {
		t.Fatalf("解出来不对: %+v", res)
	}

	q.Set("sign", strings.Repeat("0", 64))
	if _, err := c.VerifyReturn(q); !errors.Is(err, ErrReturnSign) {
		t.Fatalf("改过签名应当被拒，得到 %v", err)
	}
}

// TestMiddlewarePanicsOnEmptySecret 空 secret 必须在装配期炸。
//
// 放行的话线上表现是每一条真实通知都被 401、24 小时后进死信，而商户侧零线索。
// 宁可起不来。
func TestMiddlewarePanicsOnEmptySecret(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("空 secret 应当 panic")
		}
		if !strings.Contains(r.(string), "JJPAY_APP_SECRET") {
			t.Fatalf("panic 文案该指出去哪儿查：%v", r)
		}
	}()
	Middleware("")
}

func TestSignNotifyRejectsEmptySecret(t *testing.T) {
	if _, err := SignNotify("", EventPaySucceeded, nil, time.Now()); err == nil {
		t.Fatal("空 secret 应当报错")
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

// TestBusyAndRateLimitedAreRetried 限流与系统繁忙走的是业务码，不是 HTTP 429。
//
// jjpay 的业务响应 HTTP 恒为 200，只认 429 的话服务端自己的限流一次都不会被重试。
func TestBusyAndRateLimitedAreRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		code Code
		want bool
	}{
		{"系统繁忙", CodeServerBusy, true},
		{"限流", CodeRateLimited, true},
		{"单号冲突", CodeOrderConflict, false},
		{"参数错误", CodeParamErr, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, nil)
			fs.code = tc.code
			fs.msg = "x"
			c := mustClient(t, fs.URL)
			c.sleep = func(context.Context, time.Duration) error { return nil }

			if _, err := c.QueryOrder(context.Background(), "P1"); err == nil {
				t.Fatal("应当失败")
			}
			want := int32(1)
			if tc.want {
				want = 1 + DefaultReadRetries
			}
			if got := fs.hits.Load(); got != want {
				t.Fatalf("实际请求 %d 次，期望 %d 次", got, want)
			}
		})
	}
}

// TestRetryableOnTransportAndServerErrors 5xx 与 429 重试、4xx 不重试。
func TestRetryableOnTransportAndServerErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{{500, true}, {503, true}, {429, true}, {404, false}, {401, false}} {
		if got := retryable(&HTTPError{StatusCode: tc.status}); got != tc.want {
			t.Fatalf("HTTP %d 的可重试性 = %v，期望 %v", tc.status, got, tc.want)
		}
	}
	if retryable(errors.New("boom")) != true {
		t.Fatal("认不出来的错误按传输层算，应当可重试")
	}
	if retryable(context.Canceled) {
		t.Fatal("调用方取消不该重试")
	}
}

// TestUnsignedAuthErrorOnlyForAuthCodes 未签名响应的口子只开给鉴权段。
//
// 放进来的字节完全没验过签，只因为里面除了一个鉴权错误码没有任何值钱的东西
// 才被容忍。别的码一律按"响应验签失败"处理。
func TestUnsignedAuthErrorOnlyForAuthCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		code Code
	}{
		{"业务段不放行", CodeOrderConflict},
		{"通用段不放行", CodeParamErr},
		{"鉴权段之后的号段不放行", Code(20100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, nil)
			fs.unsigned = true
			fs.code = tc.code
			fs.msg = "x"
			c := mustClient(t, fs.URL)
			c.sleep = func(context.Context, time.Duration) error { return nil }

			_, err := c.QueryOrder(context.Background(), "P1")
			if !errors.Is(err, ErrResponseSign) {
				t.Fatalf("期望 ErrResponseSign，得到 %v", err)
			}
		})
	}

	t.Run("body 不是 JSON 也不放行", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		}))
		t.Cleanup(srv.Close)
		c := mustClient(t, srv.URL)
		c.sleep = func(context.Context, time.Duration) error { return nil }
		if _, err := c.QueryOrder(context.Background(), "P1"); !errors.Is(err, ErrResponseSign) {
			t.Fatalf("期望 ErrResponseSign，得到 %v", err)
		}
	})
}

// TestNotifyBodyTooLargeIsRejected 通知体有读取上限，不设闸等于把撑爆进程的按钮放在公网上。
func TestNotifyBodyTooLargeIsRejected(t *testing.T) {
	var gotErr error
	mw := MiddlewareWithOptions(NotifyOptions{
		Secret:  testSecret,
		OnError: func(r *http.Request, err error) { gotErr = err },
	})
	huge := strings.Repeat("x", maxNotifyBytes+1)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/pay/notify", strings.NewReader(huge))

	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("超长的 body 不该走到下游")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || gotErr == nil {
		t.Fatalf("应当被拒并报错，code=%d err=%v", rec.Code, gotErr)
	}
}

func TestQueryRefundByOutRefundNo(t *testing.T) {
	if _, err := mustClient(t, "https://pay.example.com").
		QueryRefundByOutRefundNo(context.Background(), "  "); err == nil {
		t.Fatal("空单号应当被拒")
	}
	fs := newFakeServer(t, map[string]any{"refund_no": "R1", "out_refund_no": "OR1", "amount_minor": 500})
	c := mustClient(t, fs.URL)
	r, err := c.QueryRefundByOutRefundNo(context.Background(), "OR1")
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if r.AmountMinor != 500 {
		t.Fatalf("解出来不对: %+v", r)
	}
}

func TestCodeOf(t *testing.T) {
	if got := CodeOf(&APIError{Code: CodeOrderConflict}); got != CodeOrderConflict {
		t.Fatalf("CodeOf = %d", got)
	}
	if got := CodeOf(errors.New("boom")); got != 0 {
		t.Fatalf("非业务错误应当返回 0，得到 %d", got)
	}
	if got := CodeOf(nil); got != 0 {
		t.Fatalf("nil 应当返回 0，得到 %d", got)
	}
}

func TestQueryRefundByOutRefundNoChecksReceipt(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"refund_no": "R1", "out_refund_no": "OR_OTHER"})
	c := mustClient(t, fs.URL)
	if _, err := c.QueryRefundByOutRefundNo(context.Background(), "OR_MINE"); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("期望 ErrBadResponse，得到 %v", err)
	}
}

func TestVerifyReturnRejectsIncompleteQuery(t *testing.T) {
	q := url.Values{"trade_no": {"P1"}} // 缺 status/total_minor/timestamp/nonce/sign
	if _, err := VerifyReturn(q, testSecret); !errors.Is(err, ErrReturnMissing) {
		t.Fatalf("期望 ErrReturnMissing，得到 %v", err)
	}
	if _, err := VerifyReturn(url.Values{}, ""); err == nil {
		t.Fatal("空 secret 应当被拒")
	}
}

func TestCodeTextFallsBackForUnknownCode(t *testing.T) {
	ae := &APIError{Code: Code(99999)}
	if got := ae.Error(); !strings.Contains(got, "99999") {
		t.Fatalf("没登记过的码也要能打出来：%q", got)
	}
}

// TestLoggerSeesRetries 重试是唯一会被静默吞掉的事件：一次成功的调用背后
// 可能藏着两次失败。传了 Logger 就该看得见。
func TestLoggerSeesRetries(t *testing.T) {
	var lines []string
	fs := newFakeServer(t, nil)
	fs.respStatus = http.StatusBadGateway
	c, err := New(Config{
		BaseURL: fs.URL, AppID: "a", Secret: testSecret,
		Logger: loggerFunc(func(format string, v ...any) {
			lines = append(lines, fmt.Sprintf(format, v...))
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }

	_, _ = c.QueryOrder(context.Background(), "P1")
	if len(lines) != DefaultReadRetries {
		t.Fatalf("重试了 %d 次却打了 %d 行: %v", DefaultReadRetries, len(lines), lines)
	}
	for _, l := range lines {
		if strings.Contains(l, testSecret) {
			t.Fatalf("日志里出现了密钥: %s", l)
		}
	}
}

// TestNoLoggerMeansSilent 不传 Logger 就什么都不打，也不写 stdout/stderr。
func TestNoLoggerMeansSilent(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.respStatus = http.StatusBadGateway
	c := mustClient(t, fs.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	if c.log != nil {
		t.Fatal("默认不该有 logger")
	}
	_, _ = c.QueryOrder(context.Background(), "P1")
}

type loggerFunc func(format string, v ...any)

func (f loggerFunc) Printf(format string, v ...any) { f(format, v...) }
