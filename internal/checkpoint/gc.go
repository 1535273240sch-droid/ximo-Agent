package checkpoint

import (
	"context"
	"fmt"
	"os"
	"time"
)

// GCStore 是 GC 需要的数据库能力（由 storage/repository.CheckpointRepo 实现）。
// checkpoint 包不直接依赖数据库，保持可测试性。
type GCStore interface {
	// ActiveManifestIDs 状态为 active 的 manifest（run 仍在进行时的当前状态）。
	ActiveManifestIDs(ctx context.Context) ([]string, error)
	// PinnedManifestIDs 用户固定（pinned）的 manifest。
	PinnedManifestIDs(ctx context.Context) ([]string, error)
	// RecentRunManifestIDs 最近 N 个 run 的全部 manifest。
	RecentRunManifestIDs(ctx context.Context, keepRuns int) ([]string, error)
	// UnfinishedRunManifestIDs 未完成 run 的全部 manifest。
	UnfinishedRunManifestIDs(ctx context.Context) ([]string, error)
	// ManifestsWithinWindow createdAt >= sinceUnixMS 的 manifest。
	ManifestsWithinWindow(ctx context.Context, sinceUnixMS int64) ([]string, error)
	// DeleteManifests 删除 manifest 的 DB 登记（级联删 checkpoint_blobs）。
	DeleteManifests(ctx context.Context, ids []string) error
	// ReferencedBlobHashes 删除后仍被引用的 blob hash（hex）。
	ReferencedBlobHashes(ctx context.Context) ([]string, error)
}

// GCConfig 控制保留策略。
type GCConfig struct {
	// KeepRecentRuns 保留最近 N 个 run 的全部 checkpoint（默认 10）。
	KeepRecentRuns int
	// RecoveryWindow 保留窗口：该时长内的 manifest 一律保留（默认 7 天）。
	RecoveryWindow time.Duration
	// Now 注入时钟（测试用），nil 取 time.Now。
	Now func() time.Time
}

func (c *GCConfig) withDefaults() GCConfig {
	out := *c
	if out.KeepRecentRuns <= 0 {
		out.KeepRecentRuns = 10
	}
	if out.RecoveryWindow <= 0 {
		out.RecoveryWindow = 7 * 24 * time.Hour
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	return out
}

// GCResult 是一次 GC 的报告。
type GCResult struct {
	MarkedManifests int      // 保留集合中的 manifest 数
	SweptManifests  []string // 删除的 manifest ID
	SweptBlobs      int      // 删除的 blob 数
	FreedBytes      int64    // 释放的字节数（blob 侧）
	RemainingBlobs  int
	RemainingBytes  int64
}

// GC 执行 mark→sweep 垃圾回收。
//
// mark：保留 ① 活跃 manifest（进行中 run 的当前状态）② 用户 pinned
// ③ 未完成 run 的全部 manifest ④ 最近 N 个 run 的全部 manifest
// ⑤ recovery window 内的 manifest。
// sweep：删除不在保留集合中的 manifest（文件 + DB 登记），再删除不再被
// 任何 manifest 引用的 blob 文件。
func (s *Store) GC(ctx context.Context, store GCStore, cfg GCConfig) (*GCResult, error) {
	cfg = cfg.withDefaults()
	res := &GCResult{}

	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}

	// ---------------- mark
	keep := make(map[string]struct{})
	mark := func(ids []string, err error) error {
		if err != nil {
			return err
		}
		for _, id := range ids {
			keep[id] = struct{}{}
		}
		return nil
	}
	if err := mark(store.ActiveManifestIDs(ctx)); err != nil {
		return nil, fmt.Errorf("checkpoint: gc mark active: %w", err)
	}
	if err := mark(store.PinnedManifestIDs(ctx)); err != nil {
		return nil, fmt.Errorf("checkpoint: gc mark pinned: %w", err)
	}
	if err := mark(store.UnfinishedRunManifestIDs(ctx)); err != nil {
		return nil, fmt.Errorf("checkpoint: gc mark unfinished runs: %w", err)
	}
	if err := mark(store.RecentRunManifestIDs(ctx, cfg.KeepRecentRuns)); err != nil {
		return nil, fmt.Errorf("checkpoint: gc mark recent runs: %w", err)
	}
	since := cfg.Now().Add(-cfg.RecoveryWindow).UnixMilli()
	if err := mark(store.ManifestsWithinWindow(ctx, since)); err != nil {
		return nil, fmt.Errorf("checkpoint: gc mark window: %w", err)
	}
	res.MarkedManifests = len(keep)

	// ---------------- sweep manifests
	var toDelete []string
	for _, m := range all {
		if _, ok := keep[m.ID]; !ok {
			toDelete = append(toDelete, m.ID)
		}
	}
	if len(toDelete) > 0 {
		// 先删 DB 登记（级联清理 checkpoint_blobs 行），再删文件。
		if err := store.DeleteManifests(ctx, toDelete); err != nil {
			return nil, fmt.Errorf("checkpoint: gc delete manifests: %w", err)
		}
		for _, id := range toDelete {
			if err := s.Delete(ctx, id); err != nil {
				return nil, err
			}
			res.SweptManifests = append(res.SweptManifests, id)
		}
	}

	// ---------------- sweep blobs
	referenced := make(map[string]struct{})
	refHashes, err := store.ReferencedBlobHashes(ctx)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: gc referenced hashes: %w", err)
	}
	for _, h := range refHashes {
		referenced[h] = struct{}{}
	}
	onDisk, err := s.cas.ListHashes(ctx)
	if err != nil {
		return nil, err
	}
	var orphans [][32]byte
	for _, h := range onDisk {
		if _, ok := referenced[HexHash(h)]; !ok {
			orphans = append(orphans, h)
		}
	}
	for _, h := range orphans {
		if st, err := os.Stat(s.cas.Path(BlobRef{Hash: h})); err == nil {
			res.FreedBytes += st.Size()
		}
	}
	if err := s.cas.Delete(ctx, orphans...); err != nil {
		return nil, fmt.Errorf("checkpoint: gc delete blobs: %w", err)
	}
	res.SweptBlobs = len(orphans)

	remaining, err := s.cas.ListHashes(ctx)
	if err != nil {
		return nil, err
	}
	res.RemainingBlobs = len(remaining)
	if total, err := s.cas.TotalSize(ctx); err == nil {
		res.RemainingBytes = total
	}
	return res, nil
}
