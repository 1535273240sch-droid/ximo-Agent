package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 测试专用管理令牌：只存在于本文件，绝不出现在任何产品代码或配置里。
const (
	testAdminToken = "test-admin-token-4f9a2c"
	adminHeader    = "X-Admin-Token"
)

// syncBuffer 是并发安全的写入缓冲：日志由服务端 handler goroutine 与测试协程同时写。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testGateway 是一次完整装配（真 SQLite + 真迁移 + 真 HTTP 监听）的网关实例。
type testGateway struct {
	base   string
	dbPath string
	exit   chan int
	cancel context.CancelFunc
	stdout *syncBuffer
	stderr *syncBuffer
}

// startTestGateway 用 --addr 127.0.0.1:0 启动整栈（全新空库），返回已就绪的实例。
func startTestGateway(t *testing.T, extraArgs ...string) *testGateway {
	t.Helper()
	return startGatewayOnDB(t, filepath.Join(t.TempDir(), "gateway.db"), extraArgs...)
}

// stop 取消 context 并要求优雅停机（退出码必须是 0）。
func (g *testGateway) stop(t *testing.T) {
	t.Helper()
	g.cancel()
	select {
	case code := <-g.exit:
		if code != exitOK {
			t.Errorf("优雅停机后退出码 = %d，期望 %d:\n%s", code, exitOK, g.stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Errorf("优雅停机超时:\n%s", g.stderr.String())
	}
}

// waitHealthy 轮询 /v1/health 直到 200（E2E 也用同样的方式等进程就绪）。
func (g *testGateway) waitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(g.base + "/v1/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var parsed struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal(body, &parsed); err != nil {
					t.Fatalf("/v1/health 响应不是 JSON: %s", body)
				}
				if parsed.Status != "ok" {
					t.Fatalf("/v1/health status = %q，期望 ok", parsed.Status)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("/v1/health 未在 15s 内返回 200:\n%s", g.stderr.String())
}

type httpResult struct {
	status int
	body   []byte
}

func (r httpResult) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("响应不是 JSON 对象: %s", r.body)
	}
	return out
}

// do 发一次请求。headers 里的值按原样设置（管理员令牌只在这里出现，不会进日志）。
func (g *testGateway) do(t *testing.T, method, path, body string, headers map[string]string) httpResult {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, g.base+path, reader)
	if err != nil {
		t.Fatalf("构造请求 %s %s 失败: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s %s 响应失败: %v", method, path, err)
	}
	return httpResult{status: resp.StatusCode, body: raw}
}

