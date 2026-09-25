// Package checkpoint 实现 CAS checkpoint 系统（第11章）：
//   - cas.go：blobs/<sha256> 内容寻址存储，原子写（temp→fsync→rename→fsync父目录）；
//   - manifest.go：manifests/<turn_id>.json + BlobRef/FileFingerprint；
//   - restore.go：恢复 + 文件版本冲突检测（path/size/mtime/sha256）；
//   - gc.go：mark→sweep 垃圾回收。
//
// 功能对等基准是 v1 的 CheckpointStore.ts（轮次快照/按轮次回滚），但存储
// 形态从"临时目录 JSON"升级为内容寻址 + 冲突检测。
package checkpoint

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// BlobRef is the unified binary content reference (chapter 11).
//
// Alignment (08 ruling D-4/D-6): field-for-field identical to
// internal/types.BlobRef (names, types and JSON tags), so the assembly-time
// swap to `type BlobRef = types.BlobRef` is mechanical.
type BlobRef struct {
	Hash      [32]byte `json:"hash"`
	Size      int64    `json:"size"`
	MediaType string   `json:"media_type"`
	Mode      uint32   `json:"mode"`
}

// HashHex renders the hash as lowercase hex — the on-disk directory name under
// blobs/ and the key used in manifests.
func (b BlobRef) HashHex() string { return hex.EncodeToString(b.Hash[:]) }

// IsZero reports whether the ref is unset (used to detect a manifest entry that
// never captured content, e.g. a file that did not exist at snapshot time).
func (b BlobRef) IsZero() bool { return b.Hash == [32]byte{} }

// String implements fmt.Stringer.
func (b BlobRef) String() string { return b.HashHex() }

// ParseHash 解析十六进制 hash。
func ParseHash(s string) ([32]byte, error) {
	var h [32]byte
	if len(s) != 64 {
		return h, fmt.Errorf("checkpoint: bad hash length %d", len(s))
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("checkpoint: bad hash: %w", err)
	}
	copy(h[:], raw)
	return h, nil
}

// CAS 是内容寻址存储：root/blobs/<sha256hex>。
type CAS struct {
	root    string
	blobDir string
	mu      sync.Mutex // 保护同 hash 并发写的去重判断
}

// NewCAS 创建（或打开）root 下的 CAS 存储。
func NewCAS(root string) (*CAS, error) {
	if root == "" {
		return nil, errors.New("checkpoint: empty CAS root")
	}
	blobDir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: create blob dir: %w", err)
	}
	return &CAS{root: root, blobDir: blobDir}, nil
}

// Root 返回存储根目录。
func (c *CAS) Root() string { return c.root }

// BlobDir 返回 blob 目录。
func (c *CAS) BlobDir() string { return c.blobDir }

// Path 返回 blob 的磁盘路径。
func (c *CAS) Path(ref BlobRef) string {
	return filepath.Join(c.blobDir, ref.HashHex())
}

// Put 写入内容并返回引用。内容寻址去重：同 hash 已存在则跳过写入。
// 原子写序列：temp file → write → fsync → rename → fsync 父目录。
func (c *CAS) Put(ctx context.Context, content []byte, mediaType string, mode uint32) (BlobRef, error) {
	if err := ctx.Err(); err != nil {
		return BlobRef{}, err
	}
	sum := sha256.Sum256(content)
	ref := BlobRef{Hash: sum, Size: int64(len(content)), MediaType: mediaType, Mode: mode}

	c.mu.Lock()
	defer c.mu.Unlock()
	final := c.Path(ref)
	if st, err := os.Stat(final); err == nil {
		if st.Size() == ref.Size {
			return ref, nil // 已存在（内容寻址去重）
		}
		// 大小不符 = blob 损坏，重写修复
	}
	if err := atomicWriteFile(final, content, mode); err != nil {
		return BlobRef{}, fmt.Errorf("checkpoint: write blob %s: %w", ref.HashHex(), err)
	}
	return ref, nil
}

