package jjpay

import (
	"os"
	"strings"
	"testing"
)

// TestNoThirdPartyDependencies 零第三方依赖的门禁。
//
// 把 SDK 拆成独立 module 只让依赖看得见（go.mod 里
// 多一行），并不会让任何东西变红。"只依赖标准库"是这个包对接入方的承诺——
// 接入方的依赖树里凭空多出 gin/gorm 是不可接受的，而这种事往往是某次
// "顺手用一下" 加进来的，代码评审时谁也不会去翻 go.mod。
//
// 判据直接读 go.mod：出现 require / replace / 任何非空的依赖块即失败。
func TestNoThirdPartyDependencies(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("读 go.mod: %v", err)
	}
	inRetract := false
	for i, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		// retract 放行：作废一个错版本时门禁不该拦在路上。
		if inRetract {
			if s == ")" {
				inRetract = false
			}
			continue
		}
		switch {
		case strings.HasPrefix(s, "module "), strings.HasPrefix(s, "go "),
			strings.HasPrefix(s, "toolchain "), strings.HasPrefix(s, "retract "):
			continue
		case s == "retract (":
			inRetract = true
			continue
		default:
			t.Fatalf("go.mod:%d 出现了 %q —— 本包只允许 module / go / toolchain / retract", i+1, s)
		}
	}
}

// TestNoMarkdownInDocComments 注释里不许出现 Markdown 的粗体标记。
// godoc 不认它，星号会原样渲染到 pkg.go.dev 上。
func TestNoMarkdownInDocComments(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("读 %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "**") {
				t.Errorf("%s:%d 注释里有 Markdown 粗体：%s\n"+
					"godoc 不认它，星号会原样出现在 pkg.go.dev 上。强调请用「」。",
					e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}
