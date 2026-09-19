package jjpay

import (
	"context"
	"encoding/json"
	"errors"
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

const testSecret = "sk_test_0123456789abcdef"

// recorded 记录服务端实际收到了什么，供断言。
type recorded struct {
	Method string
	Path   string // r.URL.RequestURI()，即签名里的 PATH
	Body   string
	AppID  string
}

// fakeServer 是一台按规范验签、按规范签响应的假 jjpay。
// SDK 签得对不对由它说了算，而不是 SDK 自己跟自己对。
type fakeServer struct {
	*httptest.Server
	t    *testing.T
	hits atomic.Int32
	last atomic.Pointer[recorded]

	// 下面几个旋钮用来构造异常场景。
	respStatus  int                       // 非 0 时按它回 HTTP 状态
	respBody    func(r *http.Request) any // 返回信封结构（未设时回 code=10000 + data）
	data        any                       // 默认 data
	code        Code                      // 默认 10000
	msg         string
	unsigned    bool          // 不给响应签名
	tamper      bool          // 签完再改 body
	respTSShift time.Duration // 响应时间戳偏移
	quiet       bool          // 服务端验签失败是预期的，别报错
}

// errorf 服务端侧的断言失败。quiet 时静默（用于"客户端密钥就是错的"这类用例）。
func (fs *fakeServer) errorf(format string, args ...any) {
	if fs.quiet {
		return
	}
	fs.t.Errorf(format, args...)
}

func newFakeServer(t *testing.T, data any) *fakeServer {
	t.Helper()
	return newFakeServerBehindPrefix(t, data, "")
}

// newFakeServerBehindPrefix prefix 非空时这台就是「剥前缀的网关」：
// 带前缀的请求剥掉前缀再交给应用，不带前缀的 404（线上那条规则只回源 /api/*）。
//
// 【为什么假服务端必须长这样】它按 r.URL.RequestURI() 验签，也就是"应用看到什么就验什么"。
// 不剥前缀的话它其实是在配合 SDK：SDK 签什么它就验什么，两边一起错也测不出来。
func newFakeServerBehindPrefix(t *testing.T, data any, prefix string) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t, data: data, code: CodeSuccess, msg: "success"}
	var h http.Handler = http.HandlerFunc(fs.handle)
	if prefix != "" {
		h = stripEscapedPrefix(prefix, h)
	}
	fs.Server = httptest.NewServer(h)
	t.Cleanup(fs.Close)
	return fs
}

