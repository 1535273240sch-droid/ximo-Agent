package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "import.db")
	store, err := storage.Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := migrations.ApplyFromDir(context.Background(), store.DB(), filepath.Join("..", "..", "..", "migrations")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

// buildLegacyDir 造一份覆盖全部 v1 数据形态的旧数据目录。
func buildLegacyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("settings.json", `{"model":"deepseek-chat","theme":"dark","apiKey":"sk-should-not-be-logged"}`)
	write("conversations.json", `[
		{"id":"conv-1","title":"第一个会话","mode":"default","createdAt":1700000000000,"updatedAt":1700000001000,
		 "messages":[
			{"id":"conv-1-m0","role":"user","content":"你好","timestamp":1700000000000,"tokens":2},
			{"id":"conv-1-m1","role":"assistant","content":"你好！","timestamp":1700000000500,"tokens":3}
		 ]},
		{"id":"conv-2","title":"空会话","mode":"default","createdAt":1700000100000,"updatedAt":1700000100000,"messages":[]}
	]`)
	write("memory/default.md", "# 默认模式记忆\n内容")
	write("memory/coding.md", "# coding 记忆")
	write("skills.json", `[{"id":"sk-builtin-1","name":"commit","description":"提交","steps":[{"tool":"bash"}]}]`)
	write("imported-skills.json", `[{"id":"sk-imported-1","name":"imported","steps":[]}]`)
	write("mcp-config.json", `[
		{"id":"mcp-fs","name":"fs","transport":"stdio","command":"npx","args":["-y","@mcp/fs"],"env":{"K":"V"},"enabled":true},
		{"id":"mcp-web","name":"web","transport":"http","url":"https://example.com/mcp","enabled":true}
	]`)
	write("experts.json", `[{"id":"expert-1","name":"planner","prompt":"you plan","model":"deepseek-chat","tools":["plan"]}]`)
	write("knowledge/default/entries.json", `[
		{"id":"kb-1","title":"知识一","content":"内容一","tags":["a","b"],"source":"manual","createdAt":1700000000000,"updatedAt":1700000000000},
		{"id":"kb-2","title":"知识二","content":"内容二","tags":[],"createdAt":1700000000000,"updatedAt":1700000000000}
	]`)
	write("knowledge/coding/entries.json", `[
		{"id":"kb-3","title":"编码知识","content":"x","tags":["go"],"createdAt":1700000000000,"updatedAt":1700000000000}
	]`)
	write("themes/dark.json", `{"id":"dark","name":"暗色","light":{"--c":"#000"},"dark":{"--c":"#fff"}}`)
	write("backgrounds/bg1.png", "fake-png-bytes")
	write("backgrounds/bg2.mp4", "fake-mp4-bytes")
	write("design-styles/style-1/manifest.json", `{"id":"style-1"}`)
	write("design-styles/style-1/DESIGN.md", "# style")
	write("ui-components/cat-1/comp-1/app.jsx", "export default () => null")
	write("ui-components-catalog.json", `[{"id":"comp-1","category":"cat-1"}]`)
	return dir
}

func TestImportHappyPath(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	legacyDir := buildLegacyDir(t)

	im := New(Config{Store: store, LegacyDir: legacyDir})
	rep, err := im.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Skipped {
		t.Fatal("first run must not be skipped")
	}
	if rep.BackupPath == "" {
		t.Fatal("backup path missing")
	}

	// 备份存在且内容完整
	if _, err := os.Stat(filepath.Join(rep.BackupPath, "settings.json")); err != nil {
		t.Fatalf("backup missing settings.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rep.BackupPath, "memory", "default.md")); err != nil {
		t.Fatalf("backup missing memory: %v", err)
	}

	// checksum 记录
	stat, ok := rep.Sources[filepath.Join(legacyDir, "conversations.json")]
	if !ok || len(stat.Checksum) != 64 || stat.Records != 2 {
		t.Fatalf("conversations source stat = %+v", stat)
	}

	// 各表数量
	want := map[string]int{
		"sessions":          2,
		"messages":          2,
		"skills":            2,
		"mcp_servers":       2,
		"experts":           1,
		"knowledge_entries": 3,
		"kv.settings":       1,
		"kv.memory":         2,
		"kv.themes":         1,
		"kv.assets":         2 + 1 + 1 + 1, // backgrounds + design-styles + ui-components + catalog
	}
	for table, n := range want {
		if rep.Imported[table] != n {
			t.Errorf("imported[%s] = %d, want %d", table, rep.Imported[table], n)
		}
		if !rep.Verified[table] {
			t.Errorf("table %s was not count-verified", table)
		}
	}

	// settings 原样落 kv（含敏感字段也不进日志/事件，只在 kv）
	var settings []byte
	row := store.QueryRowContext(ctx, "SELECT value FROM kv WHERE key='settings'")
	if err := row.Scan(&settings); err != nil {
		t.Fatalf("read settings kv: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(settings, &parsed); err != nil {
		t.Fatalf("settings kv not valid json: %v", err)
	}
	if parsed["model"] != "deepseek-chat" {
		t.Errorf("settings.model = %v", parsed["model"])
	}

	// 会话 + 消息 + synthetic legacy run
	var msgCount, runCount int
	if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE run_id='legacy-conv-1'").Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 2 {
		t.Errorf("conv-1 messages = %d, want 2", msgCount)
	}
	if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE idempotency_key='legacy:conv-1'").Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Errorf("legacy run = %d, want 1", runCount)
	}

	// 知识按 mode 隔离
	var kb int
	if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM knowledge_entries WHERE mode='coding'").Scan(&kb); err != nil {
		t.Fatalf("count knowledge: %v", err)
	}
	if kb != 1 {
		t.Errorf("coding knowledge = %d, want 1", kb)
	}

	// 版本标记已写
	var marker []byte
	if err := store.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", kvMarkerKey).Scan(&marker); err != nil {
		t.Fatalf("marker: %v", err)
	}
	var marked Report
	if err := json.Unmarshal(marker, &marked); err != nil {
		t.Fatalf("marker json: %v", err)
	}
	if marked.Version != MigrationVersion {
		t.Errorf("marker version = %d, want %d", marked.Version, MigrationVersion)
	}

	// 第二次运行：跳过
	rep2, err := im.Run(ctx)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !rep2.Skipped {
		t.Error("second run must be skipped (idempotent)")
	}

	// 旧数据一个字节都没动
	raw, err := os.ReadFile(filepath.Join(legacyDir, "settings.json"))
	if err != nil {
		t.Fatalf("legacy settings: %v", err)
	}
	if string(raw) != `{"model":"deepseek-chat","theme":"dark","apiKey":"sk-should-not-be-logged"}` {
		t.Errorf("legacy settings.json was modified: %s", raw)
	}
}

