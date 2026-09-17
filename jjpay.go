// Package jjpay 是 jjpay 支付网关的 Go 客户端。
//
// 它只做四件事：签名、验签、超时与有限重试、请求响应结构体。只依赖标准库。
//
// # 用法
//
//	c, err := jjpay.NewFromEnv()
//	resp, err := c.CreateOrder(ctx, jjpay.CreateOrderReq{
//		OutTradeNo: "A2026080200123",
//		Subject:    "基础版 包月",
//		TotalMinor: 1990,
//	})
//
// 把用户送到 resp.CheckoutURL。付款结果走异步通知，用 c.Middleware 验签。
//
// # 金额
//
// 金额一律 int64，单位是币种的最小单位（CNY 即分），字段名带 Minor 后缀。
// 本包不提供以元为单位的参数。
//
// # 调用方必须自己做的两件事
//
//  1. 验签。不验签的回调端点等于任何人都能给你发“已支付”。
//     用 c.Middleware 或 c.Verify。
//  2. 幂等。同一事件可能收到多次。支付类按 trade_no 去重，退款类按 refund_no。
package jjpay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Channel 支付渠道。对外一律字符串：alipay / wechat / mock。
type Channel string

const (
	ChannelAlipay Channel = "alipay"
	ChannelWechat Channel = "wechat"
	ChannelMock   Channel = "mock"
)

// Method 支付方式，含义依渠道而定：
//
//	alipay → page（电脑网站支付）| wap（手机网站支付）
//	wechat → native（扫码）| h5 | jsapi（微信内）
//	mock → pay
type Method string

const (
	MethodAlipayPage   Method = "page"
	MethodAlipayWap    Method = "wap"
	MethodWechatNative Method = "native"
	MethodWechatH5     Method = "h5"
	MethodWechatJSAPI  Method = "jsapi"
	MethodMockPay      Method = "pay"
)

// OrderStatus 支付单状态，对外是字符串。
type OrderStatus string

const (
	OrderPending OrderStatus = "pending" // 已建单，用户还没选支付方式
	OrderPaying  OrderStatus = "paying"  // 已向渠道下单，凭据已发给用户
	OrderPaid    OrderStatus = "paid"    // 已支付。终态
	OrderClosed  OrderStatus = "closed"  // 已关闭（超时/主动关）。终态
)

// RefundStatus 退款单状态。
type RefundStatus int16

const (
	RefundProcessing RefundStatus = 1 // 处理中。同步返回的是"受理"，不是"到账"
	RefundSucceeded  RefundStatus = 2
	RefundFailed     RefundStatus = 3
)

// 默认值。
const (
	// DefaultTimeout 单次 HTTP 调用的超时。
	DefaultTimeout = 15 * time.Second
	// DefaultSkewWindow 响应/通知时间戳的允许偏差，与服务端
	// openapi_timestamp_skew_seconds 的默认值一致。
	DefaultSkewWindow = 5 * time.Minute
	// DefaultReadRetries 读操作（查单/查退款）的额外重试次数。
	// 写操作永不重试，理由见 CreateOrder 的注释。
	DefaultReadRetries = 2
	// MaxReadRetries 额外重试次数的上限。传更大的值会被压到这里。
	//
	// 退避是 200ms 左移，i 一大 time.Duration 就溢出成负数，
	// 结果是不等待、瞬间把剩下的次数烧完，极端值下甚至一次请求都发不出去却
	// 返回成功。封顶比"相信没人会传 100"便宜。
	MaxReadRetries = 5
	// maxBackoff 两次重试之间的最长间隔。
	maxBackoff = 5 * time.Second

	// maxRespBytes 响应体读取上限，防止对端（或中间人）用超大响应撑爆内存。
	maxRespBytes = 1 << 20 // 1 MiB
)

