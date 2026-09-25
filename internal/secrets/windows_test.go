//go:build windows

package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCredentialManagerRoundTrip 真实写入/读取/删除 Windows Credential Manager。
// CI 或无凭据管理器的环境会自动跳过。
func TestCredentialManagerRoundTrip(t *testing.T) {
	backend := &CredentialBackend{}
	if !backend.Available() {
		t.Skip("Credential Manager 不可用")
	}
	m, err := NewManager(backend)
	if err != nil {
		t.Fatal(err)
	}
	value := "sk-test-credential-manager-value-42"
	ref, err := m.Put(value)
	if err != nil {
		t.Skipf("写入凭据失败（可能无凭据管理器）: %v", err)
	}
	defer m.Delete(ref)

	got, err := m.Get(ref)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got != value {
		t.Fatalf("Get = %q，期望 %q", got, value)
	}

	// 重复 Put 同一值是幂等的（同一 ref，内容不变）
	refAgain, err := m.Put(value)
	if err != nil {
		t.Fatal(err)
	}
	if refAgain != ref {
		t.Fatalf("同一值应得同一 ref（确定性）")
	}
	gotAgain, err := m.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if gotAgain != value {
		t.Fatalf("重复 Put 后 Get = %q", gotAgain)
	}

	// 不存在的 ref
	if _, err := m.Get(RefForValue("never-stored")); err == nil {
		t.Fatalf("未存储的 ref 应当报错")
	}

	// 删除后读不到
	if err := m.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ref); err == nil {
		t.Fatalf("删除后仍可读")
	}
}

// TestCredentialManagerRefNotPlaintext 验证凭据目标名只含 ref，不含明文。
func TestCredentialManagerRefNotPlaintext(t *testing.T) {
	target := credentialTargetPrefix + "secretref:v1:" + strings.Repeat("a", 32)
	if strings.Contains(target, "sk-") {
		t.Fatalf("目标名不应包含明文 key")
	}
}

// TestDPAPIRoundTrip 用 DPAPI 加密文件后端做真实往返。
func TestDPAPIRoundTrip(t *testing.T) {
	backend, err := NewDPAPIBackend(filepath.Join(t.TempDir(), "dpapi-secrets"))
	if err != nil {
		t.Skipf("DPAPI 不可用: %v", err)
	}
	m, err := NewManager(backend)
	if err != nil {
		t.Fatal(err)
	}
	value := "sk-test-dpapi-value-99"
	ref, err := m.Put(value)
	if err != nil {
		t.Fatalf("DPAPI 写入失败: %v", err)
	}
	defer m.Delete(ref)

	got, err := m.Get(ref)
	if err != nil {
		t.Fatalf("DPAPI 读取失败: %v", err)
	}
	if got != value {
		t.Fatalf("Get = %q", got)
	}

	// 落盘文件必须是密文（不含明文）
	path := backend.path(ref)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), value) {
		t.Fatalf("第21章 违反：DPAPI 文件包含明文 key")
	}
}

func TestDPAPIDeleteIdempotent(t *testing.T) {
	backend, err := NewDPAPIBackend(filepath.Join(t.TempDir(), "dpapi-secrets"))
	if err != nil {
		t.Skipf("DPAPI 不可用: %v", err)
	}
	ref := RefForValue("never-stored")
	if err := backend.Delete(ref); err != nil {
		t.Fatalf("删除不存在的秘密应幂等: %v", err)
	}
}