// stripEscapedPrefix 按「线路上的那串字节」剥前缀，标准库的 http.StripPrefix 不行：
// 它比的是 r.URL.Path（已解码），前缀写成 "/a%20b" 时匹配不上解码后的 "/a b"，
// 一律 404。网关剥的是请求行上的字节，这里照做。
func stripEscapedPrefix(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		esc := r.URL.EscapedPath()
		if !strings.HasPrefix(esc, prefix) {
			http.NotFound(w, r)
			return
		}
		rest, err := url.Parse(esc[len(prefix):])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		r2 := *r
		u := *r.URL
		u.Path, u.RawPath = rest.Path, rest.RawPath
		r2.URL = &u
		h.ServeHTTP(w, &r2)
	})
}

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	fs.hits.Add(1)
	body, _ := io.ReadAll(r.Body)
	fs.last.Store(&recorded{
		Method: r.Method,
		Path:   r.URL.RequestURI(),
		Body:   string(body),
		AppID:  r.Header.Get(HeaderAppID),
	})

	// —— 服务端侧验签 ——
	ts := r.Header.Get(HeaderTimestamp)
	nonce := r.Header.Get(HeaderNonce)
	sig := r.Header.Get(HeaderSignature)
	if ts == "" || nonce == "" || sig == "" || r.Header.Get(HeaderAppID) == "" {
		fs.errorf("请求缺签名头: ts=%q nonce=%q sig=%q appid=%q", ts, nonce, sig, r.Header.Get(HeaderAppID))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(nonce) < 16 || len(nonce) > 64 {
		fs.errorf("nonce 长度 %d 不在 16~64", len(nonce))
	}
	if sec, err := strconv.ParseInt(ts, 10, 64); err != nil {
		fs.errorf("时间戳不是 Unix 秒: %q", ts)
	} else if d := time.Since(time.Unix(sec, 0)); d > time.Minute || d < -time.Minute {
		fs.errorf("时间戳偏差过大: %s", d)
	}
	want := requestPayload(r.Method, r.URL.RequestURI(), ts, nonce, hashBody(body))
	if !verifySign(testSecret, want, sig) {
		fs.errorf("服务端验签失败，待签串=%q", want)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if fs.respStatus != 0 {
		http.Error(w, "boom", fs.respStatus)
		return
	}

	var env any
	if fs.respBody != nil {
		env = fs.respBody(r)
	} else {
		env = map[string]any{"code": fs.code, "msg": fs.msg, "data": fs.data}
	}
	out, err := json.Marshal(env)
	if err != nil {
		fs.t.Fatalf("序列化响应失败: %v", err)
	}

	if !fs.unsigned {
		rts := strconv.FormatInt(time.Now().Add(fs.respTSShift).Unix(), 10)
		rnonce := "resp-nonce-0123456789"
		w.Header().Set(HeaderTimestamp, rts)
		w.Header().Set(HeaderNonce, rnonce)
		w.Header().Set(HeaderSignature, computeSign(testSecret, responsePayload(rts, rnonce, hashBody(out))))
	}
	if fs.tamper {
		out = append(out, ' ') // 签完再动一个字节
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func newTestClient(t *testing.T, fs *fakeServer) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: fs.URL, AppID: "app_k7m2qx9b4t", Secret: testSecret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil } // 测试不真等退避
	return c
}

func TestNew_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"缺 BaseURL", Config{AppID: "a", Secret: "s"}},
		{"缺 AppID", Config{BaseURL: "https://x.example", Secret: "s"}},
		{"缺 Secret", Config{BaseURL: "https://x.example", AppID: "a"}},
		{"BaseURL 不是绝对地址", Config{BaseURL: "/openapi", AppID: "a", Secret: "s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil {
				t.Fatal("配置不合法却构造成功了")
			}
		})
	}
	if _, err := New(Config{BaseURL: "https://pay.example.com/", AppID: "a", Secret: "s"}); err != nil {
		t.Fatalf("合法配置构造失败: %v", err)
	}
}

