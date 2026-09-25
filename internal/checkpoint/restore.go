package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RestoreResult 描述一次恢复的结果。
type RestoreResult struct {
	// Written 成功写回的文件。
	Written []string
	// Deleted 因快照时不存在而被删除的文件。
	Deleted []string
	// Conflicts 因版本冲突被跳过的文件（内容未被覆盖）。
	Conflicts []Conflict
	// Errors 恢复过程中出错的文件（IO 错误等）。
	Errors []RestoreError
}

// Conflict 是一次文件版本冲突的详情。
type Conflict struct {
	Path     string
	Expected FileFingerprint
	Current  FileFingerprint
}

// RestoreError 是单文件恢复失败。
type RestoreError struct {
	Path string
	Err  error
}

// RestoreOptions 控制恢复行为。
type RestoreOptions struct {
	// Only 只恢复这些路径（空=全部）。
	Only []string
	// Force 跳过冲突检测直接覆盖（默认 false；用户显式确认后才应置 true）。
	Force bool
}

// RestoreTo 把 manifest 的内容写回磁盘（v1 restoreCode 的 CAS 对等物）。
//
// 每个文件写回前都做版本冲突检测：磁盘文件与快照指纹（size/sha256）
// 不一致就跳过并记入 Conflicts，绝不静默覆盖用户/其他 Agent 的修改（I11）。
// 快照时文件不存在（Exists=false）的条目，恢复动作是删除当前文件。
// 写回本身是原子写（temp→fsync→rename→fsync父目录）。
func (s *Store) RestoreTo(ctx context.Context, manifestID string, opts RestoreOptions) (*RestoreResult, error) {
	m, err := s.Load(ctx, manifestID)
	if err != nil {
		return nil, err
	}
	only := make(map[string]struct{}, len(opts.Only))
	for _, p := range opts.Only {
		only[p] = struct{}{}
	}

	res := &RestoreResult{}
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if len(only) > 0 {
			if _, ok := only[f.Path]; !ok {
				continue
			}
		}
		if !opts.Force {
			conflict, err := s.CheckConflict(ctx, f.Path, f.Fingerprint)
			if err != nil {
				res.Errors = append(res.Errors, RestoreError{Path: f.Path, Err: err})
				continue
			}
			if conflict {
				current, _ := Fingerprint(f.Path)
				res.Conflicts = append(res.Conflicts, Conflict{
					Path:     f.Path,
					Expected: f.Fingerprint,
					Current:  current,
				})
				continue
			}
		}
		if !f.Fingerprint.Exists {
			// 快照时文件不存在：删除当前文件（如存在）
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				res.Errors = append(res.Errors, RestoreError{Path: f.Path, Err: err})
				continue
			}
			res.Deleted = append(res.Deleted, f.Path)
			continue
		}
		content, err := s.cas.Get(ctx, f.Ref)
		if err != nil {
			res.Errors = append(res.Errors, RestoreError{Path: f.Path, Err: err})
			continue
		}
		if err := ensureParentDir(f.Path); err != nil {
			res.Errors = append(res.Errors, RestoreError{Path: f.Path, Err: err})
			continue
		}
		if err := atomicWriteFile(f.Path, content, f.Ref.Mode); err != nil {
			res.Errors = append(res.Errors, RestoreError{Path: f.Path, Err: err})
			continue
		}
		res.Written = append(res.Written, f.Path)
	}
	return res, nil
}

// HasConflicts 判断结果中是否有冲突。
func (r *RestoreResult) HasConflicts() bool { return len(r.Conflicts) > 0 }

// Summary 生成人类可读摘要（日志/UI 用）。
func (r *RestoreResult) Summary() string {
	return fmt.Sprintf("written=%d deleted=%d conflicts=%d errors=%d",
		len(r.Written), len(r.Deleted), len(r.Conflicts), len(r.Errors))
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
