package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileFingerprint identifies a user file at a point in time. Before writing
// back, the restore path compares all four fields: a mismatch means conflict —
// it stops two agents from silently overwriting each other's work (I11).
//
// Alignment (08 ruling D-6): field-for-field identical to
// internal/types.FileFingerprint — ModTime is Unix **milliseconds** (consistent
// with Event.CreatedAt) and the JSON tag is `mod_time`. The Exists field is
// kept (ruling: it distinguishes "file missing" from "size 0").
type FileFingerprint struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"` // Unix milliseconds
	// SHA256 is the strongest of the four checks and the one that survives a
	// same-size, same-mtime rewrite.
	SHA256 [32]byte `json:"sha256"`
	Exists bool     `json:"exists"`
}

// ManifestFile 是 manifest 中的一个文件条目。
type ManifestFile struct {
	Path        string          `json:"path"`
	Ref         BlobRef         `json:"ref"`
	Fingerprint FileFingerprint `json:"fingerprint"`
}

// Manifest 是一个 checkpoint 的全部状态（JSON 落盘在
// manifests/<manifest_id>.json）。
type Manifest struct {
	ID        string `json:"id"`
	RunID     string `json:"runId"`
	SessionID string `json:"sessionId,omitempty"`
	TurnID    string `json:"turnId,omitempty"`
	// MsgIndex 是该轮次开始时的对话消息索引（对话回退边界，v1 CheckpointStore
	// bounds() 的对等物）。
	MsgIndex  int64          `json:"msgIndex,omitempty"`
	Label     string         `json:"label,omitempty"`
	CreatedAt int64          `json:"createdAt"`
	Files     []ManifestFile `json:"files"`
}

// TotalSize 返回 manifest 引用的内容总字节。
func (m *Manifest) TotalSize() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Ref.Size
	}
	return n
}

// Paths 返回 manifest 覆盖的文件路径列表。
func (m *Manifest) Paths() []string {
	out := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	return out
}

// CheckpointStore 是任务02/04 消费的 checkpoint 接口（契约，勿改签名）。
//
// 说明：契约里的 Save/Restore 以 []BlobRef 为参数/返回值（纯内容寻址
// 包），而真实文件恢复需要路径信息——路径感知的能力由 Capture /
// RestoreTo / CheckConflict 提供（BlobRef 本身不含 path，见任务书
// 第11章 BlobRef 定义）。
type CheckpointStore interface {
	Save(ctx context.Context, runID string, files []BlobRef) (string, error)
	Restore(ctx context.Context, manifestID string) ([]BlobRef, error)
	CheckConflict(ctx context.Context, path string, expected FileFingerprint) (bool, error)
}

// Store 是 CheckpointStore 的实现：CAS（内容）+ manifests（元数据）。
type Store struct {
	cas         *CAS
	manifestDir string
	mu          chan struct{} //  manifest 写互斥（单进程内）
}

// NewStore 创建 checkpoint 存储。root 下布局：
//
//	root/blobs/<sha256>          内容寻址 blob
//	root/manifests/<id>.json     manifest
func NewStore(root string) (*Store, error) {
	cas, err := NewCAS(root)
	if err != nil {
		return nil, err
	}
	manifestDir := filepath.Join(root, "manifests")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: create manifest dir: %w", err)
	}
	return &Store{cas: cas, manifestDir: manifestDir, mu: make(chan struct{}, 1)}, nil
}

// CAS 暴露底层内容存储。
func (s *Store) CAS() *CAS { return s.cas }

// BlobDir 返回 blob 目录。
func (s *Store) BlobDir() string { return s.cas.BlobDir() }

// ManifestDir 返回 manifest 目录。
func (s *Store) ManifestDir() string { return s.manifestDir }

// ---------------------------------------------------------------- 契约方法