// PutFile 从磁盘文件读取内容并写入 CAS。
func (c *CAS) PutFile(ctx context.Context, path string) (BlobRef, error) {
	if err := ctx.Err(); err != nil {
		return BlobRef{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return BlobRef{}, fmt.Errorf("checkpoint: stat %s: %w", path, err)
	}
	if st.IsDir() {
		return BlobRef{}, fmt.Errorf("checkpoint: %s is a directory", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return BlobRef{}, fmt.Errorf("checkpoint: read %s: %w", path, err)
	}
	mediaType := guessMediaType(path)
	return c.Put(ctx, content, mediaType, uint32(st.Mode().Perm()))
}

// Get 按引用读取内容并校验 hash（内容不符返回错误，防静默损坏）。
func (c *CAS) Get(ctx context.Context, ref BlobRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.GetByHash(ctx, ref.Hash)
}

// GetByHash 按 hash 读取并校验。
func (c *CAS) GetByHash(ctx context.Context, hash [32]byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(filepath.Join(c.blobDir, hex.EncodeToString(hash[:])))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read blob %x: %w", hash[:8], err)
	}
	sum := sha256.Sum256(content)
	if sum != hash {
		return nil, fmt.Errorf("checkpoint: blob %x hash mismatch (corrupt)", hash[:8])
	}
	return content, nil
}

// Has 判断 blob 是否存在且大小匹配。
func (c *CAS) Has(ctx context.Context, ref BlobRef) bool {
	st, err := os.Stat(c.Path(ref))
	if err != nil {
		return false
	}
	return st.Size() == ref.Size
}

// Verify 校验 blob 内容与 hash 一致。
func (c *CAS) Verify(ctx context.Context, ref BlobRef) error {
	content, err := os.ReadFile(c.Path(ref))
	if err != nil {
		return fmt.Errorf("checkpoint: verify blob %s: %w", ref.HashHex(), err)
	}
	sum := sha256.Sum256(content)
	if sum != ref.Hash {
		return fmt.Errorf("checkpoint: blob %s hash mismatch", ref.HashHex())
	}
	if ref.Size > 0 && int64(len(content)) != ref.Size {
		return fmt.Errorf("checkpoint: blob %s size mismatch", ref.HashHex())
	}
	return nil
}

// Delete 删除指定 blob（不存在则跳过）。
func (c *CAS) Delete(ctx context.Context, hashes ...[32]byte) error {
	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.Join(c.blobDir, hex.EncodeToString(h[:]))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checkpoint: delete blob %x: %w", h[:8], err)
		}
	}
	return nil
}

// ListHashes 列出 blob 目录下所有 hash（GC sweep 的全集输入）。
func (c *CAS) ListHashes(ctx context.Context) ([][32]byte, error) {
	entries, err := os.ReadDir(c.blobDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: list blobs: %w", err)
	}
	var out [][32]byte
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		h, err := ParseHash(e.Name())
		if err != nil {
			continue // 忽略非 blob 文件（如残留 temp）
		}
		out = append(out, h)
	}
	return out, nil
}

// TotalSize 返回 blob 目录总字节数。
func (c *CAS) TotalSize(ctx context.Context) (int64, error) {
	var total int64
	err := filepath.WalkDir(c.blobDir, func(path string, d os.DirEntry, err error) error {
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
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("checkpoint: total size: %w", err)
	}
	return total, nil
}

// ---------------------------------------------------------------- 原子写

// atomicWriteFile 原子写文件：temp file → write → fsync → rename →
// fsync 父目录。任何一步失败都清理 temp，绝不留下半写文件。
//
// 注意：fsync 父目录在 Windows 上无法对目录句柄执行（FlushFileBuffers
// 拒绝目录），此时忽略该错误——rename 本身在 NTFS 上已是原子操作，
// POSIX 上父目录 fsync 保证目录项持久化。
func atomicWriteFile(finalPath string, content []byte, mode uint32) error {
	dir := filepath.Dir(finalPath)
	tmp, err := os.CreateTemp(dir, ".tmp-blob-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Chmod(os.FileMode(mode).Perm()); err != nil && mode != 0 {
		// chmod 失败不阻断（跨平台 mode 语义差异），仅在有明确 mode 时尝试
		_ = err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	syncDir(dir)
	ok = true
	return nil
}

// syncDir 尽力 fsync 父目录（POSIX 有效；Windows 上为 no-op）。
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync() // Windows 对目录返回错误，忽略
}

// guessMediaType 按扩展名猜媒体类型（只覆盖常见类型，未知返回
// application/octet-stream）。
func guessMediaType(path string) string {
	switch filepath.Ext(path) {
	case ".txt", ".md":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js", ".mjs":
		return "text/javascript"
	case ".ts", ".tsx":
		return "text/typescript"
	case ".go", ".py", ".rs", ".java", ".c", ".h", ".cpp":
		return "text/x-source"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

// randSuffix 生成随机后缀（manifest ID / temp 名用）。
func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", b[:])
	}
	return hex.EncodeToString(b[:])
}