// 环境变量名。三项都是 Config 对应字段留空时的回退来源，见 New。
const (
	EnvBaseURL = "JJPAY_BASE_URL"
	EnvAppID   = "JJPAY_APP_ID"
	EnvSecret  = "JJPAY_APP_SECRET"
)

// Config 构造 Client 的参数。BaseURL / AppID / Secret 三项必须有值，
// 但不一定要写在代码里：留空时各自回退到 EnvBaseURL / EnvAppID / EnvSecret。
type Config struct {
	// BaseURL 网关根地址，如 https://pay.example.com。允许带路径前缀，
	// 前缀会一并计入签名的 PATH。留空则取 JJPAY_BASE_URL。
	//
	// 本包不带内置默认值：地址属于部署，不属于代码。
	BaseURL string
	// AppID 即 app.code，作为 X-Jjpay-Appid 发出。留空则取 JJPAY_APP_ID。
	AppID string
	// Secret app_secret。只从环境变量/密钥管理取，别写进源码或配置文件。
	// 留空则取 JJPAY_APP_SECRET。
	Secret string

	// HTTPClient 可选。想复用连接池、加代理或自定义 TLS 时传自己的。
	// 不传则用带 Timeout 的默认客户端。
	HTTPClient *http.Client
	// Timeout 单次 HTTP 调用的上限（重试的每一次各算一次），默认 15s。
	Timeout time.Duration

	// SkewWindow 响应时间戳允许的偏差，默认 ±5 分钟。
	// 服务器时钟没校准时会大面积验签失败——那正是要暴露的问题，别调大绕过。
	SkewWindow time.Duration
	// ReadRetries 读操作的额外重试次数，0 取默认值 2（即最多请求 3 次），
	// 负数表示关掉重试，超过 MaxReadRetries 按上限算。
	ReadRetries int

	// Logger 可选。传了之后，本包在重试前会打一行——那是唯一会被静默吞掉的
	// 事件：一次成功的调用背后可能藏着两次失败，不打就查不出来。
	//
	// *log.Logger 直接满足这个接口。本包不会打印密钥、签名或完整响应体。
	// 不传则什么都不打；本包没有包级 logger，也不会写 stdout/stderr。
	Logger Logger
}

// Logger 是本包唯一的日志出口，与任何日志库无关。
// *log.Logger、zap 的 SugaredLogger 都能直接或包一层满足它。
type Logger interface {
	Printf(format string, v ...any)
}

// Client 是并发安全的，一个 App 建一个复用即可。
type Client struct {
	base     *url.URL
	basePath string // 已去掉尾部 "/" 的路径前缀，参与签名
	appID    string
	secret   string
	hc       *http.Client
	timeout  time.Duration
	skew     time.Duration
	retries  int
	log      Logger
	now      func() time.Time // 测试注入点
	newNonce func() (string, error)
	sleep    func(context.Context, time.Duration) error
}

// NewFromEnv 三项全从环境变量取，等价于 New(Config{})。
// 容器 / systemd 里最常见的形态：地址与密钥都在部署侧，代码里一个字都没有。
func NewFromEnv() (*Client, error) { return New(Config{}) }

