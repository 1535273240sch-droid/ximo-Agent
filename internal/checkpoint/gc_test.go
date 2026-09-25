package checkpoint

import (
	"context"
	"testing"
	"time"
)

// fakeManifest 是内存中的 manifest 元数据。
type fakeManifest struct {
	runID     string
	pinned    bool
	active    bool
	createdAt int64
	blobs     []string // 该 manifest 引用的 blob hash（hex）
}

// fakeGCStore 是内存版 GCStore，用于隔离测试 mark→sweep 逻辑。
type fakeGCStore struct {
	manifests map[string]*fakeManifest
	deleted   []string
}

func newFakeGCStore() *fakeGCStore {
	return &fakeGCStore{manifests: map[string]*fakeManifest{}}
}

func (f *fakeGCStore) add(id, runID string, pinned, active bool, createdAt int64) {
	f.manifests[id] = &fakeManifest{runID: runID, pinned: pinned, active: active, createdAt: createdAt}
}

func (f *fakeGCStore) ActiveManifestIDs(ctx context.Context) ([]string, error) {
	var out []string
	for id, m := range f.manifests {
		if m.active {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeGCStore) PinnedManifestIDs(ctx context.Context) ([]string, error) {
	var out []string
	for id, m := range f.manifests {
		if m.pinned {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeGCStore) RecentRunManifestIDs(ctx context.Context, keepRuns int) ([]string, error) {
	if keepRuns <= 0 {
		return nil, nil
	}
	runAt := map[string]int64{}
	for _, m := range f.manifests {
		if at, ok := runAt[m.runID]; !ok || m.createdAt > at {
			runAt[m.runID] = m.createdAt
		}
	}
	var runs []string
	for r := range runAt {
		runs = append(runs, r)
	}
	for i := 0; i < len(runs); i++ {
		for j := i + 1; j < len(runs); j++ {
			if runAt[runs[j]] > runAt[runs[i]] {
				runs[i], runs[j] = runs[j], runs[i]
			}
		}
	}
	if len(runs) > keepRuns {
		runs = runs[:keepRuns]
	}
	keep := map[string]bool{}
	for _, r := range runs {
		keep[r] = true
	}
	var out []string
	for id, m := range f.manifests {
		if keep[m.runID] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeGCStore) UnfinishedRunManifestIDs(ctx context.Context) ([]string, error) {
	var out []string
	for id, m := range f.manifests {
		if len(m.runID) > 11 && m.runID[:11] == "unfinished-" {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeGCStore) ManifestsWithinWindow(ctx context.Context, sinceUnixMS int64) ([]string, error) {
	var out []string
	for id, m := range f.manifests {
		if m.createdAt >= sinceUnixMS {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeGCStore) DeleteManifests(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, ok := f.manifests[id]; ok {
			delete(f.manifests, id)
			f.deleted = append(f.deleted, id)
		}
	}
	return nil
}

func (f *fakeGCStore) ReferencedBlobHashes(ctx context.Context) ([]string, error) {
	seen := map[string]struct{}{}
	for _, m := range f.manifests {
		for _, h := range m.blobs {
			seen[h] = struct{}{}
		}
	}
	var out []string
	for h := range seen {
		out = append(out, h)
	}
	return out, nil
}

func TestGCKeepsMarkedAndSweepsRest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	d30 := now.Add(-30 * 24 * time.Hour).UnixMilli()
	d29 := now.Add(-29 * 24 * time.Hour).UnixMilli()
	d28 := now.Add(-28 * 24 * time.Hour).UnixMilli()

	f := newFakeGCStore()
	// run-old：三个历史 manifest。前两个无任何保留规则覆盖 → 扫掉；
	// 第三个用户 pinned → 保留。
	f.add("cp-old-1", "run-old", false, false, d30)
	f.add("cp-old-2", "run-old", false, false, d29)
	f.add("cp-old-3", "run-old", true, false, d28)
	// run-pin：pinned → 保留
	f.add("cp-pin", "run-pin", true, false, d30)
	// run-recent：最近 1 个 run + 窗口内 → 保留
	f.add("cp-recent", "run-recent", false, true, now.UnixMilli())
	// unfinished-run：未完成 run → 保留
	f.add("cp-unf", "unfinished-run", false, true, d30)
	// run-other：无规则覆盖 → 扫掉
	f.add("cp-other", "run-other", false, false, d30)

	ids := []string{"cp-old-1", "cp-old-2", "cp-old-3", "cp-pin", "cp-recent", "cp-unf", "cp-other"}
	for _, id := range ids {
		m := &Manifest{ID: id, RunID: f.manifests[id].runID, CreatedAt: f.manifests[id].createdAt}
		if err := s.SaveManifest(m); err != nil {
			t.Fatalf("SaveManifest %s: %v", id, err)
		}
		ref, err := s.CAS().Put(ctx, []byte(id), "text/plain", 0o644)
		if err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
		f.manifests[id].blobs = []string{ref.HashHex()}
	}
	// 孤儿 blob（没有任何 manifest 引用）
	orphan, err := s.CAS().Put(ctx, []byte("orphan"), "text/plain", 0o644)
	if err != nil {
		t.Fatalf("Put orphan: %v", err)
	}

	res, err := s.GC(ctx, f, GCConfig{KeepRecentRuns: 1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	swept := map[string]bool{}
	for _, id := range res.SweptManifests {
		swept[id] = true
	}
	if !swept["cp-old-1"] || !swept["cp-old-2"] || !swept["cp-other"] {
		t.Errorf("swept = %v, want cp-old-1, cp-old-2, cp-other", res.SweptManifests)
	}
	if len(res.SweptManifests) != 3 {
		t.Errorf("swept count = %d, want exactly 3", len(res.SweptManifests))
	}
	for _, keep := range []string{"cp-old-3", "cp-pin", "cp-recent", "cp-unf"} {
		if swept[keep] {
			t.Errorf("%s must be kept (retention policy)", keep)
		}
	}

	// manifest 文件同步删除/保留
	for _, id := range []string{"cp-old-1", "cp-old-2", "cp-other"} {
		if _, err := s.Load(ctx, id); err == nil {
			t.Errorf("manifest %s file still present after sweep", id)
		}
	}
	for _, id := range []string{"cp-old-3", "cp-pin", "cp-recent", "cp-unf"} {
		if _, err := s.Load(ctx, id); err != nil {
			t.Errorf("manifest %s must still exist: %v", id, err)
		}
	}

	// 孤儿 blob 被回收；被保留 manifest 引用的 blob 保留
	if s.CAS().Has(ctx, orphan) {
		t.Error("orphan blob must be swept")
	}
	// 3 个 manifest 被扫 → 它们的 blob 也不再被引用，连同孤儿一共 4 个
	if res.SweptBlobs != 4 {
		t.Errorf("swept blobs = %d, want 4 (orphan + 3 swept manifests' blobs)", res.SweptBlobs)
	}
	if res.RemainingBlobs != 4 {
		t.Errorf("remaining blobs = %d, want 4", res.RemainingBlobs)
	}

	// DB 侧删除也发生了
	if len(f.deleted) != 3 {
		t.Errorf("store.DeleteManifests calls = %v, want 3 ids", f.deleted)
	}
}

func TestGCIsNoopWhenNothingToSweep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	f := newFakeGCStore()
	f.add("cp-1", "run-1", false, true, now.UnixMilli())
	m := &Manifest{ID: "cp-1", RunID: "run-1", CreatedAt: now.UnixMilli()}
	if err := s.SaveManifest(m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	ref, err := s.CAS().Put(ctx, []byte("x"), "text/plain", 0o644)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	f.manifests["cp-1"].blobs = []string{ref.HashHex()}

	res, err := s.GC(ctx, f, GCConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(res.SweptManifests) != 0 || res.SweptBlobs != 0 {
		t.Errorf("nothing should be swept: manifests=%v blobs=%d", res.SweptManifests, res.SweptBlobs)
	}
}
