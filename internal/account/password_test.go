package account

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

func TestHashPasswordFormatAndRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 4 {
		t.Fatalf("哈希串格式错误: %q", h)
	}
	if parts[0] != "pbkdf2-sha256" {
		t.Errorf("算法标识 = %q, 期望 pbkdf2-sha256", parts[0])
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("迭代次数不可解析: %q", parts[1])
	}
	if iter < 200000 {
		t.Errorf("迭代次数 = %d, 契约要求 >= 200000", iter)
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) != pwdSaltBytes {
		t.Errorf("盐不合法: len=%d err=%v", len(salt), err)
	}
	key, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil || len(key) != pwdKeyBytes {
		t.Errorf("密钥不合法: len=%d err=%v", len(key), err)
	}

	if !VerifyPassword(h, pw) {
		t.Error("正确口令未通过校验")
	}
	if VerifyPassword(h, pw+"x") {
		t.Error("错误口令通过了校验")
	}
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h == h2 {
		t.Error("两次哈希相同，盐没有随机化")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	cases := map[string]string{
		"空串":        "",
		"非哈希串":      "hunter2",
		"段数不足":      "pbkdf2-sha256$210000$c2FsdA==",
		"算法不匹配":     "bcrypt$210000$c2FsdA==$aGFzaA==",
		"迭代次数非数字":   "pbkdf2-sha256$abc$c2FsdA==$aGFzaA==",
		"迭代次数为零":    "pbkdf2-sha256$0$c2FsdA==$aGFzaA==",
		"迭代次数负":     "pbkdf2-sha256$-1$c2FsdA==$aGFzaA==",
		"迭代次数超大":    "pbkdf2-sha256$999999999$c2FsdA==$aGFzaA==",
		"盐非法base64": "pbkdf2-sha256$210000$!!!$aGFzaA==",
		"盐为空":       "pbkdf2-sha256$210000$$aGFzaA==",
		"密钥过短":      "pbkdf2-sha256$210000$c2FsdA==$AAAA",
	}
	for name, h := range cases {
		if VerifyPassword(h, "hunter2") {
			t.Errorf("%s: 损坏的哈希串通过了校验: %q", name, h)
		}
	}
}

func TestPepperChangesVerificationSpace(t *testing.T) {
	const pw = "pw-under-pepper"
	h, err := HashPassword(mixPepper([]byte("server-pepper"), pw))
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(h, mixPepper([]byte("server-pepper"), pw)) {
		t.Error("带 pepper 的口令未通过校验")
	}
	if VerifyPassword(h, pw) {
		t.Error("不带 pepper 的口令通过了校验")
	}
	if VerifyPassword(h, mixPepper([]byte("other-pepper"), pw)) {
		t.Error("错误 pepper 通过了校验")
	}
}

func TestDummyHashCostsSameIterations(t *testing.T) {
	iter, salt, want, ok := parsePasswordHash(dummyHash)
	if !ok {
		t.Fatalf("dummyHash 不是合法哈希串: %q", dummyHash)
	}
	if iter != pwdIterations {
		t.Errorf("dummyHash 迭代次数 = %d, 期望 %d（用于抗账号枚举的等时比较）", iter, pwdIterations)
	}
	if len(salt) == 0 || len(want) != pwdKeyBytes {
		t.Errorf("dummyHash 盐/密钥长度异常: salt=%d key=%d", len(salt), len(want))
	}
	for _, pw := range []string{"", "password", "correct horse battery staple"} {
		if VerifyPassword(dummyHash, pw) {
			t.Errorf("dummyHash 不应匹配任何口令: %q", pw)
		}
	}
}