func TestCreateOrder(t *testing.T) {
	fs := newFakeServer(t, map[string]any{
		"trade_no":     "P20260802143012K7M2QX9B4T",
		"out_trade_no": "RQ2026080200123",
		"checkout_url": "https://pay.example.com/pay/tk_9f3a",
		"expire_at":    "2026-08-02T14:30:12+08:00",
		"status":       "pending",
	})
	c := newTestClient(t, fs)

	resp, err := c.CreateOrder(context.Background(), CreateOrderReq{
		OutTradeNo: "RQ2026080200123",
		Subject:    "测试商品 · 基础版 包月",
		TotalMinor: 1990,
		Attach:     "plan_id=7",
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if resp.TradeNo != "P20260802143012K7M2QX9B4T" || resp.CheckoutURL == "" {
		t.Fatalf("响应解析不对: %+v", resp)
	}
	if resp.Status != OrderPending {
		t.Fatalf("status = %s, want %s", resp.Status, OrderPending)
	}
	if resp.ExpireAt.IsZero() {
		t.Fatal("expire_at 没解析出来")
	}

	got := fs.last.Load()
	if got.Method != "POST" || got.Path != "/openapi/v1/orders" {
		t.Fatalf("请求行不对: %s %s", got.Method, got.Path)
	}
	if got.AppID != "app_k7m2qx9b4t" {
		t.Fatalf("AppId 头 = %q", got.AppID)
	}
	// 逐字段对齐 ：必填字段在，未填的可选字段不出现。
	var sent map[string]any
	if err := json.Unmarshal([]byte(got.Body), &sent); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	for _, k := range []string{"out_trade_no", "subject", "total_minor", "attach"} {
		if _, ok := sent[k]; !ok {
			t.Fatalf("请求体缺字段 %s: %s", k, got.Body)
		}
	}
	if sent["total_minor"] != float64(1990) {
		t.Fatalf("total_minor = %v", sent["total_minor"])
	}
	for _, k := range []string{"description", "items", "expire_minutes", "notify_url", "return_url"} {
		if _, ok := sent[k]; ok {
			t.Fatalf("没填的可选字段 %s 不该出现在请求体里: %s", k, got.Body)
		}
	}
}

func TestQueryOrder_ByTradeNo(t *testing.T) {
	fs := newFakeServer(t, map[string]any{
		"trade_no": "P2026", "out_trade_no": "RQ001",
		"subject": "测试", "total_minor": 1990, "refunded_minor": 500,
		"status": "paid", "status_text": "已支付",
		"channel": "wechat", "method": "native",
		"channel_trade_no": "4200001234202608021234567890",
		"paid_at":          "2026-08-02T14:31:05+08:00",
		"expire_at":        "2026-08-02T15:00:12+08:00",
		"closed_at":        nil,
		"attach":           "plan_id=7",
		"created_at":       "2026-08-02T14:30:12+08:00",
	})
	c := newTestClient(t, fs)

	o, err := c.QueryOrder(context.Background(), "P2026")
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if !o.Paid() || o.TotalMinor != 1990 || o.RefundedMinor != 500 {
		t.Fatalf("解析不对: %+v", o)
	}
	if o.Channel != ChannelWechat || o.Method != MethodWechatNative {
		t.Fatalf("channel/method = %s/%s", o.Channel, o.Method)
	}
	if o.PaidAt.IsZero() {
		t.Fatal("paid_at 没解析出来")
	}
	if !o.ClosedAt.IsZero() {
		t.Fatal("closed_at 是 null，应保持零值")
	}
	if p := fs.last.Load().Path; p != "/openapi/v1/orders/P2026" {
		t.Fatalf("path = %s", p)
	}
	if b := fs.last.Load().Body; b != "" {
		t.Fatalf("GET 不该带 body，得到 %q", b)
	}
}

func TestQueryOrderByOutTradeNo_QueryInPath(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"trade_no": "P2026", "out_trade_no": "RQ 001/x", "status": "pending"})
	c := newTestClient(t, fs)

	if _, err := c.QueryOrderByOutTradeNo(context.Background(), "RQ 001/x"); err != nil {
		t.Fatalf("QueryOrderByOutTradeNo: %v", err)
	}
	// query 必须在签名的 PATH 里（服务端验签已经在 handle 里替我们断言了），
	// 这里再钉一下编码形态。
	if p := fs.last.Load().Path; p != "/openapi/v1/orders?out_trade_no=RQ+001%2Fx" {
		t.Fatalf("path = %s", p)
	}
}

func TestCloseOrder(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"trade_no": "P2026", "status": "closed"})
	c := newTestClient(t, fs)

	resp, err := c.CloseOrder(context.Background(), "P2026")
	if err != nil {
		t.Fatalf("CloseOrder: %v", err)
	}
	if resp.Status != OrderClosed {
		t.Fatalf("status = %s", resp.Status)
	}
	got := fs.last.Load()
	if got.Method != "POST" || got.Path != "/openapi/v1/orders/P2026/close" {
		t.Fatalf("请求行不对: %s %s", got.Method, got.Path)
	}
	if got.Body != "" {
		t.Fatalf("关单不该带 body（空 body 的哈希参与签名），得到 %q", got.Body)
	}
}

