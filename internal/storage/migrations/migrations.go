// Package migrations 实现带版本号的 schema migration（第30章 / 第45章
// “所有 DB schema 有 migration version”）。
//
// 迁移文件是纯 SQL，命名规则 NNNN_name.sql（版本号从 1 开始，四位以上数字）。
// SQL 的单一来源是仓库根 migrations/ 目录；本包通过 fs.FS 注入读取，
// 因此同一份文件既能被开发时从磁盘读取，也能被嵌入二进制（embed.FS）。
package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// Migration 是一个已解析的迁移文件。
type Migration struct {
	Version  int
	Name     string // 不含版本号前缀与 .sql 后缀，如 "init"
	SQL      string
	Checksum string // sha256(SQL 原文)
}

var nameRE = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_-]+)\.sql$`)

// bootstrapMigrations 建 migrations 登记表自身（不属于任何版本化迁移，
// 是版本机制的地基，必须存在于应用第一个迁移文件之前）。
const bootstrapMigrations = `CREATE TABLE IF NOT EXISTS migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
)`

// Runner 按版本顺序应用迁移。
type Runner struct {
	db         *sqlite.DB
	migrations []Migration
}

// NewRunner 创建 Runner（尚未加载迁移文件）。
func NewRunner(db *sqlite.DB) *Runner { return &Runner{db: db} }

// Load 从 fsys 读取全部迁移文件（按版本号排序），会做基本校验：
// 版本号唯一、从 1 开始连续、无空洞（防止漏文件导致部分环境缺表）。
func (r *Runner) Load(fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("migrations: read dir: %w", err)
	}
	var ms []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := nameRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue // 忽略非迁移文件（README 等）
		}
		version, err := strconv.Atoi(m[1])
		if err != nil || version <= 0 {
			return fmt.Errorf("migrations: bad version in %q", e.Name())
		}
		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return fmt.Errorf("migrations: read %q: %w", e.Name(), err)
		}
		sqlText := string(raw)
		ms = append(ms, Migration{
			Version:  version,
			Name:     m[2],
			SQL:      sqlText,
			Checksum: checksum(sqlText),
		})
	}
	if len(ms) == 0 {
		return fmt.Errorf("migrations: no migration files found")
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	for i, m := range ms {
		if m.Version != i+1 {
			return fmt.Errorf("migrations: version gap: expected %d, got %d (%s)", i+1, m.Version, m.Name)
		}
	}
	r.migrations = ms
	return nil
}

// Loaded 返回已加载的迁移列表。
func (r *Runner) Loaded() []Migration { return r.migrations }

// CurrentVersion 返回数据库当前已应用的最高版本（0 = 空库）。
func (r *Runner) CurrentVersion(ctx context.Context) (int, error) {
	if err := r.bootstrap(ctx); err != nil {
		return 0, err
	}
	var v int
	row := r.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM migrations")
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("migrations: current version: %w", err)
	}
	return v, nil
}

// Pending 返回尚未应用的迁移。
func (r *Runner) Pending(ctx context.Context) ([]Migration, error) {
	cur, err := r.CurrentVersion(ctx)
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, m := range r.migrations {
		if m.Version > cur {
			out = append(out, m)
		}
	}
	return out, nil
}

// Apply 应用所有待执行迁移。每个迁移在独立事务内执行：SQL + 登记行 +
// PRAGMA user_version 一起提交，中途失败则整体回滚，不会留下
// “应用了一半”的 schema。返回本次应用的版本列表。
func (r *Runner) Apply(ctx context.Context) ([]int, error) {
	if len(r.migrations) == 0 {
		return nil, fmt.Errorf("migrations: no migrations loaded")
	}
	if err := r.bootstrap(ctx); err != nil {
		return nil, err
	}
	cur, err := r.CurrentVersion(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.verifyApplied(ctx); err != nil {
		return nil, err
	}

	var applied []int
	for _, m := range r.migrations {
		if m.Version <= cur {
			continue
		}
		if err := r.applyOne(ctx, m); err != nil {
			return applied, fmt.Errorf("migrations: apply %04d_%s: %w", m.Version, m.Name, err)
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

func (r *Runner) applyOne(ctx context.Context, m Migration) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO migrations (version, name, checksum, applied_at) VALUES (?,?,?,?)",
			m.Version, m.Name, m.Checksum, time.Now().UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", m.Version)); err != nil {
			return err
		}
		return nil
	})
}

// verifyApplied 校验已登记迁移的 checksum 与当前文件一致（防止文件被
// 改过却没人发现，导致不同环境 schema 漂移）。
//
// 校验和对换行形态免疫：Windows 检出（autocrlf）会把 LF 转成 CRLF，仓库里
// 同一份 SQL 在不同构建下字节不同但语义完全一致。旧版本把「构建时原文哈希」
// 写进数据库后，换行形态不同的新包会被误判为内容漂移并拒绝启动（表现为
// engine 无限重启）。这里同时接受 LF 与 CRLF 两种形态的哈希；真正的内容
// 改动仍然会命中错误并 fail-fast。
func (r *Runner) verifyApplied(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, "SELECT version, name, checksum FROM migrations")
	if err != nil {
		return fmt.Errorf("migrations: read applied: %w", err)
	}
	defer rows.Close()
	byVersion := make(map[int]Migration, len(r.migrations))
	for _, m := range r.migrations {
		byVersion[m.Version] = m
	}
	for rows.Next() {
		var version int
		var name, sum string
		if err := rows.Scan(&version, &name, &sum); err != nil {
			return fmt.Errorf("migrations: scan applied: %w", err)
		}
		m, ok := byVersion[version]
		if !ok {
			return fmt.Errorf("migrations: applied version %d (%s) has no file", version, name)
		}
		variants := lineEndingChecksums(m.SQL)
		if !slices.Contains(variants, sum) {
			return fmt.Errorf("migrations: checksum mismatch for %04d_%s: db=%s file=%s", version, name, sum, m.Checksum)
		}
	}
	return rows.Err()
}

// lineEndingChecksums 返回同一 SQL 在 LF 与 CRLF 两种换行形态下的 sha256。
// 统一先归一成 LF 再派生两种形态，保证对混合换行文件也稳定。
func lineEndingChecksums(sqlText string) []string {
	norm := strings.ReplaceAll(sqlText, "\r\n", "\n")
	return []string{
		checksum(norm),
		checksum(strings.ReplaceAll(norm, "\n", "\r\n")),
	}
}

func (r *Runner) bootstrap(ctx context.Context) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, bootstrapMigrations)
		return err
	})
}

// ApplyFromDir 是从目录加载并应用迁移的便捷函数（dir 通常是仓库根的
// "migrations"）。嵌入二进制的场景可自行构造 embed.FS 后调 Load+Apply。
func ApplyFromDir(ctx context.Context, db *sqlite.DB, dir string) ([]int, error) {
	r := NewRunner(db)
	if err := r.Load(os.DirFS(dir)); err != nil {
		return nil, err
	}
	return r.Apply(ctx)
}

func checksum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
