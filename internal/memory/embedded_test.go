package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newEmbedded(t *testing.T, userID string) *EmbeddedBackend {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Backend = BackendEmbedded
	cfg.UserID = userID
	cfg.AgentID = "a1"
	b, err := NewEmbeddedBackend(filepath.Join(t.TempDir(), "memory.db"), cfg)
	if err != nil {
		t.Fatalf("打开进程内后端: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestEmbeddedWriteSearchListDelete(t *testing.T) {
	ctx := context.Background()
	b := newEmbedded(t, "u1")

	if err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// 同一轮写两次，验证幂等（同内容只留一条）
	turn := []Message{
		{Role: "user", Content: "以后所有界面都用暗色主题，别给我亮色"},
		{Role: "assistant", Content: "好的，记下了"},
	}
	for i := 0; i < 2; i++ {
		recs, err := b.Add(ctx, turn, AddOptions{RunID: "run-1"})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if len(recs) != 1 || recs[0].Memory == "" {
			t.Fatalf("Add 应返回一条记忆，得到 %+v", recs)
		}
		if recs[0].RunID != "run-1" || recs[0].AgentID != "a1" {
			t.Errorf("归属字段丢了: %+v", recs[0])
		}
	}
	if _, err := b.Add(ctx, []Message{{Role: "user", Content: "服务器端口改成 8600"}}, AddOptions{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	all, err := b.GetAll(ctx, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("幂等去重后应有 2 条，得到 %d 条: %+v", len(all), all)
	}

	// 检索：相关那条必须排在前面，且带正分数
	hits, err := b.Search(ctx, "界面主题偏好", SearchOptions{TopK: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("检索应命中至少一条")
	}
	if !strings.Contains(hits[0].Memory, "暗色主题") {
		t.Errorf("首位命中应是主题那条，得到 %q", hits[0].Memory)
	}
	if hits[0].Score <= 0 {
		t.Errorf("命中应带正分数: %+v", hits[0])
	}
	if hits, _ := b.Search(ctx, "量子纠缠实验", SearchOptions{TopK: 3}); len(hits) != 0 {
		t.Errorf("无关查询不该命中: %+v", hits)
	}

	// 删除 + 重复删除报错
	if err := b.Delete(ctx, hits[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := b.Delete(ctx, hits[0].ID); err == nil {
		t.Error("重复删除应报错")
	}
	if rest, _ := b.GetAll(ctx, 10); len(rest) != 1 {
		t.Errorf("删除后应剩 1 条，得到 %d", len(rest))
	}
}

// 同一个库文件里，不同 UserID 的记忆必须互不可见（WHERE 子句的隔离）。
func TestEmbeddedIsolatesUsersInSameFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	mk := func(user string) *EmbeddedBackend {
		cfg := DefaultConfig()
		cfg.Enabled = true
		cfg.Backend = BackendEmbedded
		cfg.UserID = user
		b, err := NewEmbeddedBackend(path, cfg)
		if err != nil {
			t.Fatalf("打开 %s: %v", user, err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	alice, bob := mk("alice"), mk("bob")

	if _, err := alice.Add(ctx, []Message{{Role: "user", Content: "alice 喜欢暗色主题"}}, AddOptions{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if hits, _ := bob.Search(ctx, "暗色主题", SearchOptions{TopK: 5}); len(hits) != 0 {
		t.Errorf("bob 不该读到 alice 的记忆: %+v", hits)
	}
	if all, _ := bob.GetAll(ctx, 5); len(all) != 0 {
		t.Errorf("bob 列表应为空: %+v", all)
	}
	if hits, _ := alice.Search(ctx, "暗色主题", SearchOptions{TopK: 5}); len(hits) != 1 {
		t.Errorf("alice 应命中自己的那条，得到 %+v", hits)
	}
}

func TestBackendSelection(t *testing.T) {
	cases := []struct {
		backend  string
		endpoint string
		want     string
	}{
		{"", "", BackendEmbedded},                   // 留空且没 endpoint → 进程内
		{"", "http://x:1", BackendMem0},             // 留空但有 endpoint → mem0
		{"embedded", "http://x:1", BackendEmbedded}, // 显式优先
		{"mem0", "", BackendMem0},
		{"sqlite", "", BackendEmbedded},
	}
	for _, c := range cases {
		cfg := Config{Enabled: true, Backend: c.backend, Endpoint: c.endpoint}
		if got := cfg.EffectiveBackend(); got != c.want {
			t.Errorf("backend=%q endpoint=%q → %q，想要 %q", c.backend, c.endpoint, got, c.want)
		}
	}
	if !(Config{Enabled: true, Backend: BackendEmbedded}).Active() {
		t.Error("进程内后端打开开关就应算启用")
	}
	if (Config{Enabled: true, Backend: BackendMem0}).Active() {
		t.Error("mem0 缺 endpoint 不该算启用")
	}
	if err := (Config{Enabled: true, Backend: BackendMem0}).Validate(); err == nil {
		t.Error("mem0 缺 endpoint 应校验失败")
	}
	if err := (Config{Enabled: true, Backend: "nonsense"}).Validate(); err == nil {
		t.Error("未知 backend 应校验失败")
	}
	// 关闭状态下任何字段都可以为空（老配置兼容）
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("关闭状态不该校验失败: %v", err)
	}
}