func TestRefund(t *testing.T) {
	fs := newFakeServer(t, map[string]any{
		"refund_no": "R20260803091500B6VK2XD9NM", "out_refund_no": "RF20260803001",
		"trade_no": "P2026", "amount_minor": 500, "status": 1, "status_text": "处理中",
	})
	c := newTestClient(t, fs)

	r, err := c.Refund(context.Background(), RefundReq{
		OutTradeNo: "RQ2026080200123", OutRefundNo: "RF20260803001", AmountMinor: 500, Reason: "用户取消",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if r.Status != RefundProcessing || r.AmountMinor != 500 {
		t.Fatalf("解析不对: %+v", r)
	}
	got := fs.last.Load()
	if got.Path != "/openapi/v1/refunds" || got.Method != "POST" {
		t.Fatalf("请求行不对: %s %s", got.Method, got.Path)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(got.Body), &sent)
	if _, ok := sent["trade_no"]; ok {
		t.Fatalf("没填的 trade_no 不该出现: %s", got.Body)
	}
	if sent["out_refund_no"] != "RF20260803001" || sent["amount_minor"] != float64(500) {
		t.Fatalf("请求体不对: %s", got.Body)
	}
}

func TestQueryRefund(t *testing.T) {
	fs := newFakeServer(t, map[string]any{
		"refund_no": "R2026", "out_refund_no": "RF001", "trade_no": "P2026",
		"amount_minor": 500, "status": 3, "status_text": "失败",
		"channel_refund_no": "50300807092026080300", "fail_reason": "余额不足",
		"finished_at": "2026-08-03T09:20:00+08:00",
	})
	c := newTestClient(t, fs)

	r, err := c.QueryRefundByOutRefundNo(context.Background(), "RF001")
	if err != nil {
		t.Fatalf("QueryRefundByOutRefundNo: %v", err)
	}
	if r.Status != RefundFailed || r.FailReason != "余额不足" || r.FinishedAt.IsZero() {
		t.Fatalf("解析不对: %+v", r)
	}
	if p := fs.last.Load().Path; p != "/openapi/v1/refunds?out_refund_no=RF001" {
		t.Fatalf("path = %s", p)
	}

	if _, err := c.QueryRefund(context.Background(), "R2026"); err != nil {
		t.Fatalf("QueryRefund: %v", err)
	}
	if p := fs.last.Load().Path; p != "/openapi/v1/refunds/R2026" {
		t.Fatalf("path = %s", p)
	}
}

func TestBusinessError_MapsToSentinel(t *testing.T) {
	cases := []struct {
		code Code
		want error
	}{
		{CodeOrderConflict, ErrOrderConflict},
		{CodeOrderNotFound, ErrOrderNotFound},
		{CodeOrderStatusDenied, ErrOrderStatusDenied},
		{CodeRefundExceedTotal, ErrRefundExceedTotal},
		{CodeSignMismatch, ErrSignMismatch},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(int(tc.code)), func(t *testing.T) {
			fs := newFakeServer(t, nil)
			fs.code = tc.code
			fs.msg = "服务端文案"
			c := newTestClient(t, fs)

			_, err := c.CreateOrder(context.Background(), CreateOrderReq{OutTradeNo: "RQ001", Subject: "x", TotalMinor: 1})
			if err == nil {
				t.Fatal("应返回错误")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is 判别失败: %v", err)
			}
			if CodeOf(err) != tc.code {
				t.Fatalf("CodeOf = %d", CodeOf(err))
			}
			var ae *APIError
			if !errors.As(err, &ae) || ae.Msg != "服务端文案" {
				t.Fatalf("APIError 里没带服务端文案: %v", err)
			}
			// 别的哨兵不能误命中。
			if errors.Is(err, ErrServerBusy) && tc.code != CodeServerBusy {
				t.Fatal("哨兵串味了")
			}
		})
	}
}

func TestUnknownBusinessCode_StillErrors(t *testing.T) {
	// SDK 落后于服务端时，没登记的新错误码也必须是错误，不能被当成成功。
	fs := newFakeServer(t, nil)
	fs.code = 99999
	c := newTestClient(t, fs)
	_, err := c.QueryOrder(context.Background(), "P1")
	if err == nil {
		t.Fatal("未知错误码被吞成成功了")
	}
	if CodeOf(err) != 99999 {
		t.Fatalf("CodeOf = %d", CodeOf(err))
	}
}

func TestResponseVerify_Unsigned(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"trade_no": "P1"})
	fs.unsigned = true
	c := newTestClient(t, fs)

	_, err := c.QueryOrder(context.Background(), "P1")
	if !errors.Is(err, ErrResponseSign) {
		t.Fatalf("没签名的响应应被拒: %v", err)
	}
}

