package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSaveCredAtomicAndPrivate 覆盖落盘：原子替换、0600、不留临时文件、
// 已有宽权限文件会被收紧。
func TestSaveCredAtomicAndPrivate(t *testing.T) {
	credPath := testCredPath(t)
	dir := filepath.Dir(credPath)
	mustNoErr(t, os.MkdirAll(dir, 0o700), "建目录")
	// 预置一个权限过宽、内容过期的旧文件：保存后必须被替换并收紧。
	if runtime.GOOS != "windows" {
		mustNoErr(t, os.WriteFile(credPath, []byte(`{"gateway":"https://old.example"}`), 0o644), "预置旧文件")
	}

	c := &Client{BaseURL: "https://gw.example.com/", CredPath: credPath}
	cred := Cred{Gateway: "https://gw.example.com", AccessToken: "gwa_AAAAAAAAAAAAAAAA", RefreshToken: "gwr_RRRRRRRRRRRRRR"}
	mustNoErr(t, c.SaveCred(cred), "SaveCred")

	entries, err := os.ReadDir(dir)
	mustNoErr(t, err, "ReadDir")
	if len(entries) != 1 || entries[0].Name() != credFileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录内容 = %v, 期望只有 %s（不能残留 .cred-*.tmp）", names, credFileName)
	}

	raw, err := os.ReadFile(credPath)
	mustNoErr(t, err, "ReadFile")
	if !strings.Contains(string(raw), cred.AccessToken) {
		t.Error("凭据文件里应保存令牌（该文件本身就是密钥库）")
	}
	var parsed Cred
	mustNoErr(t, json.Unmarshal(raw, &parsed), "解析落盘 JSON")
	if parsed.Gateway != cred.Gateway || parsed.AccessToken != cred.AccessToken || parsed.RefreshToken != cred.RefreshToken {
		t.Errorf("落盘内容与写入的不一致: %+v", parsed)
	}
	if parsed.UpdatedAt == 0 {
		t.Error("UpdatedAt 应被写入")
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(credPath)
		mustNoErr(t, err, "Stat")
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("凭据文件权限 = %04o, 期望 0600（覆盖 0644 的旧文件后也必须收紧）", perm)
		}
		if perm := mustStatPerm(t, dir); perm != 0o700 {
			t.Errorf("凭据目录权限 = %04o, 期望 0700", perm)
		}
	}
}

// TestCheckCredPerm 权限体检：Windows 上无法用权限位表达访问控制，必须报 WARN。
func TestCheckCredPerm(t *testing.T) {
	credPath := testCredPath(t)
	c := &Client{BaseURL: "https://gw.example.com", CredPath: credPath}

	// 文件不存在不算权限问题（doctor 的"是否已登录"是另一项检查）。
	if st, err := c.CredPerm(); err != nil || st.Exists || st.Warning != "" {
		t.Fatalf("未登录时 = %+v/%v, 期望不存在且无警告", st, err)
	}
	mustNoErr(t, c.SaveCred(Cred{Gateway: "https://gw.example.com", AccessToken: "gwa_BBBBBBBBBBBBBBBB"}), "SaveCred")

	st, err := c.CredPerm()
	mustNoErr(t, err, "CredPerm")
	if !st.Exists || st.Path != credPath {
		t.Fatalf("PermStatus = %+v", st)
	}
	if runtime.GOOS == "windows" {
		if st.Secure {
			t.Error("Windows 上无法验证 0600，Secure 应为 false")
		}
		if !strings.Contains(st.Warning, "Windows") || !strings.Contains(st.Warning, "ACL") {
			t.Errorf("Windows 上必须给出 WARN 说明（doctor 报 WARN 用），实际: %q", st.Warning)
		}
		return
	}
	if !st.Secure || st.Warning != "" {
		t.Errorf("Posix 下刚写入的文件应为 0600/无警告: %+v", st)
	}
	// 反向控制：把权限放宽后必须能检出。
	mustNoErr(t, os.Chmod(credPath, 0o644), "Chmod 0644")
	st = CheckCredPerm(credPath)
	if st.Secure || !strings.Contains(st.Warning, "0600") {
		t.Errorf("0644 的文件必须报 WARN 并提示 0600: %+v", st)
	}
}

// TestLoadCredErrors 缺文件与坏 JSON 的区别：前者是"未登录"，后者是错误。
func TestLoadCredErrors(t *testing.T) {
	credPath := testCredPath(t)
	c := &Client{BaseURL: "https://gw.example.com", CredPath: credPath}

	if _, err := c.LoadCred(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("缺文件时 = %v, 期望 ErrNoCredential", err)
	}

	mustNoErr(t, os.MkdirAll(filepath.Dir(credPath), 0o700), "建目录")
	mustNoErr(t, os.WriteFile(credPath, []byte(`{"gateway":`), 0o600), "写坏文件")
	_, err := c.LoadCred()
	if err == nil || errors.Is(err, ErrNoCredential) {
		t.Fatalf("坏 JSON 时 = %v, 期望普通错误（不能当成「未登录」，否则会盖掉用户数据）", err)
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("错误信息应说明是 JSON 解析问题: %v", err)
	}
}

