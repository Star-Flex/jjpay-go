package jjpay

import (
	"errors"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const rtSecret = "sk_test_return"

// returnQuery 造一份 jjpay 会拼到 return_url 上的参数。
func returnQuery(at time.Time, status string, paidAt string) url.Values {
	ts := strconv.FormatInt(at.Unix(), 10)
	nonce := "3f9a1c2b4d5e6f708192a3b4c5d6e7f8"
	q := url.Values{}
	q.Set("trade_no", "P20260802143012K7M2QX9B4T")
	q.Set("out_trade_no", "RQ2026080200123")
	q.Set("status", status)
	q.Set("total_minor", "1990")
	q.Set("currency", "CNY")
	if paidAt != "" {
		q.Set("paid_at", paidAt)
	}
	q.Set("timestamp", ts)
	q.Set("nonce", nonce)
	q.Set("sign", computeSign(rtSecret, returnPayload("P20260802143012K7M2QX9B4T", "RQ2026080200123", status, "1990", "CNY", paidAt, ts, nonce)))
	// 商户自己的参数混在一起不影响验签
	q.Set("from", "checkout")
	return q
}

func TestVerifyReturn_Paid(t *testing.T) {
	now := time.Date(2026, 8, 2, 14, 31, 10, 0, time.FixedZone("CST", 8*3600))
	q := returnQuery(now, "paid", "2026-08-02T14:31:05+08:00")
	res, err := VerifyReturnWithOptions(q, ReturnOptions{Secret: rtSecret, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Paid() || res.TotalMinor != 1990 || res.Currency != "CNY" || res.OutTradeNo != "RQ2026080200123" {
		t.Fatalf("解析结果不对: %+v", res)
	}
	if res.PaidAt.IsZero() || res.PaidAt.Unix() != now.Add(-5*time.Second).Unix() {
		t.Fatalf("paid_at 解析不对: %v", res.PaidAt)
	}
}

func TestVerifyReturn_ClosedHasNoPaidAt(t *testing.T) {
	now := time.Now()
	res, err := VerifyReturn(returnQuery(now, "closed", ""), rtSecret)
	if err != nil {
		t.Fatal(err)
	}
	if res.Paid() || !res.PaidAt.IsZero() {
		t.Fatalf("已关闭不该有 paid_at: %+v", res)
	}
}

func TestVerifyReturn_Tampered(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ name, k, v string }{
		{"改状态", "status", "paid"},
		{"改金额", "total_minor", "1"},
		{"改单号", "out_trade_no", "OTHER"},
		{"补一个 paid_at", "paid_at", "2026-08-02T14:31:05+08:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := returnQuery(now, "closed", "")
			q.Set(tc.k, tc.v)
			if _, err := VerifyReturn(q, rtSecret); !errors.Is(err, ErrReturnSign) {
				t.Fatalf("改了 %s 应验签失败，实际 %v", tc.k, err)
			}
		})
	}
	t.Run("密钥不对", func(t *testing.T) {
		if _, err := VerifyReturn(returnQuery(now, "paid", ""), "sk_other"); !errors.Is(err, ErrReturnSign) {
			t.Fatalf("期望 ErrReturnSign，实际 %v", err)
		}
	})
}

func TestVerifyReturn_StaleLink(t *testing.T) {
	// 十分钟前的链接：签名是真的，但不能再当凭证
	q := returnQuery(time.Now().Add(-10*time.Minute), "paid", "2026-08-02T14:31:05+08:00")
	if _, err := VerifyReturn(q, rtSecret); !errors.Is(err, ErrTimestampWindow) {
		t.Fatalf("期望 ErrTimestampWindow，实际 %v", err)
	}
}

func TestVerifyReturn_Missing(t *testing.T) {
	q := url.Values{"trade_no": {"P1"}, "out_trade_no": {"O1"}}
	if _, err := VerifyReturn(q, rtSecret); !errors.Is(err, ErrReturnMissing) {
		t.Fatalf("只带单号（未签名的退化形态）应报缺参数，实际 %v", err)
	}
	if _, err := VerifyReturn(returnQuery(time.Now(), "paid", ""), ""); err == nil {
		t.Fatal("secret 为空应报错")
	}
}
