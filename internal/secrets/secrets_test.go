package secrets

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRefForValueDeterministic(t *testing.T) {
	ref1 := RefForValue("sk-abc123")
	ref2 := RefForValue("sk-abc123")
	ref3 := RefForValue("sk-xyz789")
	if ref1 != ref2 {
		t.Fatalf("同一值必须得到同一 ref：%q vs %q", ref1, ref2)
	}
	if ref1 == ref3 {
		t.Fatalf("不同值得到相同 ref")
	}
	if !strings.HasPrefix(ref1, refPrefix) {
		t.Fatalf("ref 前缀错误: %q", ref1)
	}
	if !IsRef(ref1) {
		t.Fatalf("IsRef 拒绝合法 ref: %q", ref1)
	}
	// ref 不能包含明文值
	if strings.Contains(ref1, "sk-abc123") {
		t.Fatalf("ref 泄露了明文值: %q", ref1)
	}
}

func TestIsRef(t *testing.T) {
	if IsRef("") || IsRef("sk-abc") || IsRef("secretref:v1:short") || IsRef("secretref:v2:"+strings.Repeat("a", 32)) {
		t.Fatalf("IsRef 应当拒绝非法 ref")
	}
	if !IsRef(refPrefix + strings.Repeat("a1b2", 8)) {
		t.Fatalf("IsRef 应当接受合法 ref")
	}
}

func TestNewManagerRejectsNilBackend(t *testing.T) {
	if _, err := NewManager(nil); err == nil {
		t.Fatalf("nil backend 必须报错（绝不退化为明文存储）")
	}
}

func TestManagerPutGetDelete(t *testing.T) {
	backend := newMemoryBackend()
	m, err := NewManager(backend)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := m.Put("sk-secret-value")
	if err != nil {
		t.Fatal(err)
	}
	if ref != RefForValue("sk-secret-value") {
		t.Fatalf("ref = %q", ref)
	}
	got, err := m.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret-value" {
		t.Fatalf("Get = %q", got)
	}
	// Put 之后 Redactor 已登记该值（供 I12 过滤）
	if !m.Redactor().ContainsSecret("prefix sk-secret-value suffix") {
		t.Fatalf("Put 后 Redactor 应登记秘密值")
	}
	// 删除后 Redactor 撤销登记
	if err := m.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if m.Redactor().ContainsSecret("sk-secret-value") {
		t.Fatalf("Delete 后 Redactor 应撤销登记")
	}
	if _, err := m.Get(ref); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("删除后 Get 错误 = %v", err)
	}
	// 重复删除幂等
	if err := m.Delete(ref); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}

func TestManagerRejectsEmptyAndInvalidRef(t *testing.T) {
	m, err := NewManager(newMemoryBackend())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(""); err == nil {
		t.Fatalf("空值必须拒绝")
	}
	if _, err := m.MigratePlaintext(""); err == nil {
		t.Fatalf("空明文必须拒绝")
	}
	if _, err := m.Get("not-a-ref"); err == nil {
		t.Fatalf("非法 ref 必须拒绝")
	}
}

