package gateway

import (
	"strings"
	"testing"
)

// TestMask 掩码规则：只留前后各 4 字符，短串整体遮蔽。
func TestMask(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "(未配置)"},
		{"a", "****"},
		{"12345678", "****"},          // 恰好 8：不留片段
		{"123456789", "1234****6789"}, // 9：前后各 4
		{"gwa_AAAAAAAAAAAAAAAAAAAA", "gwa_****AAAA"},
		{"gwr_RRRRRRRRRRRRRRRR", "gwr_****RRRR"},
		{"ximo_sk_ab12cd34ef56", "ximo****ef56"},
		{"短的中文凭据", "****"}, // 按字符数而非字节数
	}
	for _, c := range cases {
		if got := Mask(c.in); got != c.want {
			t.Errorf("Mask(%q) = %q, 期望 %q", c.in, got, c.want)
		}
		// 掩码结果本身绝不能包含原文（除前后各 4 以外的部分）。
		if len(c.in) > 8 && strings.Contains(Mask(c.in), c.in) {
			t.Errorf("Mask(%q) 仍然包含原文", c.in)
		}
	}
	// 中文按 rune 计数：8 个字符以下整体遮蔽。
	if got := Mask("前四位中文后四位中文"); !strings.HasPrefix(got, "前四位中") || !strings.HasSuffix(got, "位中文") {
		t.Errorf("Mask 混合文本 = %q", got)
	}
}

// TestRedact 只掩掉真正传进来的凭据，且短串不参与替换（避免打碎正常文本）。
func TestRedact(t *testing.T) {
	access := "gwa_AAAAAAAAAAAAAAAAAAAA"
	refresh := "gwr_RRRRRRRRRRRRRRRRRR"
	msg := "bad token " + access + " and refresh " + refresh

	got := Redact(msg, access, refresh)
	if strings.Contains(got, access) || strings.Contains(got, refresh) {
		t.Errorf("Redact 后仍有明文: %s", got)
	}
	if !strings.Contains(got, Mask(access)) || !strings.Contains(got, Mask(refresh)) {
		t.Errorf("应替换为掩码形式: %s", got)
	}

	// 不在列表里的串保持原样（本函数不是通用脱敏器）。
	if !strings.Contains(Redact(msg, refresh), "gwa_") {
		t.Error("未传入的凭据不应被替换")
	}
	// 空串与极短串不参与替换。
	fixed := "a test string with the letters the"
	if out := Redact(fixed, "", "the"); out != fixed {
		t.Errorf("短串不应参与替换: %q", out)
	}
	// 大小写敏感：凭据是 Base64/前缀串，不做大小写归一。
	if out := Redact("gwa_AAAAAAAAAAAAAAAAAAAA", strings.ToLower(access)); out == "" {
		t.Error("不应 panic")
	}
}
