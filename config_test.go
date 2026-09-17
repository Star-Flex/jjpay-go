package jjpay

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestConfigFallsBackToEnv 地址与凭据的取值次序：显式 > 环境变量。
//
// 网关域名不该烧进包里。备案换域名、开发走隧道、预发与生产
// 各一套，都只能靠部署侧改一个环境变量解决——一旦有人往包里塞了个默认值，
// 换域名就变成"发新版本 + 所有接入方升级依赖 + 重新编译"。
func TestConfigFallsBackToEnv(t *testing.T) {
	t.Setenv(EnvBaseURL, "https://env.example.com")
	t.Setenv(EnvAppID, "env-app")
	t.Setenv(EnvSecret, "env-secret")

	t.Run("三项全缺时用环境变量", func(t *testing.T) {
		c, err := NewFromEnv()
		if err != nil {
			t.Fatalf("NewFromEnv: %v", err)
		}
		if c.base.Host != "env.example.com" {
			t.Fatalf("BaseURL 没走环境变量，得到 %s", c.base.Host)
		}
		if c.AppID() != "env-app" {
			t.Fatalf("AppID 没走环境变量，得到 %s", c.AppID())
		}
		if c.secret != "env-secret" {
			t.Fatal("Secret 没走环境变量")
		}
	})

	t.Run("显式的赢", func(t *testing.T) {
		c, err := New(Config{BaseURL: "https://code.example.com", AppID: "code-app", Secret: "code-secret"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.base.Host != "code.example.com" || c.AppID() != "code-app" || c.secret != "code-secret" {
			t.Fatal("显式 Config 应盖过环境变量")
		}
	})
}

// TestConfigMissingMentionsEnvVar 两处都没有时，错误里必须写出环境变量名。
// 联调期最常见的一句话是"它说 BaseURL 为空，可我明明配了"——配在哪里、
// 该配成什么名字，错误本身就该回答。
func TestConfigMissingMentionsEnvVar(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvAppID, "")
	t.Setenv(EnvSecret, "")

	_, err := New(Config{})
	if err == nil {
		t.Fatal("三项全空应当报错")
	}
	if !strings.Contains(err.Error(), EnvBaseURL) {
		t.Fatalf("错误里应写出 %s，得到 %v", EnvBaseURL, err)
	}

	_, err = New(Config{BaseURL: "https://x.example.com"})
	if err == nil || !strings.Contains(err.Error(), EnvAppID) {
		t.Fatalf("缺 AppID 的错误里应写出 %s，得到 %v", EnvAppID, err)
	}

	_, err = New(Config{BaseURL: "https://x.example.com", AppID: "a"})
	if err == nil || !strings.Contains(err.Error(), EnvSecret) {
		t.Fatalf("缺 Secret 的错误里应写出 %s，得到 %v", EnvSecret, err)
	}
}

// TestNoDefaultEndpointBakedIn 包里不许出现内置默认网关地址。
func TestNoDefaultEndpointBakedIn(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvAppID, "a")
	t.Setenv(EnvSecret, "s")
	if _, err := New(Config{}); err == nil {
		t.Fatal("BaseURL 两处都没有却构造成功了 —— 包里多了个内置默认地址")
	}
}

// TestNoURLsInSource 源码里不许出现具体的网关地址，测试文件也算。
// 放行的只有 RFC 2606 / 6761 的保留域名：example.com/org/net 与 .example 顶级域。
func TestNoURLsInSource(t *testing.T) {
	urlRe := regexp.MustCompile(`https?://([A-Za-z0-9.\-]+)`)
	reserved := func(host string) bool {
		if strings.HasSuffix(host, ".example") {
			return true
		}
		for _, d := range []string{"example.com", "example.org", "example.net"} {
			if host == d || strings.HasSuffix(host, "."+d) {
				return true
			}
		}
		return false
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range urlRe.FindAllStringSubmatch(line, -1) {
				if reserved(m[1]) {
					continue
				}
				t.Fatalf("%s:%d 出现了具体地址 %q —— 网关地址属于部署不属于代码，"+
					"示例请用 example.com 或 .example", name, i+1, m[0])
			}
		}
	}
}
