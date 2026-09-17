// 这个文件刻意用外部测试包 jjpay_test：它只能碰导出的东西，
// 所以公开 API 够不够用是被编译器验着的——哪天有人把某个必需的类型收进包内，
// 这里当场编不过。pkg.go.dev 的 Examples 一栏也由它填。
package jjpay_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	jjpay "github.com/Star-Flex/jjpay-go"
)

// 三步接入：建客户端 → 下单 → 收通知。
func Example() {
	// 地址与凭据都在部署侧：JJPAY_BASE_URL / JJPAY_APP_ID / JJPAY_APP_SECRET
	c, err := jjpay.NewFromEnv()
	if err != nil {
		log.Fatal(err) // 配置不合法当场炸，别留到第一次收款
	}

	// 下单，把 CheckoutURL 给用户
	resp, err := c.CreateOrder(context.Background(), jjpay.CreateOrderReq{
		OutTradeNo: "A2026080200123", // 你自己的单号，商户内唯一，也是幂等键
		Subject:    "基础版 包月",
		TotalMinor: 1990, // 分，不是元
		NotifyURL:  "https://your-app.example/pay/notify",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.CheckoutURL)

	// 收通知——这一步才是"钱到了"的真相
	mux := http.NewServeMux()
	mux.Handle("/pay/notify", c.Middleware()(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			evt := jjpay.EventFrom(r.Context()) // 已验签、已解析
			if evt.Event == jjpay.EventPaySucceeded {
				// 幂等地开通服务：按 trade_no + event 去重，靠唯一索引裁决
			}
			fmt.Fprint(w, "SUCCESS") // 不回这个就会被重试
		})))
}

// 下单失败时怎么分辨该重试还是该查代码。
func ExampleClient_CreateOrder() {
	c, _ := jjpay.NewFromEnv()

	resp, err := c.CreateOrder(context.Background(), jjpay.CreateOrderReq{
		OutTradeNo: "A2026080200123",
		Subject:    "基础版 包月",
		TotalMinor: 1990,
	})
	switch {
	case err == nil:
		fmt.Println(resp.TradeNo, resp.CheckoutURL)
	case errors.Is(err, jjpay.ErrOrderConflict):
		// 同一个单号之前用别的金额下过单：这是业务 bug，重试多少次都一样
		log.Fatal(err)
	default:
		// 网络 / 超时 / 5xx：用同一个OutTradeNo 再调一次，那是幂等的。
		// 不要换单号重下——SDK 分不清"没建成单"和"建成了但回执丢了"，
		// 换单号会把一次成功的下单变成两笔应付。
		log.Println("稍后重试:", err)
	}
}

// 回跳页：只准展示，不准发货。
func ExampleClient_VerifyReturn() {
	c, _ := jjpay.NewFromEnv()

	handler := func(w http.ResponseWriter, r *http.Request) {
		res, err := c.VerifyReturn(r.URL.Query())
		if err != nil {
			// 不是收银台送回来的、被改过、或链接太旧。
			// err != nil 时 res 是 nil，往下走就是空指针。
			// 正确做法是当作"没带结果"，拿 trade_no 去查单。
			http.Redirect(w, r, "/orders?checking=1", http.StatusFound)
			return
		}
		if res.Paid() {
			fmt.Fprintf(w, "支付成功 ¥%d.%02d", res.TotalMinor/100, res.TotalMinor%100)
			return
		}
		fmt.Fprint(w, "正在确认…")
	}
	_ = handler
}

// 商户给自己的通知处理写测试：用 SignNotify 造一条真能通过验签的请求。
func ExampleSignNotify() {
	const secret = "sk_test_0123456789abcdef"
	body := []byte(`{"event":"pay.succeeded","trade_no":"P2026","out_trade_no":"A1","total_minor":1990}`)

	h, err := jjpay.SignNotify(secret, jjpay.EventPaySucceeded, body, time.Now())
	if err != nil {
		log.Fatal(err)
	}

	// 这一步走的是线上同一条验签路径，不是把校验绕过去。
	evt, err := jjpay.Verify(h, body, secret)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(evt.Event, evt.OutTradeNo, evt.TotalMinor)
	// Output: pay.succeeded A1 1990
}