// TestEnsureCredRefreshesOnlyWhenExpired 是自动续期的核心：过期→续期并回写；
// 未过期→一个请求都不发。
func TestEnsureCredRefreshesOnlyWhenExpired(t *testing.T) {
	f := newFakeGateway(t)
	oldAccess, oldRefresh := f.seedTokens()
	credPath := testCredPath(t)
	c := f.client(credPath)

	expired := Cred{
		Gateway:         f.srv.URL,
		AccessToken:     oldAccess,
		RefreshToken:    oldRefresh,
		AccessExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}
	mustNoErr(t, c.SaveCred(expired), "SaveCred")

	refreshed, err := c.EnsureCred(context.Background())
	mustNoErr(t, err, "EnsureCred")
	if refreshed.AccessToken == oldAccess {
		t.Fatal("access token 未轮换")
	}
	if !f.accessValid(refreshed.AccessToken) {
		t.Error("新 access token 未被网关认可")
	}
	// 轮换语义：旧 refresh 必须失效（这正是"不能并发续期"的原因）。
	if f.refreshValid(oldRefresh) {
		t.Error("旧 refresh token 仍然有效：服务端是轮换的，这里说明续期没走真实路径")
	}
	// 回写：磁盘上的 refresh 必须变成新的那个（否则下次启动会用到失效的旧值）。
	onDisk, err := c.LoadCred()
	mustNoErr(t, err, "LoadCred")
	if onDisk.RefreshToken != refreshed.RefreshToken || onDisk.AccessToken != refreshed.AccessToken {
		t.Error("续期结果未回写凭据文件")
	}
	if onDisk.AccessExpiresAt <= time.Now().Unix() {
		t.Error("回写的到期时间应在未来")
	}

	// 未过期：不得再发任何请求（防止无谓烧掉 refresh token）。
	before := f.requestCount()
	again, err := c.EnsureCred(context.Background())
	mustNoErr(t, err, "EnsureCred(第二次)")
	if again.AccessToken != refreshed.AccessToken {
		t.Error("未过期时不应换令牌")
	}
	if got := f.requestCount() - before; got != 0 {
		t.Errorf("未过期时发了 %d 个请求, 期望 0", got)
	}

	// 反向控制：把到期时间挪到 10 秒后（落在 30 秒余量内）→ 必须提前续期。
	soon := refreshed
	soon.AccessExpiresAt = time.Now().Add(10 * time.Second).Unix()
	mustNoErr(t, c.SaveCred(soon), "SaveCred(soon)")
	before = f.requestCount()
	if _, err := c.EnsureCred(context.Background()); err != nil {
		t.Fatalf("余量内应能续期: %v", err)
	}
	if got := f.requestCount() - before; got != 1 {
		t.Errorf("余量内续期的请求数 = %d, 期望 1（RefreshSkew 未生效）", got)
	}
}

// TestEnsureCredRefreshRevoked 续期用的 refresh 已失效时必须明确要求重新登录。
func TestEnsureCredRefreshRevoked(t *testing.T) {
	f := newFakeGateway(t)
	access, refresh := f.seedTokens()
	f.markRefreshRotated(refresh)
	credPath := testCredPath(t)
	c := f.client(credPath)
	mustNoErr(t, c.SaveCred(Cred{
		Gateway: f.srv.URL, AccessToken: access, RefreshToken: refresh,
		AccessExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}), "SaveCred")

	_, err := c.EnsureCred(context.Background())
	if !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("错误 = %v, 期望 ErrCredentialRevoked", err)
	}
	if !strings.Contains(err.Error(), "重新登录") {
		t.Errorf("错误信息应引导用户重新登录: %v", err)
	}
	assertNoSecrets(t, "错误文本", err.Error(), refresh, access)
}

// TestEnsureCredNoRefreshToken 已过期又没有 refresh：明确报错，不发请求。
func TestEnsureCredNoRefreshToken(t *testing.T) {
	f := newFakeGateway(t)
	credPath := testCredPath(t)
	c := f.client(credPath)
	mustNoErr(t, c.SaveCred(Cred{
		Gateway: f.srv.URL, AccessToken: "gwa_EXPIREDTOKENAAAA",
		AccessExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}), "SaveCred")

	_, err := c.EnsureCred(context.Background())
	if !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("错误 = %v, 期望 ErrCredentialExpired", err)
	}
	if f.requestCount() != 0 {
		t.Errorf("请求数 = %d, 期望 0", f.requestCount())
	}
}