func TestImportForceRerun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	legacyDir := buildLegacyDir(t)
	im := New(Config{Store: store, LegacyDir: legacyDir, Force: true})
	for i := 0; i < 2; i++ {
		rep, err := im.Run(ctx)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if rep.Skipped {
			t.Fatalf("Run %d skipped", i)
		}
	}
	// 幂等 upsert：行数不翻倍
	var n int
	if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("sessions = %d after 2 forced runs, want 2", n)
	}
}

func TestImportRejectsCorruptSource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	legacyDir := buildLegacyDir(t)
	// 破坏 conversations.json
	if err := os.WriteFile(filepath.Join(legacyDir, "conversations.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	im := New(Config{Store: store, LegacyDir: legacyDir})
	if _, err := im.Run(ctx); err == nil {
		t.Fatal("Run must fail on corrupt source")
	}
	// 什么都没进 DB
	for _, table := range []string{"sessions", "messages", "skills", "mcp_servers", "experts", "knowledge_entries"} {
		var n int
		if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows, want 0 (nothing imported)", table, n)
		}
	}
	// 版本标记未写
	var n int
	if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM kv WHERE key=?", kvMarkerKey).Scan(&n); err != nil {
		t.Fatalf("count marker: %v", err)
	}
	if n != 0 {
		t.Errorf("migration marker written despite failure")
	}
	// 旧数据未动（仍然坏着，但没被"修复"或删除）
	if _, err := os.Stat(filepath.Join(legacyDir, "conversations.json")); err != nil {
		t.Fatalf("legacy file vanished: %v", err)
	}
}

func TestImportRollsBackOnCountMismatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	legacyDir := buildLegacyDir(t)
	// 两个会话引用同一条消息 ID：upsert 后消息行数少于解析数量 →
	// verify counts 必须失败并整体回滚。
	if err := os.WriteFile(filepath.Join(legacyDir, "conversations.json"), []byte(`[
		{"id":"conv-a","mode":"default","createdAt":1,"updatedAt":1,"messages":[
			{"id":"dup-msg","role":"user","content":"a","timestamp":1}]},
		{"id":"conv-b","mode":"default","createdAt":2,"updatedAt":2,"messages":[
			{"id":"dup-msg","role":"user","content":"b","timestamp":2}]}
	]`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	im := New(Config{Store: store, LegacyDir: legacyDir})
	_, err := im.Run(ctx)
	if err == nil {
		t.Fatal("Run must fail when imported row count != parsed count")
	}
	// 整体回滚：连 sessions 都不该有（同一事务）
	for _, table := range []string{"sessions", "messages", "runs"} {
		var n int
		if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s = %d rows, want 0 (whole transaction rolled back)", table, n)
		}
	}
}

func TestImportEmptyLegacyDir(t *testing.T) {
	store := newTestStore(t)
	im := New(Config{Store: store, LegacyDir: t.TempDir()})
	if _, err := im.Run(context.Background()); err == nil {
		t.Fatal("Run must fail when there is no legacy data")
	}
}

func TestImportBackupIsComplete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	legacyDir := buildLegacyDir(t)
	im := New(Config{Store: store, LegacyDir: legacyDir})
	rep, err := im.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 备份里必须包含全部源文件
	for rel := range rep.Sources {
		backupPath := filepath.Join(rep.BackupPath, rel[len(legacyDir)+1:])
		if _, err := os.Stat(backupPath); err != nil {
			t.Errorf("backup missing %s: %v", rel, err)
		}
	}
	// 备份内容与源一致
	src, _ := os.ReadFile(filepath.Join(legacyDir, "settings.json"))
	dst, _ := os.ReadFile(filepath.Join(rep.BackupPath, "settings.json"))
	if string(src) != string(dst) {
		t.Errorf("backup content differs:\n src=%s\n dst=%s", src, dst)
	}
	_ = time.Now
}