// Save 把一组 BlobRef 存成 manifest（内容已在 CAS 中）。
// 返回 manifest ID（manifests/<id>.json 的文件名主体）。
func (s *Store) Save(ctx context.Context, runID string, files []BlobRef) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// 校验引用内容确实存在且完好
	for _, ref := range files {
		if !s.cas.Has(ctx, ref) {
			return "", fmt.Errorf("checkpoint: blob %s missing in CAS", ref.HashHex())
		}
	}
	m := &Manifest{
		ID:        newManifestID(),
		RunID:     runID,
		CreatedAt: time.Now().UnixMilli(),
	}
	for _, ref := range files {
		m.Files = append(m.Files, ManifestFile{Path: "", Ref: ref})
	}
	if err := s.writeManifest(m); err != nil {
		return "", err
	}
	return m.ID, nil
}

// Restore 读取 manifest 并返回其中的 BlobRef（逐个校验内容完好）。
func (s *Store) Restore(ctx context.Context, manifestID string) ([]BlobRef, error) {
	m, err := s.Load(ctx, manifestID)
	if err != nil {
		return nil, err
	}
	refs := make([]BlobRef, 0, len(m.Files))
	for _, f := range m.Files {
		if err := s.cas.Verify(ctx, f.Ref); err != nil {
			return nil, fmt.Errorf("checkpoint: manifest %s: %w", manifestID, err)
		}
		refs = append(refs, f.Ref)
	}
	return refs, nil
}

// CheckConflict 对比磁盘文件与期望指纹。不一致（含“应存在却不存在”
// 与“应不存在却存在”）返回 conflict=true。
func (s *Store) CheckConflict(ctx context.Context, path string, expected FileFingerprint) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current, err := Fingerprint(path)
	if err != nil {
		return false, err
	}
	return fingerprintConflict(expected, current), nil
}

// ---------------------------------------------------------------- 路径感知能力

// Capture 把磁盘上的一组文件快照进 CAS，生成带路径与指纹的 manifest。
// 文件不存在时记录 Exists=false 的指纹（回滚时删除该文件，对应 v1 的
// content=null 语义）。
func (s *Store) Capture(ctx context.Context, runID, sessionID, turnID, label string, paths []string) (*Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m := &Manifest{
		ID:        newManifestID(),
		RunID:     runID,
		SessionID: sessionID,
		TurnID:    turnID,
		Label:     label,
		CreatedAt: time.Now().UnixMilli(),
	}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fp, err := Fingerprint(p)
		if err != nil {
			return nil, err
		}
		var ref BlobRef
		if fp.Exists {
			ref, err = s.cas.PutFile(ctx, p)
			if err != nil {
				return nil, err
			}
		}
		m.Files = append(m.Files, ManifestFile{Path: p, Ref: ref, Fingerprint: fp})
	}
	if err := s.writeManifest(m); err != nil {
		return nil, err
	}
	return m, nil
}

// CaptureFile 快照单个文件并加入已有 manifest（v1 snapshot() 对等物：
// 写工具修改前调用，同一路径只记第一次）。
func (s *Store) CaptureFile(ctx context.Context, m *Manifest, path string) error {
	for _, f := range m.Files {
		if f.Path == path {
			return nil // 已快照
		}
	}
	fp, err := Fingerprint(path)
	if err != nil {
		return err
	}
	var ref BlobRef
	if fp.Exists {
		ref, err = s.cas.PutFile(ctx, path)
		if err != nil {
			return err
		}
	}
	m.Files = append(m.Files, ManifestFile{Path: path, Ref: ref, Fingerprint: fp})
	return nil
}

// SaveManifest 落盘一个（可能被追加过文件的）manifest。
func (s *Store) SaveManifest(m *Manifest) error {
	if m.ID == "" {
		m.ID = newManifestID()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().UnixMilli()
	}
	return s.writeManifest(m)
}

// Load 读取 manifest。
func (s *Store) Load(ctx context.Context, manifestID string) (*Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateManifestID(manifestID); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.manifestPath(manifestID))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: load manifest %s: %w", manifestID, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("checkpoint: parse manifest %s: %w", manifestID, err)
	}
	return &m, nil
}