func adminHeaders() map[string]string {
	return map[string]string{adminHeader: testAdminToken}
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestVersionFlagExitsZero(t *testing.T) {
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	code := runWithContext(context.Background(), []string{"--version"}, stdout, stderr, func(string) {
		t.Error("--version 不应开始监听")
	})
	if code != exitOK {
		t.Fatalf("--version 退出码 = %d，期望 %d (stderr=%s)", code, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ximo-gateway "+version) {
		t.Fatalf("--version 输出缺少版本串: %q", stdout.String())
	}
}

// TestStartupFailsWithoutAdminToken 验证 fail closed：既没有 --admin-token 也没有
// 环境变量时必须拒绝启动，且不留下任何副作用（连数据库文件都不创建）。
func TestStartupFailsWithoutAdminToken(t *testing.T) {
	t.Setenv(envAdminToken, "")
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	stdout, stderr := &syncBuffer{}, &syncBuffer{}

	code := runWithContext(context.Background(), []string{
		"--db", dbPath,
		"--migrations", repoMigrationsDir(t),
	}, stdout, stderr, func(string) { t.Error("缺少管理令牌时不应开始监听") })

	if code == exitOK {
		t.Fatalf("缺少管理令牌仍返回 0，stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "管理令牌") {
		t.Fatalf("错误信息未说明缺少管理令牌: %s", stderr.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("配置校验失败不应创建数据库文件（stat err = %v）", err)
	}
}

// TestMigrateOnlyExitsZero 验证 --migrate-only：不需要管理令牌、退出码 0，
// 且 migrations 表与网关各表都已建好。
func TestMigrateOnlyExitsZero(t *testing.T) {
	t.Setenv(envAdminToken, "")
	dbPath := filepath.Join(t.TempDir(), "migrate-only.db")
	stdout, stderr := &syncBuffer{}, &syncBuffer{}

	code := runWithContext(context.Background(), []string{
		"--migrate-only",
		"--db", dbPath,
		"--migrations", repoMigrationsDir(t),
	}, stdout, stderr, func(string) { t.Error("--migrate-only 不应开始监听") })

	if code != exitOK {
		t.Fatalf("--migrate-only 退出码 = %d，期望 %d (stderr=%s)", code, exitOK, stderr.String())
	}
	db, err := sqlite.Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		t.Fatalf("打开迁移后的库失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for _, table := range []string{"migrations", "gw_users", "gw_usage", "gw_audit"} {
		var n int
		if err := db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
			t.Fatalf("查询表 %s 失败: %v", table, err)
		}
		if n != 1 {
			t.Errorf("迁移后缺少表 %s", table)
		}
	}
	var applied int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM migrations").Scan(&applied); err != nil {
		t.Fatalf("查询 migrations 表失败: %v", err)
	}
	if applied < 3 {
		t.Errorf("已应用迁移数 = %d，期望 >= 3（0001/0002/0003）", applied)
	}
}

// TestDefaultMigrationProbeOrder 验证未指定 --migrations 时的仓库既有探测顺序
// （从仓库根运行 → ./migrations 命中）。
func TestDefaultMigrationProbeOrder(t *testing.T) {
	t.Setenv(envAdminToken, "")
	t.Setenv(envLogLevel, "info")
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	// 先取绝对路径（Chdir 之前），再切到仓库根。
	root := repoRoot(t)
	t.Chdir(root)

	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	code := runWithContext(context.Background(), []string{
		"--migrate-only",
		"--db", dbPath,
	}, stdout, stderr, nil)
	if code != exitOK {
		t.Fatalf("默认迁移探测失败，退出码 = %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "迁移已应用") {
		t.Fatalf("未看到迁移应用日志: %s", stderr.String())
	}
}

// TestHealthAndAuthGates 验证三件硬要求：/v1/health 200、/v1/models 无凭据 401、
// /admin/* 无/错管理令牌 401；同时确认日志里没有管理令牌明文。
func TestHealthAndAuthGates(t *testing.T) {
	t.Setenv(envLogLevel, "info")
	g := startTestGateway(t)

	if resp := g.do(t, http.MethodGet, "/v1/health", "", nil); resp.status != http.StatusOK {
		t.Errorf("/v1/health 状态 = %d，期望 200（body=%s）", resp.status, resp.body)
	}
	if resp := g.do(t, http.MethodGet, "/v1/models", "", nil); resp.status != http.StatusUnauthorized {
		t.Errorf("/v1/models 无凭据状态 = %d，期望 401（body=%s）", resp.status, resp.body)
	}
	if resp := g.do(t, http.MethodGet, "/v1/usage", "", nil); resp.status != http.StatusUnauthorized {
		t.Errorf("/v1/usage 无凭据状态 = %d，期望 401（body=%s）", resp.status, resp.body)
	}
	if resp := g.do(t, http.MethodGet, "/admin/users", "", nil); resp.status != http.StatusUnauthorized {
		t.Errorf("/admin/users 无管理令牌状态 = %d，期望 401（body=%s）", resp.status, resp.body)
	}
	if resp := g.do(t, http.MethodGet, "/admin/users", "", map[string]string{
		adminHeader: "wrong-token",
	}); resp.status != http.StatusUnauthorized {
		t.Errorf("/admin/users 错误令牌状态 = %d，期望 401（body=%s）", resp.status, resp.body)
	}
	// /v1/auth/device 属于无鉴权路由：没带凭据也必须能拿到设备码，否则插件永远登不上。
	device := g.do(t, http.MethodPost, "/v1/auth/device", "", nil)
	if device.status != http.StatusOK {
		t.Errorf("/v1/auth/device 状态 = %d，期望 200（body=%s）", device.status, device.body)
	}
	if code, _ := device.json(t)["device_code"].(string); code == "" {
		t.Errorf("/v1/auth/device 响应缺少 device_code: %s", device.body)
	}

	// 令牌绝不能出现在任何一路输出里（日志最多出现 sha256 前 8 位）。
	for name, buf := range map[string]*syncBuffer{"stderr": g.stderr, "stdout": g.stdout} {
		if strings.Contains(buf.String(), testAdminToken) {
			t.Errorf("%s 里出现了管理令牌明文", name)
		}
	}
	if !strings.Contains(g.stderr.String(), "admin_fingerprint") {
		t.Errorf("启动日志缺少 admin_fingerprint（无法核对令牌）: %s", g.stderr.String())
	}
}

// TestAdminFlowThenUserRoutes 端到端验证装配真的连通了 store/account/quota/catalog：
// 建用户 → 建 API Key → 用 Key 读 /v1/models 与 /v1/usage。
func TestAdminFlowThenUserRoutes(t *testing.T) {
	g := startTestGateway(t)

	createUser := g.do(t, http.MethodPost, "/admin/users",
		`{"username":"alice","password":"Ximo-Test-Pass-2026!","group_id":"default"}`, adminHeaders())
	if createUser.status != http.StatusOK {
		t.Fatalf("创建用户失败: %d %s", createUser.status, createUser.body)
	}
	userID, _ := createUser.json(t)["id"].(string)
	if userID == "" {
		t.Fatalf("创建用户响应没有 id: %s", createUser.body)
	}

	createKey := g.do(t, http.MethodPost, "/admin/keys",
		`{"user_id":"`+userID+`"}`, adminHeaders())
	if createKey.status != http.StatusOK {
		t.Fatalf("创建 API Key 失败: %d %s", createKey.status, createKey.body)
	}
	plain, _ := createKey.json(t)["api_key"].(string)
	if !strings.HasPrefix(plain, "ximo_sk_") {
		t.Fatalf("API Key 明文前缀异常: %q", plain)
	}

	auth := map[string]string{"Authorization": "Bearer " + plain}
	models := g.do(t, http.MethodGet, "/v1/models", "", auth)
	if models.status != http.StatusOK {
		t.Fatalf("/v1/models 带凭据状态 = %d（body=%s）", models.status, models.body)
	}
	if _, ok := models.json(t)["data"]; !ok {
		t.Fatalf("/v1/models 响应缺少 data 字段: %s", models.body)
	}

	usage := g.do(t, http.MethodGet, "/v1/usage", "", auth)
	if usage.status != http.StatusOK {
		t.Fatalf("/v1/usage 带凭据状态 = %d（body=%s）", usage.status, usage.body)
	}

	// 口令登录链（无鉴权路由）也必须通，并且换出的 access token（gwa_）要能过用户态鉴权。
	login := g.do(t, http.MethodPost, "/v1/auth/login",
		`{"username":"alice","password":"Ximo-Test-Pass-2026!"}`, nil)
	if login.status != http.StatusOK {
		t.Fatalf("/v1/auth/login 状态 = %d（body=%s）", login.status, login.body)
	}
	access, _ := login.json(t)["access_token"].(string)
	if !strings.HasPrefix(access, "gwa_") {
		t.Fatalf("access token 前缀异常: %q", access)
	}
	if models := g.do(t, http.MethodGet, "/v1/models", "",
		map[string]string{"Authorization": "Bearer " + access}); models.status != http.StatusOK {
		t.Fatalf("/v1/models 用 access token 状态 = %d（body=%s）", models.status, models.body)
	}

	// 管理态读接口（审计与用户列表）也要通，说明 admin.Deps 的 store 接线正确。
	users := g.do(t, http.MethodGet, "/admin/users", "", adminHeaders())
	if users.status != http.StatusOK {
		t.Fatalf("/admin/users 状态 = %d（body=%s）", users.status, users.body)
	}
	audit := g.do(t, http.MethodGet, "/admin/audit", "", adminHeaders())
	if audit.status != http.StatusOK {
		t.Fatalf("/admin/audit 状态 = %d（body=%s）", audit.status, audit.body)
	}
	if !strings.Contains(string(audit.body), "user.create") {
		t.Errorf("/admin/audit 未记录 user.create: %s", audit.body)
	}
}

// ---------------------------------------------------------------------------
// 路径辅助
// ---------------------------------------------------------------------------

// repoRoot 从当前工作目录向上找含 migrations/0003_gateway.sql 的目录。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "migrations", "0003_gateway.sql")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("向上找不到 migrations/0003_gateway.sql（起点 %s）", dir)
	return ""
}

func repoMigrationsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "migrations")
}