// TestEnsureCredAPIKeyShortCircuits 有长期 API Key 时不看令牌、不续期。
func TestEnsureCredAPIKeyShortCircuits(t *testing.T) {
	f := newFakeGateway(t)
	credPath := testCredPath(t)
	c := f.client(credPath)
	key := "ximo_sk_FAKEAPIKEY0001"
	mustNoErr(t, c.SaveCred(Cred{
		Gateway: f.srv.URL, APIKey: key, AccessToken: "gwa_OLDTOKENAAAAAAAA",
		AccessExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}), "SaveCred")

	got, err := c.EnsureCred(context.Background())
	mustNoErr(t, err, "EnsureCred")
	if got.Bearer() != key {
		t.Errorf("Bearer = %s, 期望 API Key（长期凭据优先）", Mask(got.Bearer()))
	}
	if f.requestCount() != 0 {
		t.Errorf("有 API Key 时不应发续期请求，实际 %d 个", f.requestCount())
	}
	if fp := got.Fingerprint(); strings.Contains(fp, key) {
		t.Errorf("Fingerprint 不得含明文: %s", fp)
	}
}

// TestEnsureCredGatewayMismatch 凭据属于别的网关时拒绝使用（不把 A 的令牌发给 B）。
func TestEnsureCredGatewayMismatch(t *testing.T) {
	f := newFakeGateway(t)
	credPath := testCredPath(t)
	c := f.client(credPath)
	mustNoErr(t, c.SaveCred(Cred{
		Gateway: "https://other.example.com", AccessToken: "gwa_OTHERSITETOKENAAAA",
		AccessExpiresAt: time.Now().Add(time.Hour).Unix(),
	}), "SaveCred")

	_, err := c.EnsureCred(context.Background())
	if !errors.Is(err, ErrGatewayMismatch) {
		t.Fatalf("错误 = %v, 期望 ErrGatewayMismatch", err)
	}
	if !strings.Contains(err.Error(), "other.example.com") {
		t.Errorf("错误信息应指出凭据属于哪个网关: %v", err)
	}
	if f.requestCount() != 0 {
		t.Errorf("请求数 = %d, 期望 0（绝不能把令牌发给别的网关）", f.requestCount())
	}
}

// TestEnsureCredPersistFailure 续期成功但回写失败：返回的新凭据本次可用，
// 同时必须把"没能持久化"告诉调用方（新 refresh 只在内存里）。
func TestEnsureCredPersistFailure(t *testing.T) {
	f := newFakeGateway(t)
	access, refresh := f.seedTokens()
	credPath := testCredPath(t)
	c := f.client(credPath)
	mustNoErr(t, c.SaveCred(Cred{
		Gateway: f.srv.URL, AccessToken: access, RefreshToken: refresh,
		AccessExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}), "SaveCred")

	// 在"读到凭据之后、回写之前"（即网关处理续期请求的瞬间）毁掉目录：
	// 用一个同名普通文件占住目录路径，MkdirAll/rename 都会失败。
	f.onRefresh = func() {
		_ = os.RemoveAll(filepath.Dir(credPath))
		_ = os.WriteFile(filepath.Dir(credPath), []byte("not a directory"), 0o600)
	}

	next, err := c.EnsureCred(context.Background())
	if err == nil {
		t.Fatal("回写失败必须返回错误")
	}
	if !strings.Contains(err.Error(), "续期") {
		t.Errorf("错误信息应说明是回写失败: %v", err)
	}
	if next.AccessToken == "" || next.AccessToken == access {
		t.Errorf("返回值应是已续期的新凭据（本次仍可用）: %q", Mask(next.AccessToken))
	}
	if f.refreshValid(refresh) {
		t.Error("服务端应已完成轮换")
	}
}

// TestDefaultCredPath 默认路径形状。
func TestDefaultCredPath(t *testing.T) {
	p, err := DefaultCredPath()
	mustNoErr(t, err, "DefaultCredPath")
	if filepath.Base(p) != "cred.json" || filepath.Base(filepath.Dir(p)) != ".ximo-plugin" {
		t.Errorf("默认路径 = %q, 期望 <home>/.ximo-plugin/cred.json", p)
	}
	home := t.TempDir()
	if got := CredPathIn(home); got != filepath.Join(home, ".ximo-plugin", "cred.json") {
		t.Errorf("CredPathIn = %q", got)
	}
	// 未显式设置 CredPath 时使用默认路径。
	c := &Client{BaseURL: "https://gw.example.com"}
	if resolved, err := c.ResolvedCredPath(); err != nil || resolved != p {
		t.Errorf("ResolvedCredPath = %q/%v, 期望 %q", resolved, err, p)
	}
}

func mustStatPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	mustNoErr(t, err, "Stat "+path)
	return fi.Mode().Perm()
}