// TestUnsignedAuthError 后端对鉴权没过的响应刻意不签（不给攻击者
// "密钥对不对"的预言机）。这种响应里没有值钱的数据，SDK 允许把真实错误码
// 透出来——否则接入方联调时看到的永远是"响应验签失败"，猜不到是密钥配错了。
func TestUnsignedAuthError(t *testing.T) {
	for _, tc := range []struct {
		code Code
		want error
	}{
		{CodeSignMismatch, ErrSignMismatch},
		{CodeTimestampSkew, ErrTimestampSkew},
		{CodeAppDisabled, ErrAppDisabled},
		{CodeNonceReplay, ErrNonceReplay},
	} {
		t.Run(strconv.Itoa(int(tc.code)), func(t *testing.T) {
			fs := newFakeServer(t, nil)
			fs.unsigned = true
			fs.code = tc.code
			fs.msg = "鉴权失败"
			c := newTestClient(t, fs)

			_, err := c.QueryOrder(context.Background(), "P1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("应透出真实鉴权错误: %v", err)
			}
		})
	}
}

// TestUnsignedNonAuthResponse_StillRejected 例外只开给鉴权段。
// 未签名的业务响应（尤其是"成功"）一律当验签失败——那是中间人伪造回执的入口。
func TestUnsignedNonAuthResponse_StillRejected(t *testing.T) {
	for _, code := range []Code{CodeSuccess, CodeOrderNotFound, CodeServerBusy, CodeRefundExceedTotal} {
		t.Run(strconv.Itoa(int(code)), func(t *testing.T) {
			fs := newFakeServer(t, map[string]any{"trade_no": "P1", "status": 3, "total_minor": 1990})
			fs.unsigned = true
			fs.code = code
			c := newTestClient(t, fs)

			_, err := c.QueryOrder(context.Background(), "P1")
			if !errors.Is(err, ErrResponseSign) {
				t.Fatalf("未签名的非鉴权响应必须按验签失败处理: %v", err)
			}
		})
	}
}

func TestResponseVerify_Tampered(t *testing.T) {
	// 签完再改一个字节 —— 中间人改回执的模拟。
	fs := newFakeServer(t, map[string]any{"trade_no": "P1"})
	fs.tamper = true
	c := newTestClient(t, fs)

	_, err := c.QueryOrder(context.Background(), "P1")
	if !errors.Is(err, ErrResponseSign) {
		t.Fatalf("被篡改的响应应被拒: %v", err)
	}
}

func TestResponseVerify_TimestampOutOfWindow(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"trade_no": "P1"})
	fs.respTSShift = -10 * time.Minute
	c := newTestClient(t, fs)

	_, err := c.QueryOrder(context.Background(), "P1")
	if !errors.Is(err, ErrTimestampWindow) {
		t.Fatalf("超窗的响应应被拒: %v", err)
	}
}

