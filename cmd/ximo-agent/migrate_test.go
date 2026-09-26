package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 本文件的被测对象是 --migrate-only（release/upgrade.ps1 第 4 步与
// release/upgrade.sh 第 4 步调用的正是这条命令）。
//
// 为什么不能只测 runMigrateOnly：脚本的失败模式不是"迁移逻辑写错了"，而是
// **flag 根本不存在**——Go 的 flag 包会打印 `flag provided but not defined:
// -migrate-only` 并以退出码 2 结束，升级脚本据此删掉新版本目录并回滚。
// 这条路径只有真二进制能覆盖，所以这里同时有一个走 exec 的用例。

// repoRootFromCmd 返回仓库根。本包位于 cmd/ximo-agent，所以从包目录上跳两级。
func repoRootFromCmd(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("解析仓库根失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("仓库根 %s 下没有 go.mod（%v）：本测试依赖仓库根的 migrations/ 与 go.mod", root, err)
	}
	return root
}

// countMigrationFiles 数出仓库 migrations/ 下的迁移脚本条数。
//
// 这个数字是本测试的"应然版本数"：它随仓库新增迁移自动调整，因此断言的是
// 「全部待应用迁移都跑完了」这条不变量，而不是写死条数。
func countMigrationFiles(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatalf("读取 migrations/ 失败: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		n++
	}
	if n < 2 {
		t.Fatalf("migrations/ 下只有 %d 个 .sql，期望至少 2", n)
	}
	return n
}

// assertMigratedDB 断言库真的迁移到位，返回最终 schema version：
// migrations 表存在；登记版本与 migrations/ 下的脚本数一致、从 1 起连续；
// user_version 与最高登记版本相同；0001 建立的 runs 表存在。
func assertMigratedDB(t *testing.T, dbPath string, wantFiles int) int {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		t.Fatalf("打开已迁移的库 %s 失败: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	var registry int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migrations'").Scan(&registry); err != nil {
		t.Fatalf("查询 migrations 表失败: %v", err)
	}
	if registry != 1 {
		t.Fatalf("migrations 表不存在（COUNT=%d）：迁移没有落库", registry)
	}

	rows, err := db.QueryContext(ctx, "SELECT version FROM migrations ORDER BY version")
	if err != nil {
		t.Fatalf("读取 migrations 表失败: %v", err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("扫描 version 失败: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 version 失败: %v", err)
	}
	if len(versions) != wantFiles {
		t.Fatalf("已登记版本 = %v，但 migrations/ 下有 %d 个脚本：有迁移没被应用", versions, wantFiles)
	}
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("已登记版本 %v 不连续（index %d = %d，期望 %d）", versions, i, v, i+1)
		}
	}
	last := versions[len(versions)-1]

	var userVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("读取 user_version 失败: %v", err)
	}
	if userVersion != last {
		t.Fatalf("user_version = %d，期望 %d（与 migrations 表最高版本一致）", userVersion, last)
	}

	var runs int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='runs'").Scan(&runs); err != nil {
		t.Fatalf("查询 runs 表失败: %v", err)
	}
	if runs != 1 {
		t.Fatal("0001_init.sql 建立的 runs 表不存在：schema 没有真正落地")
	}
	return last
}

// envWith 返回一份把 XIMO_HOME 覆盖成 home 的环境变量副本。
//
// 刻意先把已有的 XIMO_HOME 摘掉再追加，而不是直接 append：Windows 的环境变量
// 名不区分大小写，同名重复键到底哪个生效不值得赌。
func envWith(home string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "XIMO_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "XIMO_HOME="+home)
}

// TestRunMigrateOnlyAppliesMigrations 覆盖正常路径：临时库里退出码 0、
// migrations 表已建、版本连续且全部脚本都到位，并且重复执行幂等。
func TestRunMigrateOnlyAppliesMigrations(t *testing.T) {
	root := repoRootFromCmd(t)
	wantFiles := countMigrationFiles(t, root)

	home := t.TempDir()
	t.Setenv("XIMO_HOME", home)

	cfg, err := config.LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if rc := runMigrateOnly(cfg); rc != 0 {
		t.Fatalf("runMigrateOnly 退出码 = %d，期望 0", rc)
	}

	dbPath := cfg.ResolveDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("迁移后库文件不存在 %s: %v", dbPath, err)
	}
	last := assertMigratedDB(t, dbPath, wantFiles)
	t.Logf("迁移完成：%s，schema version %d（%d 个迁移脚本）", dbPath, last, wantFiles)

	// 升级脚本可能被重复执行，迁移必须幂等：再跑一次仍然 0，且不重复登记。
	if rc := runMigrateOnly(cfg); rc != 0 {
		t.Fatalf("第二次 runMigrateOnly 退出码 = %d，期望 0", rc)
	}
	if again := assertMigratedDB(t, dbPath, wantFiles); again != last {
		t.Fatalf("第二次迁移后 schema version = %d，期望仍是 %d", again, last)
	}
}

