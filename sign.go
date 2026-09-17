package jjpay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// 四个签名头。
//
// HTTP 头名大小写不敏感，Go 的 http.Header 会把它们规范成
// `X-Jjpay-Appid` 这样的 MIME 规范形式再存取——两端都用 Header.Get/Set
// 即可，不必纠结字面大小写。
const (
	HeaderAppID     = "X-Jjpay-Appid"
	HeaderTimestamp = "X-Jjpay-Timestamp"
	HeaderNonce     = "X-Jjpay-Nonce"
	HeaderSignature = "X-Jjpay-Signature"
	HeaderEvent     = "X-Jjpay-Event"
)

// hashBody 返回 body 原始字节的 SHA-256，小写 hex。
//
// GET 这类没有 body 的请求传 nil 即可——取的是空串的 SHA-256
// （e3b0c442…b855），这一点 有明写，两端必须一致。
func hashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// requestPayload 拼请求待签串：
//
//	{METHOD}\n{PATH}\n{TIMESTAMP}\n{NONCE}\n{SHA256_HEX(BODY)}
//
// method 必须大写；path 含 query（如 `/openapi/v1/orders?x=1`），不含 scheme
// 与 host。结尾没有换行——多一个 \n 就是另一个签名。
func requestPayload(method, path, timestamp, nonce, bodyHash string) string {
	return strings.Join([]string{strings.ToUpper(method), path, timestamp, nonce, bodyHash}, "\n")
}

// responsePayload 拼响应待签串：{TIMESTAMP}\n{NONCE}\n{SHA256_HEX(BODY)}。
//
// 响应也签名是为了让商户能确认回执没被中间人改过。
func responsePayload(timestamp, nonce, bodyHash string) string {
	return strings.Join([]string{timestamp, nonce, bodyHash}, "\n")
}

// notifyPayload 拼异步通知待签串：
//
//	{EVENT}\n{TIMESTAMP}\n{NONCE}\n{SHA256_HEX(BODY)}
//
// 密钥同样是该 App 的 app_secret。
func notifyPayload(event, timestamp, nonce, bodyHash string) string {
	return strings.Join([]string{event, timestamp, nonce, bodyHash}, "\n")
}

// returnPayload 拼收银台回跳参数的待签串：
//
//	return\n{TRADE_NO}\n{OUT_TRADE_NO}\n{STATUS}\n{TOTAL_MINOR}\n{CURRENCY}\n{PAID_AT}\n{TIMESTAMP}\n{NONCE}
//
// 字段顺序固定，缺省字段（未支付时的 paid_at）留空串占位。
func returnPayload(tradeNo, outTradeNo, status, totalMinor, currency, paidAt, timestamp, nonce string) string {
	return strings.Join([]string{"return", tradeNo, outTradeNo, status, totalMinor, currency, paidAt, timestamp, nonce}, "\n")
}

// sign 用 app_secret 对待签串做 HMAC-SHA256，输出小写 hex。
func computeSign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// verifySign 常数时间比对签名。不能用 ==：它是短路比较，耗时随前缀匹配长度
// 变化，能被逐位试出来。比较前按小写 hex 归一。
func verifySign(secret, payload, got string) bool {
	want := computeSign(secret, payload)
	return hmac.Equal([]byte(strings.ToLower(got)), []byte(want))
}

// randomNonce 生成 32 位小写 hex 随机串（落在规范要求的 16~64 位内）。
func randomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("jjpay: 生成 nonce 失败: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
