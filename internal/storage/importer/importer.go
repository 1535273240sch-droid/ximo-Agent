// Package importer 把 v1（Electron/TS 版）的 JSON 文件存储迁移进 SQLite
// schema（第30章）。
//
// 迁移流程是严格的“全有或全无”：
//
//	backup old data → validate → import transaction → checksum → verify counts → mark version
//
// 任何一步失败（尤其是 verify counts 对不上）都会整体回滚：新数据库的事务
// 回滚、旧数据一个字节都没动过。已迁移过的库不会重复导入（version 标记
// 幂等），除非显式 Force。
package importer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
)

// MigrationVersion 是本导入器的数据迁移版本号（写入 kv 标记）。
const MigrationVersion = 1

// kvMarkerKey 是数据迁移版本标记的 kv 键。
const kvMarkerKey = "migration:legacy_v1"

// ErrAlreadyImported 表示该库已完成过 v1 数据迁移。
var ErrAlreadyImported = errors.New("importer: legacy v1 data already imported")

// SourceStat 描述一个源文件的迁移统计。
type SourceStat struct {
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Checksum string `json:"checksum"`
	Records  int    `json:"records"`
}

// Report 是一次迁移的完整报告。
type Report struct {
	BackupPath string                `json:"backupPath"`
	Sources    map[string]SourceStat `json:"sources"`
	Imported   map[string]int        `json:"imported"`
	Verified   map[string]bool       `json:"verified"`
	Skipped    bool                  `json:"skipped"`
	Version    int                   `json:"version"`
	StartedAt  int64                 `json:"startedAt"`
	FinishedAt int64                 `json:"finishedAt"`
}

// Importer 执行 v1 → v2 数据迁移。
type Importer struct {
	store     *storage.Store
	legacyDir string
	backupDir string // 为空时默认 <legacyDir>.backup-<ts>
	force     bool
	now       func() time.Time
}

// Config 是导入参数。
type Config struct {
	Store     *storage.Store
	LegacyDir string
	// BackupDir 备份目录；为空时默认 <legacyDir>.backup-<timestamp>。
	BackupDir string
	// Force 忽略“已迁移”标记重新导入（已存在行按唯一约束幂等跳过）。
	Force bool
}

// New 创建导入器。
func New(cfg Config) *Importer {
	return &Importer{
		store:     cfg.Store,
		legacyDir: cfg.LegacyDir,
		backupDir: cfg.BackupDir,
		force:     cfg.Force,
		now:       time.Now,
	}
}

// Run 执行迁移。失败时新库事务已回滚、旧数据未动。
func (im *Importer) Run(ctx context.Context) (*Report, error) {
	rep := &Report{
		Sources:  map[string]SourceStat{},
		Imported: map[string]int{},
		Verified: map[string]bool{},
		Version:  MigrationVersion,
	}
	rep.StartedAt = im.now().UnixMilli()

	if !im.force && im.alreadyImported(ctx) {
		rep.Skipped = true
		rep.FinishedAt = im.now().UnixMilli()
		return rep, nil
	}

	// 1) 发现源文件
	sources, err := im.discover()
	if err != nil {
		return rep, err
	}
	if len(sources) == 0 {
		return rep, fmt.Errorf("importer: no legacy data found in %s", im.legacyDir)
	}

	// 2) 备份旧数据（任何文件拷贝失败即中止，此时 DB 尚未被触碰）
	backupDir := im.backupDir
	if backupDir == "" {
		backupDir = fmt.Sprintf("%s.backup-%d", im.legacyDir, im.now().UnixMilli())
	}
	if err := backupTree(ctx, im.legacyDir, backupDir); err != nil {
		return rep, fmt.Errorf("importer: backup failed: %w", err)
	}
	rep.BackupPath = backupDir

	// 3) 解析 + 校验 + checksum
	data, err := im.parseAll(ctx, sources, rep)
	if err != nil {
		return rep, err
	}

	// 4) 单事务导入；5) 同事务内核对数量，对不上返回错误 → 整体回滚
	if err := im.store.WithTx(ctx, func(tx storage.Tx) error {
		return im.importAndVerify(ctx, tx, data, rep)
	}); err != nil {
		return rep, err
	}

	// 6) 标记迁移版本（幂等：后续 Run 直接跳过）
	marker, err := json.Marshal(rep)
	if err != nil {
		return rep, fmt.Errorf("importer: marshal marker: %w", err)
	}
	if err := im.setKV(ctx, kvMarkerKey, marker); err != nil {
		return rep, fmt.Errorf("importer: mark version: %w", err)
	}

	rep.FinishedAt = im.now().UnixMilli()
	return rep, nil
}

// alreadyImported 检查 kv 中的迁移版本标记。
func (im *Importer) alreadyImported(ctx context.Context) bool {
	row := im.store.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", kvMarkerKey)
	var v []byte
	if err := row.Scan(&v); err != nil {
		return false
	}
	var rep Report
	if err := json.Unmarshal(v, &rep); err != nil {
		return false
	}
	return rep.Version >= MigrationVersion
}

func (im *Importer) setKV(ctx context.Context, key string, value []byte) error {
	return im.store.WithTx(ctx, func(tx storage.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, value, im.now().UnixMilli())
		return err
	})
}

// ---------------------------------------------------------------- 备份

// backupTree 把整棵旧数据目录拷到 backupDir，并核对总字节数一致。
func backupTree(ctx context.Context, src, dst string) error {
	var totalBytes int64
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := copyFile(path, target, info.Mode().Perm()); err != nil {
			return err
		}
		totalBytes += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	// 核对备份总字节数
	var backupBytes int64
	err = filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		backupBytes += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	if backupBytes != totalBytes {
		return fmt.Errorf("backup byte mismatch: src=%d dst=%d", totalBytes, backupBytes)
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---------------------------------------------------------------- 工具

func checksumBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readJSONFile 读取并解析 JSON 文件。
func readJSONFile(path string, out any) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return raw, nil
}

// countRows 在事务内统计行数。
func countRows(ctx context.Context, tx txExecer, query string, args ...any) (int, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// txExecer 与 storage 包内部接口一致（事务内 SQL 最小能力集）。
type txExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// normID 规整 v1 的服务器/技能 ID（与 v1 parseSingleServer 同一规则）。
func normID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