// New 构造 Client。配置不合法立刻返回错误——把"密钥忘了配"这类问题留到
// 第一次收款时才炸，代价太大。
//
// BaseURL / AppID / Secret 留空时依次回退到 EnvBaseURL / EnvAppID / EnvSecret；
// 两处都没有才报错，错误里会把环境变量名一并写出来。
func New(cfg Config) (*Client, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv(EnvBaseURL))
	}
	if baseURL == "" {
		return nil, fmt.Errorf("jjpay: 网关地址为空，请传 Config.BaseURL 或设 %s", EnvBaseURL)
	}
	appID := strings.TrimSpace(cfg.AppID)
	if appID == "" {
		appID = strings.TrimSpace(os.Getenv(EnvAppID))
	}
	if appID == "" {
		return nil, fmt.Errorf("jjpay: AppID 为空，请传 Config.AppID 或设 %s", EnvAppID)
	}
	// secret 从文件/env_file/k8s secret 读进来时几乎必然
	// 带一个尾换行，而带换行的 secret 会让每一次请求被服务端以 20002 拒掉。
	// 三项凭据两种处理方式本身就是坑，所以一视同仁。
	secret := strings.TrimSpace(cfg.Secret)
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv(EnvSecret))
	}
	if secret == "" {
		return nil, fmt.Errorf("jjpay: Secret 为空，请传 Config.Secret 或设 %s", EnvSecret)
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("jjpay: BaseURL 解析失败: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("jjpay: BaseURL 必须是绝对地址（含 scheme 与 host），得到 %q", baseURL)
	}
	// 带 query 或 fragment 的话，
	// 后面拼上接口路径会得到一个谁也没想要的地址：?a=1 后面再接 /openapi/v1/orders
	// 就成了 query 的一部分。与其拼出来再猜，不如构造时就拒。
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("jjpay: BaseURL 不能带 query 或 fragment，得到 %q", baseURL)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	skew := cfg.SkewWindow
	if skew <= 0 {
		skew = DefaultSkewWindow
	}
	retries := cfg.ReadRetries
	if retries < 0 {
		retries = 0
	} else if cfg.ReadRetries == 0 {
		retries = DefaultReadRetries
	}
	if retries > MaxReadRetries {
		retries = MaxReadRetries
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	// 不跟重定向：待签串不含 host，而自定义签名头会跟着 302 原样转发到目标主机。
	// 复制一份再改，不动调用方那只 Client（Transport 是指针，连接池照常复用）。
	noRedirect := *hc
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	hc = &noRedirect

	return &Client{
		base: u,
		// Path 是解码后的：BaseURL 写成
		// https://pay.example.com/a%20b 时 Path 是 "/a b"，拿它签名就会签出与真正发出去的
		// 字节不同的串，服务端一律 20002。EscapedPath 才是上线路的那一份。
		basePath: strings.TrimRight(u.EscapedPath(), "/"),
		appID:    appID,
		secret:   secret,
		log:      cfg.Logger,
		hc:       hc,
		timeout:  timeout,
		skew:     skew,
		retries:  retries,
		now:      time.Now,
		newNonce: randomNonce,
		sleep:    sleepCtx,
	}, nil
}

// AppID 返回当前 App 标识，便于调用方打日志。Secret 刻意不给读。
func (c *Client) AppID() string { return c.appID }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// call 描述一次 openapi 调用。
type call struct {
	method string // 大写
	path   string // 以 / 开头，含 query，不含 scheme 与 host（basePath 由 Client 补）
	body   any    // nil 表示无 body（GET / 无入参的 POST）
	out    any    // 成功时把 data 反序列化进去，可为 nil
	// retry 是否允许重试。只有读操作能置 true。
	retry bool
}

func (c *Client) do(ctx context.Context, cl call) error {
	var raw []byte
	if cl.body != nil {
		b, err := json.Marshal(cl.body)
		if err != nil {
			return fmt.Errorf("jjpay: 请求序列化失败: %w", err)
		}
		raw = b
	}

	attempts := 1
	if cl.retry {
		attempts += c.retries
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			// 200ms、400ms、800ms……封顶 5s，只在重试之间等，最后一次失败不白等。
			d := backoff(i)
			if c.log != nil {
				c.log.Printf("jjpay: %s %s 第 %d 次失败，%s 后重试: %v",
					cl.method, cl.path, i, d, lastErr)
			}
			if err := c.sleep(ctx, d); err != nil {
				return err
			}
		}
		err := c.attempt(ctx, cl, raw)
		if err == nil {
			return nil
		}
		lastErr = err
		// 这一条必须在 retryable 之前判，
		// 而且判的是调用方那只 ctx，不是错误本身：SDK 给每次尝试套了自己的
		// 超时，那个超时到期同样是 context.DeadlineExceeded，按错误判会把
		// 单次尝试超时误当成调用方不想要了——而超时恰恰是最该重试的情况。
		if ctx.Err() != nil {
			return err
		}
		if !retryable(err) {
			return err
		}
	}
	return lastErr
}