// TestRunMigrateOnlyReturnsNonZeroWhenDatabaseUnusable 覆盖失败路径：
// 库不可用时必须返回非 0，否则升级脚本会把没迁移成功的版本当成成功切上去。
func TestRunMigrateOnlyReturnsNonZeroWhenDatabaseUnusable(t *testing.T) {
	loadCfg := func(t *testing.T) *config.Config {
		t.Helper()
		cfg, err := config.LoadConfig("")
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		return cfg
	}

	t.Run("base dir is a regular file", func(t *testing.T) {
		// <tmp>/blocked 是普通文件，MkdirAll(<tmp>/blocked) 必然失败（ENOTDIR）。
		blocked := filepath.Join(t.TempDir(), "blocked")
		if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("写入占位文件失败: %v", err)
		}
		t.Setenv("XIMO_HOME", blocked)

		cfg := loadCfg(t)
		if rc := runMigrateOnly(cfg); rc == 0 {
			t.Fatalf("基础目录不可创建时 runMigrateOnly 返回 0（库路径 %s）", cfg.ResolveDBPath())
		}
	})

	t.Run("database path is a directory", func(t *testing.T) {
		// 目录本身能创建，但库文件位置被一个目录占住：storage.Open 会失败。
		home := t.TempDir()
		t.Setenv("XIMO_HOME", home)

		cfg := loadCfg(t)
		if err := os.MkdirAll(cfg.ResolveDBPath(), 0o755); err != nil {
			t.Fatalf("创建占位目录失败: %v", err)
		}
		if rc := runMigrateOnly(cfg); rc == 0 {
			t.Fatalf("库文件位置被目录占用时 runMigrateOnly 返回 0（库路径 %s）", cfg.ResolveDBPath())
		}
	})
}

// TestMigrateOnlyFlagIsAcceptedByTheBinary 是升级脚本契约的回归防线：
// 用真二进制执行 release/upgrade.ps1 / upgrade.sh 第 4 步那条命令
// `ximo-agent --role=engine --migrate-only`，断言退出码 0 且库已迁移到位。
//
// flag 未定义时退出码是 2，且 stderr 含 "flag provided but not defined"。
func TestMigrateOnlyFlagIsAcceptedByTheBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实编译 ximo-agent 二进制，-short 下跳过")
	}
	root := repoRootFromCmd(t)
	wantFiles := countMigrationFiles(t, root)

	exe := filepath.Join(t.TempDir(), "ximo-agent")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", exe, "./cmd/ximo-agent")
	build.Dir = root
	build.Env = os.Environ()
	var buildOut bytes.Buffer
	build.Stdout, build.Stderr = &buildOut, &buildOut
	if err := build.Run(); err != nil {
		t.Fatalf("go build ./cmd/ximo-agent 失败（%v）: %s", err, buildOut.String())
	}

	home := t.TempDir()
	runCtx, cancelRun := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelRun()
	cmd := exec.CommandContext(runCtx, exe, "--role=engine", "--migrate-only")
	// 工作目录设为仓库根：迁移目录探测因此一定命中 ./migrations，
	// 与升级脚本的部署形态（二进制同级带 migrations/）互为兜底。
	cmd.Dir = root
	cmd.Env = envWith(home)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	combined := out.String()

	if strings.Contains(combined, "flag provided but not defined") {
		t.Fatalf("--migrate-only 未被识别（flag 包以退出码 2 结束，升级脚本必然回滚）:\n%s", combined)
	}
	if runErr != nil {
		t.Fatalf("`ximo-agent --role=engine --migrate-only` 失败（%v）:\n%s", runErr, combined)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("退出码 = %d，期望 0:\n%s", code, combined)
	}
	t.Logf("退出码 0，输出：\n%s", strings.TrimSpace(combined))

	assertMigratedDB(t, filepath.Join(home, "data", "ximo-agent.db"), wantFiles)
}