func TestResponseVerify_WrongSecret(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"trade_no": "P1"})
	c, err := New(Config{BaseURL: fs.URL, AppID: "app_k7m2qx9b4t", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	// 换掉密钥后请求签名也会错，服务端会 401 —— 这里只关心 SDK 不把 401 当成功。
	fs.quiet = true // 服务端验签失败是本用例的预期
	c.secret = "另一个密钥"
	c.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := c.QueryOrder(context.Background(), "P1"); err == nil {
		t.Fatal("密钥不对却成功了")
	}
}

func TestNonOKHTTP_ReturnsHTTPError(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.respStatus = http.StatusBadGateway
	c := newTestClient(t, fs)

	_, err := c.CreateOrder(context.Background(), CreateOrderReq{OutTradeNo: "RQ001", Subject: "x", TotalMinor: 1})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusBadGateway {
		t.Fatalf("应返回 HTTPError(502): %v", err)
	}
}

// TestWritesAreNeverRetried 是这套 SDK 的一条硬规矩：写操作绝不自动重试。
// SDK 替你重试，会让"其实已经建单/已经在退款"的请求看起来像失败。
func TestWritesAreNeverRetried(t *testing.T) {
	writes := []struct {
		name string
		call func(*Client) error
	}{
		{"CreateOrder", func(c *Client) error {
			_, err := c.CreateOrder(context.Background(), CreateOrderReq{OutTradeNo: "RQ001", Subject: "x", TotalMinor: 1})
			return err
		}},
		{"Refund", func(c *Client) error {
			_, err := c.Refund(context.Background(), RefundReq{OutTradeNo: "RQ001", OutRefundNo: "RF001", AmountMinor: 1})
			return err
		}},
		{"CloseOrder", func(c *Client) error {
			_, err := c.CloseOrder(context.Background(), "P1")
			return err
		}},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			fs := newFakeServer(t, nil)
			fs.respStatus = http.StatusInternalServerError // 换成读操作就会重试的那类错误
			c := newTestClient(t, fs)

			if err := w.call(c); err == nil {
				t.Fatal("应返回错误")
			}
			if n := fs.hits.Load(); n != 1 {
				t.Fatalf("写操作被重试了 %d 次——绝对不许", n-1)
			}
		})
	}
}

func TestReadsRetryOn5xx(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.respStatus = http.StatusInternalServerError
	c := newTestClient(t, fs) // 默认 ReadRetries=2

	if _, err := c.QueryOrder(context.Background(), "P1"); err == nil {
		t.Fatal("应返回错误")
	}
	if n := fs.hits.Load(); n != 3 {
		t.Fatalf("读操作应请求 3 次（1+2 重试），实际 %d", n)
	}
}

func TestReadsDoNotRetryBusinessErrors(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.code = CodeOrderNotFound
	c := newTestClient(t, fs)

	if _, err := c.QueryOrder(context.Background(), "P1"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
	if n := fs.hits.Load(); n != 1 {
		t.Fatalf("业务错误被重试了 %d 次", n-1)
	}
}

func TestReadRetriesDisabled(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.respStatus = http.StatusInternalServerError
	c, err := New(Config{BaseURL: fs.URL, AppID: "a", Secret: testSecret, ReadRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryOrder(context.Background(), "P1"); err == nil {
		t.Fatal("应返回错误")
	}
	if n := fs.hits.Load(); n != 1 {
		t.Fatalf("ReadRetries=-1 应关掉重试，实际请求 %d 次", n)
	}
}

func TestBaseURLPathPrefix_NotSigned(t *testing.T) {
	// 网关按路径分流：BaseURL 带前缀，请求也要带着它发出去，但「前缀不进签名」——
	// 网关剥掉它才回源，应用看到的路径里没有这一段，签了两端就永远对不上。
	//
	// 这台 srv 就是那个网关：剥掉 /gw 再交给应用，应用按它看到的路径验签。
	var gotPath string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		body, _ := io.ReadAll(r.Body)
		p := requestPayload(r.Method, r.URL.RequestURI(), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce), hashBody(body))
		if !verifySign(testSecret, p, r.Header.Get(HeaderSignature)) {
			t.Errorf("应用按自己看到的路径验签失败，待签串=%q", p)
		}
		out := []byte(`{"code":10000,"msg":"success","data":{"trade_no":"P1"}}`)
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		w.Header().Set(HeaderTimestamp, ts)
		w.Header().Set(HeaderNonce, "resp-nonce-0123456789")
		w.Header().Set(HeaderSignature, computeSign(testSecret, responsePayload(ts, "resp-nonce-0123456789", hashBody(out))))
		_, _ = w.Write(out)
	})
	srv := httptest.NewServer(http.StripPrefix("/gw", inner))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL + "/gw/", AppID: "a", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryOrder(context.Background(), "P1"); err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	// 应用看到的（= 待签的）那一段
	if gotPath != "/openapi/v1/orders/P1" {
		t.Fatalf("应用看到的 path = %s", gotPath)
	}
}