// retryable 只认"重试可能换个结果"的错误：传输层失败、5xx/429，以及限流与
// 系统繁忙这两个业务码（业务响应 HTTP 恒为 200，只认 429 会漏掉它们）。
// 只对读操作生效。
func retryable(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode >= 500 || he.StatusCode == http.StatusTooManyRequests
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code == CodeServerBusy || ae.Code == CodeRateLimited
	}
	if errors.Is(err, ErrResponseSign) || errors.Is(err, ErrTimestampWindow) ||
		errors.Is(err, ErrMissingHeader) || errors.Is(err, ErrBadResponse) {
		return false
	}
	// 调用方取消不重试。注意这里不判 DeadlineExceeded——单次尝试的超时也是
	// 它，而那是可以重试的；调用方自己的 ctx 到期由 do 里的 ctx.Err() 拦住。
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true // 传输层错误，含单次尝试超时
}

// backoff 第 i 次重试前等多久：200ms 指数退避，封顶 maxBackoff。
func backoff(i int) time.Duration {
	if i < 1 {
		return 0
	}
	if i > 20 { // 再大就该溢出了，直接给上限
		return maxBackoff
	}
	d := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

func (c *Client) attempt(ctx context.Context, cl call, raw []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var rdr io.Reader
	if raw != nil {
		rdr = bytes.NewReader(raw)
	}
	full := c.base.Scheme + "://" + c.base.Host + c.basePath + cl.path
	req, err := http.NewRequestWithContext(reqCtx, cl.method, full, rdr)
	if err != nil {
		return fmt.Errorf("jjpay: 构造请求失败: %w", err)
	}

	// RequestURI() 返回的就是这条
	// 请求行上真正写出去的那串字节（EscapedPath + ? + RawQuery）。自己拼字符串
	// 去签，只要 net/url 在任何一环做了归一化（大小写转义、点段消除），
	// 签的和发的就会是两串东西，服务端一律 20002，而错误信息只会说"签名不对"。
	// 让它由构造过程保证相等，比事后对齐可靠。
	ts := strconv.FormatInt(c.now().Unix(), 10)
	nonce, err := c.newNonce()
	if err != nil {
		return err
	}
	sig := computeSign(c.secret, requestPayload(cl.method, req.URL.RequestURI(), ts, nonce, hashBody(raw)))
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderAppID, c.appID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, sig)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("jjpay: 请求失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return fmt.Errorf("jjpay: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := truncate(string(body), 512)
		if loc := resp.Header.Get("Location"); loc != "" {
			// 重定向是被刻意拒掉的（见 New 里的 CheckRedirect），这里把 Location
			// 带出来，否则接入方只看到一个空 body 的 302，查不出是谁在跳。
			// 跳转地址的 query 里常常挂着一次性 token，
			// 而错误文本是要进日志的——日志比响应体活得久得多。
			detail = "本 SDK 不跟重定向，Location: " + redactURL(loc)
		}
		return &HTTPError{StatusCode: resp.StatusCode, Body: detail}
	}

	// 验签在解析之前：没验过的字节不配被当成数据。
	if err := c.verifyResponse(resp.Header, body); err != nil {
		// 鉴权没过的响应服务端刻意不签名，此时透出鉴权错误码本身，比让调用方
		// 对着"响应验签失败"猜密钥有用。范围见 unsignedAuthError。
		if ae := unsignedAuthError(resp.Header, body); ae != nil {
			return ae
		}
		return err
	}

	var env struct {
		Code Code            `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	if env.Code != CodeSuccess {
		return &APIError{Code: env.Code, Msg: env.Msg, Data: env.Data}
	}
	if cl.out == nil {
		return nil
	}
	// 以前这里是 len(env.Data) == 0 直接返回 nil，
	// 于是 data 缺失、data:null、data:{} 三种都会给调用方一个「零值结构体 +
	// err == nil」——金额 0、状态空串，看起来像一笔没付钱的单，而实际上
	// 是回执根本没说。宁可报错让调用方去查，也不能让零值冒充答案。
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("%w: 成功响应里没有 data", ErrBadResponse)
	}
	if err := json.Unmarshal(env.Data, cl.out); err != nil {
		return fmt.Errorf("%w: data 反序列化失败: %v", ErrBadResponse, err)
	}
	return nil
}

// verifyResponse 校验回执签名，防中间人改回执。
// 待签串：{TIMESTAMP}\n{NONCE}\n{SHA256_HEX(BODY)}。
func (c *Client) verifyResponse(h http.Header, body []byte) error {
	ts := h.Get(HeaderTimestamp)
	nonce := h.Get(HeaderNonce)
	sig := h.Get(HeaderSignature)
	if ts == "" || nonce == "" || sig == "" {
		return fmt.Errorf("%w: 响应缺少 %s/%s/%s", ErrResponseSign, HeaderTimestamp, HeaderNonce, HeaderSignature)
	}
	if err := checkSkew(ts, c.now(), c.skew); err != nil {
		return err
	}
	if !verifySign(c.secret, responsePayload(ts, nonce, hashBody(body)), sig) {
		return ErrResponseSign
	}
	return nil
}

// unsignedAuthError 处理"没签名 + 鉴权段错误码"这一种响应，见 attempt 里的
// 说明。不符合这两个条件一律返回 nil，由调用方按验签失败处理。
func unsignedAuthError(h http.Header, body []byte) *APIError {
	if h.Get(HeaderSignature) != "" || h.Get(HeaderTimestamp) != "" || h.Get(HeaderNonce) != "" {
		return nil // 签了但没通过 = 篡改或密钥不对，不走这条路
	}
	var env struct {
		Code Code   `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	// 放进来的字节是完全没有验过签的，
	// 只因为里面除了一个鉴权错误码没有任何值钱的东西才被容忍。
	// 口子开到 20999 等于给未来任何 209xx 新码一张免验签的通行证。
	if env.Code < 20001 || env.Code > 20099 {
		return nil
	}
	// 这条响应整个没验过签，Msg 是对端随便写的字符串。
	// 它的去处是接入方的日志，而攻击者能控制它就等于能往你的日志里写东西。
	// 错误码本身已经说清了是哪一类鉴权失败，codeText 给的是我们自己的中文，
	// 够排查用了。Data 同理丢弃。
	return &APIError{Code: env.Code}
}

// checkSkew 校验 Unix 秒时间戳落在 now ± window 内。
func checkSkew(ts string, now time.Time, window time.Duration) error {
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: 时间戳 %q 不是 Unix 秒", ErrTimestampWindow, ts)
	}
	diff := now.Sub(time.Unix(sec, 0))
	if diff < 0 {
		diff = -diff
	}
	if diff > window {
		return fmt.Errorf("%w: 偏差 %s 超过 %s", ErrTimestampWindow, diff, window)
	}
	return nil
}

// redactURL 只保留 scheme://host/path，丢掉 query 与 fragment。
// 解析不出来就整条丢掉，不做"尽量保留"——猜错的代价是把 token 写进日志。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(无法解析的地址，已略去)"
	}
	out := u.Scheme + "://" + u.Host + u.EscapedPath()
	if u.RawQuery != "" || u.Fragment != "" {
		out += "?(已略去)"
	}
	return truncate(out, 256)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 这里接的是上游网关与服务端的中文文案，
	// 512 字节几乎必然落在一个 UTF-8 字符中间，日志里就会出现 �。
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