func TestManagerMigratePlaintextIdempotent(t *testing.T) {
	backend := newMemoryBackend()
	m, err := NewManager(backend)
	if err != nil {
		t.Fatal(err)
	}
	ref1, err := m.MigratePlaintext("legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := m.MigratePlaintext("legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	if ref1 != ref2 {
		t.Fatalf("迁移必须幂等: %q vs %q", ref1, ref2)
	}
	got, err := m.Get(ref1)
	if err != nil || got != "legacy-key" {
		t.Fatalf("迁移后读取 = %q, %v", got, err)
	}
}

func TestRedactorKnownValueAndPatterns(t *testing.T) {
	r := NewRedactor()
	r.Track("secretref:v1:abc", "sk-my-private-key-123")

	// 已知值替换
	got := r.RedactString("prefix sk-my-private-key-123 suffix")
	if strings.Contains(got, "sk-my-private-key-123") {
		t.Fatalf("已知秘密值未被过滤: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("应包含脱敏标记: %q", got)
	}

	// 高置信度模式（未登记过的值）
	patterns := []string{
		"sk-abcdefghijklmnopqrstuvwxyz012345",
		"AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_1234567890abcdefghijklmnopqrstuvwx",
		"github_pat_11ABCDEFG0abcdefghijkl_1234567890abcdefghijklmnopqrstuvwxyzABCDEF",
		"xoxb-1234567890-abcdefghijkl",
		"glpat-1234567890abcdefghijkl",
		"-----BEGIN RSA PRIVATE KEY-----",
	}
	for _, p := range patterns {
		if got := r.RedactString("token=" + p); strings.Contains(got, p) {
			t.Fatalf("模式 %q 未被过滤: %q", p, got)
		}
	}

	// 普通文本不误伤
	if got := r.RedactString("hello world 12345"); got != "hello world 12345" {
		t.Fatalf("普通文本被误伤: %q", got)
	}
}

func TestRedactorJSON(t *testing.T) {
	r := NewRedactor()
	r.Track("secretref:v1:x", "topsecret")
	data, err := r.RedactJSON([]byte(`{"note":"key=topsecret","api_key":"topsecret","list":["topsecret","plain"],"nested":{"token":"topsecret","ok":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "topsecret") {
		t.Fatalf("JSON 脱敏失败: %s", data)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("脱敏后不是合法 JSON: %v", err)
	}
	// 非 JSON 输入退化为字符串脱敏而不是原样放行
	out, err := r.RedactJSON([]byte(`not json but has topsecret inside`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "topsecret") {
		t.Fatalf("非法 JSON 输入未脱敏: %s", out)
	}
}

func TestRedactorValueAndStruct(t *testing.T) {
	r := NewRedactor()
	r.Track("secretref:v1:y", "hidden-value")

	type inner struct {
		APIKey string
		Note   string
	}
	value := r.RedactValue(map[string]any{
		"a": "contains hidden-value",
		"b": []any{"hidden-value", "plain"},
		"c": inner{APIKey: "hidden-value", Note: "visible"},
		"d": 42,
	})
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("类型 = %T", value)
	}
	if strings.Contains(m["a"].(string), "hidden-value") {
		t.Fatalf("字符串字段未脱敏: %v", m["a"])
	}
	if strings.Contains(m["b"].([]any)[0].(string), "hidden-value") {
		t.Fatalf("切片元素未脱敏: %v", m["b"])
	}
	in := m["c"].(inner)
	if strings.Contains(in.APIKey, "hidden-value") || strings.Contains(in.Note, "hidden-value") {
		t.Fatalf("struct 字段未脱敏: %+v", in)
	}
	if in.Note != "visible" {
		t.Fatalf("非敏感 struct 字段被误伤: %+v", in)
	}
	if m["d"].(int) != 42 {
		t.Fatalf("数字被改变: %v", m["d"])
	}
}

func TestRedactorSensitiveKeyNames(t *testing.T) {
	r := NewRedactor()
	value := r.RedactValue(map[string]any{
		"api_key":       "whatever",
		"API-KEY":       "whatever",
		"secret_token":  "whatever",
		"password":      "whatever",
		"Authorization": "Bearer whatever",
		"username":      "should-stay",
		"content":       "should-stay",
	})
	m := value.(map[string]any)
	for _, k := range []string{"api_key", "API-KEY", "secret_token", "password", "Authorization"} {
		if m[k] != redactedPlaceholder {
			t.Fatalf("敏感键 %q 未脱敏: %v", k, m[k])
		}
	}
	if m["username"] != "should-stay" || m["content"] != "should-stay" {
		t.Fatalf("非敏感键被误伤: %v", m)
	}
}

func TestRedactorUntrackAndContains(t *testing.T) {
	r := NewRedactor()
	r.Track("ref1", "value-one")
	if !r.ContainsSecret("x value-one y") {
		t.Fatalf("ContainsSecret 失败")
	}
	if r.TrackedCount() != 1 {
		t.Fatalf("TrackedCount = %d", r.TrackedCount())
	}
	r.Untrack("ref1")
	if r.ContainsSecret("value-one") {
		t.Fatalf("Untrack 后仍能检测到")
	}
}

func TestSanitizeForLog(t *testing.T) {
	r := NewRedactor()
	r.Track("ref", "multi\nline\nsecret")
	got := r.SanitizeForLog("before multi\nline\nsecret after")
	if strings.Contains(got, "multi\nline\nsecret") {
		t.Fatalf("日志脱敏失败: %q", got)
	}
	if !strings.Contains(got, "\\n") {
		t.Fatalf("换行应被转义: %q", got)
	}
}

func TestManagerWithSecretClosure(t *testing.T) {
	backend := newMemoryBackend()
	m, err := NewManager(backend)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := m.Put("sk-closure-secret")
	if err != nil {
		t.Fatal(err)
	}

	// 闭包内可用明文
	var seen string
	if err := m.WithSecret(ref, func(plaintext string) error {
		seen = plaintext
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != "sk-closure-secret" {
		t.Fatalf("闭包内明文 = %q", seen)
	}

	// 闭包错误原样透传
	sentinel := errors.New("请求失败")
	if err := m.WithSecret(ref, func(string) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("闭包错误未透传: %v", err)
	}

	// 不存在的 ref：闭包不应被执行
	called := false
	if err := m.WithSecret(RefForValue("missing"), func(string) error {
		called = true
		return nil
	}); err == nil {
		t.Fatalf("不存在的 ref 应当报错")
	}
	if called {
		t.Fatalf("ref 不存在时闭包不应执行")
	}
}

func TestRedactorReflectEdgeCases(t *testing.T) {
	// 审核报告 S-1 补充：[]byte / json.RawMessage / 未导出字段 / 嵌套指针
	r := NewRedactor()
	r.Track("secretref:v1:edge", "edge-secret-value")

	type inner struct {
		unexported string // 必须被安全跳过（不 panic）
		Note       string
	}
	type payload struct {
		Raw     []byte
		RawJSON json.RawMessage
		Ptr     *inner
		Nested  []inner
		Map     map[string]string
		APIKey  string
		Ignored int
	}

	value := r.RedactValue(payload{
		Raw:     []byte("bytes with edge-secret-value"),
		RawJSON: json.RawMessage(`{"k":"edge-secret-value"}`),
		Ptr:     &inner{unexported: "edge-secret-value", Note: "keep"},
		Nested:  []inner{{unexported: "edge-secret-value", Note: "keep2"}},
		Map:     map[string]string{"a": "edge-secret-value", "b": "keep3"},
		APIKey:  "edge-secret-value",
		Ignored: 7,
	})
	got, ok := value.(payload)
	if !ok {
		t.Fatalf("类型 = %T", value)
	}
	if strings.Contains(string(got.Raw), "edge-secret-value") {
		t.Fatalf("[]byte 未脱敏: %q", got.Raw)
	}
	if strings.Contains(string(got.RawJSON), "edge-secret-value") {
		t.Fatalf("json.RawMessage 未脱敏: %q", got.RawJSON)
	}
	if strings.Contains(got.Ptr.Note, "edge-secret-value") {
		t.Fatalf("嵌套指针字段未脱敏: %+v", got.Ptr)
	}
	if got.Ptr.Note != "keep" {
		t.Fatalf("嵌套指针普通字段被误伤: %+v", got.Ptr)
	}
	if len(got.Nested) != 1 || got.Nested[0].Note != "keep2" {
		t.Fatalf("结构体切片被破坏: %+v", got.Nested)
	}
	if strings.Contains(got.Map["a"], "edge-secret-value") {
		t.Fatalf("map 值未脱敏: %v", got.Map)
	}
	if got.Map["b"] != "keep3" {
		t.Fatalf("map 普通值被误伤: %v", got.Map)
	}
	if strings.Contains(got.APIKey, "edge-secret-value") {
		t.Fatalf("敏感字段未脱敏: %q", got.APIKey)
	}
	if got.Ignored != 7 {
		t.Fatalf("数字字段被改变: %d", got.Ignored)
	}
}

// ---------------------------------------------------------------------------
// 内存 backend（跨平台测试替身）
// ---------------------------------------------------------------------------

type memoryBackend struct {
	values map[string]string
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{values: map[string]string{}}
}

func (b *memoryBackend) Get(ref string) (string, error) {
	v, ok := b.values[ref]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (b *memoryBackend) Put(ref, value string) error {
	b.values[ref] = value
	return nil
}

func (b *memoryBackend) Delete(ref string) error {
	delete(b.values, ref)
	return nil
}

func (b *memoryBackend) Available() bool { return true }
func (b *memoryBackend) Name() string    { return "memory" }

// ---------------------------------------------------------------------------
// Env backend
// ---------------------------------------------------------------------------

func TestEnvBackend(t *testing.T) {
	value := "env-secret-value"
	ref := RefForValue(value)
	envKey := "XIMO_SECRET_" + strings.ToUpper(strings.TrimPrefix(ref, refPrefix))
	t.Setenv(envKey, value)

	m, err := NewEnvManager()
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ref)
	if err != nil {
		t.Fatalf("读取环境秘密失败: %v", err)
	}
	if got != value {
		t.Fatalf("Get = %q", got)
	}
	// 未设置的环境变量报 not found
	other := RefForValue("missing")
	if _, err := m.Get(other); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("错误 = %v", err)
	}
}

func TestEnvBackendCannotWriteUnset(t *testing.T) {
	b := &EnvBackend{}
	ref := RefForValue("unset-value")
	if err := b.Put(ref, "unset-value"); err == nil {
		t.Fatalf("未预先设置的环境变量不能写入")
	}
}

// ---------------------------------------------------------------------------
// 文件权限（DPAPI 后端目录）
// ---------------------------------------------------------------------------

func TestDPAPIBackendDirPermissions(t *testing.T) {
	// Windows 的目录 ACL 不由 Go 的 Mode() 反映（统一报告 rwxrwxrwx），
	// 权限断言只在 POSIX 平台有意义。
	if runtime.GOOS != "windows" {
		dir := filepath.Join(t.TempDir(), "secrets")
		if _, err := NewDPAPIBackend(dir); err != nil {
			t.Skipf("非 Windows 平台无 DPAPI: %v", err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("秘密目录权限过宽: %v", info.Mode().Perm())
		}
		return
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	backend, err := NewDPAPIBackend(dir)
	if err != nil {
		t.Skipf("DPAPI 不可用: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("秘密目录未创建")
	}
	if backend.Name() != "windows-dpapi" {
		t.Fatalf("Name = %q", backend.Name())
	}
}