// List 列出全部 manifest（按创建时间升序）。
func (s *Store) List(ctx context.Context) ([]*Manifest, error) {
	entries, err := os.ReadDir(s.manifestDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: list manifests: %w", err)
	}
	var out []*Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		m, err := s.Load(ctx, id)
		if err != nil {
			continue // 跳过损坏的 manifest（不阻塞列表）
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// ListByRun 列出某 run 的 manifest（时间升序）。
func (s *Store) ListByRun(ctx context.Context, runID string) ([]*Manifest, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, m := range all {
		if m.RunID == runID {
			out = append(out, m)
		}
	}
	return out, nil
}

// Delete 删除 manifest 文件（blob 由 GC 负责）。
func (s *Store) Delete(ctx context.Context, manifestID string) error {
	if err := validateManifestID(manifestID); err != nil {
		return err
	}
	if err := os.Remove(s.manifestPath(manifestID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checkpoint: delete manifest %s: %w", manifestID, err)
	}
	return nil
}

// DeleteRun 删除某 run 的全部 manifest（v1 removeCheckpointStore 对等物：
// 会话被删除时清理其检查点；blob 由 GC 回收）。
func (s *Store) DeleteRun(ctx context.Context, runID string) (int, error) {
	manifests, err := s.ListByRun(ctx, runID)
	if err != nil {
		return 0, err
	}
	for _, m := range manifests {
		if err := s.Delete(ctx, m.ID); err != nil {
			return 0, err
		}
	}
	return len(manifests), nil
}

// Bounds 返回 run 的 turnID → MsgIndex 映射（对话回退边界，v1 bounds() 对等物）。
func (s *Store) Bounds(ctx context.Context, runID string) (map[string]int64, error) {
	manifests, err := s.ListByRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(manifests))
	for _, m := range manifests {
		if m.TurnID != "" {
			out[m.TurnID] = m.MsgIndex
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 指纹

// Fingerprint 计算文件的版本指纹。文件不存在时返回 Exists=false 的
// 空指纹（不报错）。
func Fingerprint(path string) (FileFingerprint, error) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileFingerprint{Path: path, Exists: false}, nil
		}
		return FileFingerprint{}, fmt.Errorf("checkpoint: stat %s: %w", path, err)
	}
	if st.IsDir() {
		return FileFingerprint{}, fmt.Errorf("checkpoint: %s is a directory", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return FileFingerprint{}, fmt.Errorf("checkpoint: read %s: %w", path, err)
	}
	sum := sha256.Sum256(content)
	return FileFingerprint{
		Path:    path,
		Size:    st.Size(),
		ModTime: st.ModTime().UnixMilli(),
		SHA256:  sum,
		Exists:  true,
	}, nil
}

// fingerprintConflict 判断两个指纹是否冲突：存在性不同必冲突；
// 都存在时 size 或 sha256 不同即冲突（mtime 仅作参考，内容相同不算
// 冲突——touch 不该阻塞恢复）。
func fingerprintConflict(expected, current FileFingerprint) bool {
	if expected.Exists != current.Exists {
		return true
	}
	if !expected.Exists {
		return false
	}
	if expected.Size != current.Size {
		return true
	}
	return expected.SHA256 != current.SHA256
}

// ---------------------------------------------------------------- 内部

func (s *Store) manifestPath(id string) string {
	return filepath.Join(s.manifestDir, id+".json")
}

func (s *Store) writeManifest(m *Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal manifest: %w", err)
	}
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	if err := atomicWriteFile(s.manifestPath(m.ID), raw, 0o644); err != nil {
		return fmt.Errorf("checkpoint: write manifest %s: %w", m.ID, err)
	}
	return nil
}

func validateManifestID(id string) error {
	if id == "" {
		return errors.New("checkpoint: empty manifest id")
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("checkpoint: invalid manifest id %q", id)
	}
	return nil
}

func newManifestID() string {
	return fmt.Sprintf("cp-%d-%s", time.Now().UnixMilli(), randSuffix())
}

// HexHash 辅助：把 [32]byte 转 hex（日志/DB 用）。
func HexHash(h [32]byte) string { return hex.EncodeToString(h[:]) }