// TestSignPathStripsPrefix 待签 PATH 的切法，逐串钉死。
//
// 上面那条走的是真实链路，但只覆盖一种形状；这条把 query、转义前缀、多段前缀
// 与"切不动就报错"一次列清楚。
func TestSignPathStripsPrefix(t *testing.T) {
	cases := []struct {
		base, uri, want string
	}{
		{"https://pay.example.com", "/openapi/v1/orders?x=1", "/openapi/v1/orders?x=1"},
		{"https://pay.example.com/api", "/api/openapi/v1/orders?x=1", "/openapi/v1/orders?x=1"},
		{"https://pay.example.com/gw/pay/", "/gw/pay/openapi/v1/orders", "/openapi/v1/orders"},
		{"https://pay.example.com/a%20b", "/a%20b/openapi/v1/orders", "/openapi/v1/orders"},
	}
	for _, tc := range cases {
		c, err := New(Config{BaseURL: tc.base, AppID: "a", Secret: testSecret})
		if err != nil {
			t.Fatalf("New(%q): %v", tc.base, err)
		}
		got, err := c.signPath(tc.uri)
		if err != nil {
			t.Fatalf("signPath(%q): %v", tc.uri, err)
		}
		if got != tc.want {
			t.Errorf("BaseURL=%q signPath(%q) = %q，期望 %q", tc.base, tc.uri, got, tc.want)
		}
	}

	// 切不动就报错，不能静默签全路径——那会变成"偶尔能通、换个地址就 20002"
	c, err := New(Config{BaseURL: "https://pay.example.com/api", AppID: "a", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.signPath("/openapi/v1/orders"); err == nil {
		t.Fatal("前缀对不上时应当报错")
	}
}

func TestEmptyArgsRejectedLocally(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := newTestClient(t, fs)
	ctx := context.Background()

	if _, err := c.QueryOrder(ctx, "  "); err == nil {
		t.Fatal("空单号应就地报错")
	}
	if _, err := c.QueryOrderByOutTradeNo(ctx, ""); err == nil {
		t.Fatal("空商户单号应就地报错")
	}
	if _, err := c.CloseOrder(ctx, ""); err == nil {
		t.Fatal("空单号应就地报错")
	}
	if _, err := c.QueryRefund(ctx, ""); err == nil {
		t.Fatal("空退款单号应就地报错")
	}
	if _, err := c.QueryRefundByOutRefundNo(ctx, ""); err == nil {
		t.Fatal("空商户退款单号应就地报错")
	}
	if n := fs.hits.Load(); n != 0 {
		t.Fatalf("参数不合法却发出了 %d 个请求", n)
	}
}

func TestContextCancel(t *testing.T) {
	fs := newFakeServer(t, map[string]any{"trade_no": "P1"})
	c := newTestClient(t, fs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.QueryOrder(ctx, "P1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := fs.hits.Load(); n != 0 {
		t.Fatalf("ctx 已取消却发出了 %d 个请求", n)
	}
}

func TestBadEnvelope(t *testing.T) {
	fs := newFakeServer(t, nil)
	fs.respBody = func(*http.Request) any { return "不是信封" }
	c := newTestClient(t, fs)

	// JSON 字符串能被 json.Unmarshal 进 struct 吗？不能，会报错。
	_, err := c.QueryOrder(context.Background(), "P1")
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "jjpay") {
		t.Fatalf("错误信息应带包名前缀: %v", err)
	}
}
