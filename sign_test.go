package jjpay

import (
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

// 与 jjpay 服务端的 sign 包共用的那一组向量。
//
// 签名算法是跨仓库契约（后端一份实现、SDK 一份实现），光看代码发现不了拼接
// 顺序/大小写/分隔符的差异，但对方会全线验签失败。两边钉同一组输入输出，
// 任何一方改了当场红。改这里的期望值 = 改一个已发布的契约，先改
// 并同步另一端。
const (
	shSecret = "sk_test_0123456789abcdef"
	// 固定向量的输入串是冻结的字节，字段名改叫 total_minor 之后也不动它（后端同）
	shBody      = `{"out_trade_no":"RQ001","subject":"测试","total_fen":1990}`
	shReqSig    = "ba9fb13a8c376b024d2992bdcfa3135ac4c36479847175d510cc8d3a7a1c4ce0"
	shEmptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	shReturnSig = "b9215baa74c02a73c95e5a0b35ee73c95650a26604c5d94c85ce40314c6fa368"
	// 通知与响应这两组是 2026-09-17 补的。此前只有请求与回跳有跨端向量，
	// 而通知是唯一决定发货的链路：两端字段顺序真漂了，后端照签、SDK 照验，
	// 两个仓库都不会有测试变红，直到某天所有商户同时收不到货。
	shNotifySig = "3d30cc9a421ecaba2af10e08c9327525168b19b286b8a9700609150a638c071f"
	shRespSig   = "f34d8ea685fca91b2b73cbee2dbba9348d9c9580bf8d2ffd0af732a1522c6d78"
)

func TestKnownVector_NotifySharedWithBackend(t *testing.T) {
	p := notifyPayload("pay.succeeded", "1785000000", "abc123", hashBody([]byte(shBody)))
	if got := computeSign(shSecret, p); got != shNotifySig {
		t.Fatalf("通知签名 = %s\n期望 = %s\n（刚改过签名算法？先确认服务端的 sign 包同步改了）", got, shNotifySig)
	}
}

func TestKnownVector_ResponseSharedWithBackend(t *testing.T) {
	p := responsePayload("1785000000", "abc123", hashBody([]byte(shBody)))
	if got := computeSign(shSecret, p); got != shRespSig {
		t.Fatalf("响应签名 = %s\n期望 = %s\n（刚改过签名算法？先确认服务端的 sign 包同步改了）", got, shRespSig)
	}
}

func TestKnownVector_ReturnSharedWithBackend(t *testing.T) {
	p := returnPayload("P20260802143012K7M2QX9B4T", "RQ2026080200123", "paid", "1990", "CNY",
		"2026-08-02T14:31:05+08:00", "1785000000", "3f9a1c2b4d5e6f708192a3b4c5d6e7f8")
	if got := computeSign(shSecret, p); got != shReturnSig {
		t.Fatalf("回跳签名 = %s\n期望 = %s\n（刚改过签名算法？先确认服务端的 sign 包同步改了）", got, shReturnSig)
	}
}

func TestKnownVector_SharedWithBackend(t *testing.T) {
	// 后端 sign.hashBody(nil) 的向量。
	if got := hashBody(nil); got != shEmptyHash {
		t.Fatalf("hashBody(nil) = %s, want %s", got, shEmptyHash)
	}
	p := requestPayload("POST", "/openapi/v1/orders", "1785000000", "abc123", hashBody([]byte(shBody)))
	if want := "POST\n/openapi/v1/orders\n1785000000\nabc123\n" + hashBody([]byte(shBody)); p != want {
		t.Fatalf("待签串 = %q, want %q", p, want)
	}
	got := computeSign(shSecret, p)
	if got != shReqSig {
		t.Fatalf("请求签名 = %s\n期望 = %s\n（刚改过签名算法？先确认服务端的 sign 包同步改了）", got, shReqSig)
	}
}

func TestSign_MethodCaseNormalized(t *testing.T) {
	// 后端 RequestPayload 会把 method 大写化，SDK 必须一致。
	if computeSign(shSecret, requestPayload("post", "/x", "1", "n", "b")) !=
		computeSign(shSecret, requestPayload("POST", "/x", "1", "n", "b")) {
		t.Fatal("method 大小写应被归一")
	}
}

func TestSign_QueryIsPartOfSignature(t *testing.T) {
	a := computeSign(shSecret, requestPayload("GET", "/openapi/v1/orders?out_trade_no=A", "1", "n", "b"))
	b := computeSign(shSecret, requestPayload("GET", "/openapi/v1/orders?out_trade_no=B", "1", "n", "b"))
	if a == b {
		t.Fatal("换了 query 却签出同样的值——中间人可以改查询目标")
	}
}

func TestSign_PayloadShapesDiffer(t *testing.T) {
	if responsePayload("1", "n", "b") == notifyPayload("pay.succeeded", "1", "n", "b") {
		t.Fatal("响应与通知的待签串不该相同")
	}
	if notifyPayload("pay.succeeded", "1", "n", "b") == notifyPayload("refund.failed", "1", "n", "b") {
		t.Fatal("event 必须进签名，否则 refund.failed 可被改头换面成 refund.succeeded")
	}
}

// 以下是 SDK 侧补的向量（响应签名、通知签名、带 query 的 GET 请求签名）。
// 后端只固化了请求签名那一组，这几组等后端实现对应路径时应搬过去共用。
const (
	// 下面那几个 vec*Sig 是拿它们
	// 算出来的 HMAC：动了输入而不重算期望值，测试当场红；两边一起被 sed 改才是灾难。
	// 曾经有一次全仓改名就扫中过这里，就是靠这组测试发现的。
	// 里面写的是什么字段名不重要，重要的是它和下面的期望值是一对。
	vecSecret = "sk_test_jjpay_0123456789abcdef"
	vecTS     = "1785000000"
	vecNonce  = "3f9a1c2b4d5e6f708192a3b4c5d6e7f8"
	vecBody   = `{"out_trade_no":"RQ2026080200123","subject":"测试商品 · 基础版 包月","total_minor":1990}`

	vecEmptyHash  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	vecBodyHash   = "4b5be3d0171039a9fc0bd67811def328613c78021d23cce71a68d22eabf32835"
	vecReqPostSig = "52dd8c1b3489132ece8505ed6ce35698cde36439ef24e8c492acedfa54f931cb"
	vecReqGetSig  = "7763916ddb93be419c8d4c23ab0670d4993d343a82a12f3eee0729c84df7c5bf"

	vecRespBody = `{"code":10000,"msg":"success","data":{"trade_no":"P20260802143012K7M2QX9B4T"}}`
	vecRespSig  = "f8e734ba8251f1a00d3931c2ba915e634720b16564551c8636c26467eeea847b"

	// vecNotifyBody 里的 payer_ref 服务端已经不发了，这里留着是有意的：签名算的是
	// 原始字节，报文里多一个 SDK 不认识的字段照样要验得过。改动它会同时作废下面
	// 那条签名向量——那是这组测试的锚，别动。
	vecNotifyBody = `{"event":"pay.succeeded","trade_no":"P20260802143012K7M2QX9B4T",` +
		`"out_trade_no":"RQ2026080200123","total_minor":1990,"channel":"wechat","method":"native",` +
		`"channel_trade_no":"4200001234202608021234567890","paid_at":"2026-08-02T14:31:05+08:00",` +
		`"attach":"plan_id=7","payer_ref":"u_8842"}`
	vecNotifySig = "00c8db603cdb92c81780a7ed73e6a69de867ad5d2b709c4489b4bac85de87391"
)

func TestBodyHash_EmptyIsSHA256OfEmptyString(t *testing.T) {
	// GET 无 body 时取的是空串的 SHA-256，不是空字符串本身。
	if got := hashBody(nil); got != vecEmptyHash {
		t.Fatalf("hashBody(nil) = %s, want %s", got, vecEmptyHash)
	}
	if got := hashBody([]byte{}); got != vecEmptyHash {
		t.Fatalf("hashBody([]) = %s, want %s", got, vecEmptyHash)
	}
	if got := hashBody([]byte(vecBody)); got != vecBodyHash {
		t.Fatalf("hashBody(body) = %s, want %s", got, vecBodyHash)
	}
}

func TestRequestSignPayload_Shape(t *testing.T) {
	got := requestPayload("post", "/openapi/v1/orders", vecTS, vecNonce, vecBodyHash)
	want := "POST\n/openapi/v1/orders\n" + vecTS + "\n" + vecNonce + "\n" + vecBodyHash
	if got != want {
		t.Fatalf("待签串不对：\n got %q\nwant %q", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatal("待签串结尾不许有换行")
	}
	if n := strings.Count(got, "\n"); n != 4 {
		t.Fatalf("待签串应有 4 个换行，实际 %d", n)
	}
}

func TestKnownVector_RequestPOST(t *testing.T) {
	p := requestPayload("POST", "/openapi/v1/orders", vecTS, vecNonce, hashBody([]byte(vecBody)))
	if got := computeSign(vecSecret, p); got != vecReqPostSig {
		t.Fatalf("POST 签名 = %s, want %s", got, vecReqPostSig)
	}
}

func TestKnownVector_RequestGETWithQuery(t *testing.T) {
	// PATH 必须含 query，且 GET 的 body 哈希是空串哈希。
	p := requestPayload("GET", "/openapi/v1/orders?out_trade_no=RQ2026080200123", vecTS, vecNonce, hashBody(nil))
	if got := computeSign(vecSecret, p); got != vecReqGetSig {
		t.Fatalf("GET 签名 = %s, want %s", got, vecReqGetSig)
	}
}

func TestKnownVector_Response(t *testing.T) {
	p := responsePayload(vecTS, vecNonce, hashBody([]byte(vecRespBody)))
	if want := vecTS + "\n" + vecNonce + "\n" + hashBody([]byte(vecRespBody)); p != want {
		t.Fatalf("响应待签串 = %q, want %q", p, want)
	}
	if got := computeSign(vecSecret, p); got != vecRespSig {
		t.Fatalf("响应签名 = %s, want %s", got, vecRespSig)
	}
}

func TestKnownVector_Notify(t *testing.T) {
	p := notifyPayload("pay.succeeded", vecTS, vecNonce, hashBody([]byte(vecNotifyBody)))
	if got := computeSign(vecSecret, p); got != vecNotifySig {
		t.Fatalf("通知签名 = %s, want %s", got, vecNotifySig)
	}
}

func TestSign_IsLowerHexAndKeyed(t *testing.T) {
	sig := computeSign(vecSecret, "x")
	if len(sig) != 64 {
		t.Fatalf("签名长度 = %d, want 64", len(sig))
	}
	if sig != strings.ToLower(sig) {
		t.Fatalf("签名必须是小写 hex，得到 %s", sig)
	}
	if computeSign("another-secret", "x") == sig {
		t.Fatal("换了密钥签名居然一样")
	}
}

func TestVerifySignature(t *testing.T) {
	p := requestPayload("POST", "/openapi/v1/orders", vecTS, vecNonce, hashBody([]byte(vecBody)))
	if !verifySign(vecSecret, p, vecReqPostSig) {
		t.Fatal("正确签名应通过")
	}
	if !verifySign(vecSecret, p, strings.ToUpper(vecReqPostSig)) {
		t.Fatal("大写 hex 也应通过（归一后比较）")
	}
	if verifySign(vecSecret, p, "") {
		t.Fatal("空签名不该通过")
	}
	if verifySign("wrong-secret", p, vecReqPostSig) {
		t.Fatal("错密钥不该通过")
	}
	// 只差一位也必须拒。
	bad := vecReqPostSig[:63] + "0"
	if bad == vecReqPostSig {
		bad = vecReqPostSig[:63] + "1"
	}
	if verifySign(vecSecret, p, bad) {
		t.Fatal("改一位的签名不该通过")
	}
}

// TestHeaderNames_MatchBackendOnTheWire 两端的头名常量必须都是 MIME 规范形式。
//
// 两端曾经一个写 `...-AppId`、另一个写 `...-Appid`，字面就不
// 一致；不炸只因为 http.Header 会把两者规范成同一个键、而且待签串里不含头名——
// 也就是说这种漂移在运行时完全没有症状，只会在某天有人改用别的 HTTP 库时爆开。
// 现在两边都对齐成 textproto 的规范形式，这个测试留着防再漂。
func TestHeaderNames_MatchBackendOnTheWire(t *testing.T) {
	const backendAppID = "X-Jjpay-Appid" // 后端 sign.HeaderAppID 的字面
	h := http.Header{}
	h.Set(HeaderAppID, "app_k7m2qx9b4t")
	if got := h.Get(backendAppID); got != "app_k7m2qx9b4t" {
		t.Fatalf("按后端头名取不到值：%q", got)
	}
	if textproto.CanonicalMIMEHeaderKey(HeaderAppID) != backendAppID {
		t.Fatalf("规范化后 %q != %q", textproto.CanonicalMIMEHeaderKey(HeaderAppID), backendAppID)
	}
	for _, k := range []string{HeaderTimestamp, HeaderNonce, HeaderSignature, HeaderEvent} {
		if textproto.CanonicalMIMEHeaderKey(k) != k {
			t.Fatalf("头名 %q 不是规范形式（后端按规范形式写死）", k)
		}
	}
}

func TestNonce(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		n, err := randomNonce()
		if err != nil {
			t.Fatalf("randomNonce() 出错: %v", err)
		}
		if len(n) != 32 { // 规范要求 16~64 位
			t.Fatalf("nonce 长度 = %d, want 32", len(n))
		}
		if seen[n] {
			t.Fatalf("nonce 重复: %s", n)
		}
		seen[n] = true
	}
}
