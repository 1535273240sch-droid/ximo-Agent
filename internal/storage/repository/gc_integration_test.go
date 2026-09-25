package repository

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/checkpoint"
)

// TestGCIntegrationWithRealStore 用真实 repository.CheckpointRepo 作为
// checkpoint.GCStore，验证 DB 登记、manifest 文件、blob 三者被一致回收。
func TestGCIntegrationWithRealStore(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()

	cpRoot := filepath.Join(t.TempDir(), "cp")
	cpStore, err := checkpoint.NewStore(cpRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour).UnixMilli()

	// 两个 run：旧 run（应被 GC）+ 新 run（应保留）
	mkRun := func(id string, createdAt int64, status string) string {
		t.Helper()
		if err := r.Sessions.Create(ctx, &Session{ID: "s-" + id, Mode: "default"}); err != nil {
			t.Fatalf("session: %v", err)
		}
		created, err := r.Runs.Create(ctx, &Run{ID: id, SessionID: "s-" + id, Status: status, CreatedAt: createdAt, UpdatedAt: createdAt})
		if err != nil || !created {
			t.Fatalf("run create: %v", err)
		}
		return id
	}
	mkRun("run-old", old, RunStatusCompleted)
	mkRun("run-new", now.UnixMilli(), RunStatusCompleted)

	// 旧 run 打两个 checkpoint，新 run 打一个
	capture := func(runID, turn string, content string) *checkpoint.Manifest {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, runID+"-"+turn+".txt")
		if err := writeTestFile(p, content); err != nil {
			t.Fatalf("write file: %v", err)
		}
		m, err := cpStore.Capture(ctx, runID, "s-"+runID, turn, "", []string{p})
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		m.CreatedAt = now.UnixMilli()
		if runID == "run-old" {
			m.CreatedAt = old
		}
		if err := r.Checkpoints.Record(ctx, m, false); err != nil {
			t.Fatalf("Record: %v", err)
		}
		return m
	}
	oldM1 := capture("run-old", "t1", "old-1")
	oldM2 := capture("run-old", "t2", "old-2")
	newM := capture("run-new", "t1", "new-1")

	// GC 前：3 个 manifest、3 个 blob
	if got := countManifests(t, cpStore); got != 3 {
		t.Fatalf("manifests before GC = %d, want 3", got)
	}
	blobsBefore, err := cpStore.CAS().ListHashes(ctx)
	if err != nil {
		t.Fatalf("ListHashes: %v", err)
	}

	// 只保留最近 1 个 run → run-old 的两个 checkpoint 应被回收
	res, err := cpStore.GC(ctx, r.Checkpoints, checkpoint.GCConfig{
		KeepRecentRuns: 1,
		RecoveryWindow: 24 * time.Hour,
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(res.SweptManifests) != 2 {
		t.Fatalf("swept manifests = %v, want 2 (run-old's)", res.SweptManifests)
	}

	// manifest 文件没了
	if _, err := cpStore.Load(ctx, oldM1.ID); err == nil {
		t.Error("old manifest 1 file still present")
	}
	if _, err := cpStore.Load(ctx, oldM2.ID); err == nil {
		t.Error("old manifest 2 file still present")
	}
	if _, err := cpStore.Load(ctx, newM.ID); err != nil {
		t.Errorf("new manifest must survive: %v", err)
	}

	// DB 登记同步删除，只剩新 run 的
	list, err := r.Checkpoints.ListByRun(ctx, "run-old", 0)
	if err != nil {
		t.Fatalf("list old: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("run-old checkpoints in DB = %d, want 0", len(list))
	}
	list, err = r.Checkpoints.ListByRun(ctx, "run-new", 0)
	if err != nil {
		t.Fatalf("list new: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("run-new checkpoints in DB = %d, want 1", len(list))
	}

	// blob 回收：旧的两个没了，新的还在
	blobsAfter, err := cpStore.CAS().ListHashes(ctx)
	if err != nil {
		t.Fatalf("ListHashes after: %v", err)
	}
	if len(blobsAfter) != 1 {
		t.Errorf("blobs after GC = %d, want 1", len(blobsAfter))
	}
	if len(blobsBefore) != 3 {
		t.Errorf("blobs before GC = %d, want 3", len(blobsBefore))
	}
	if !cpStore.CAS().Has(ctx, newM.Files[0].Ref) {
		t.Error("new run's blob must survive GC")
	}
	if cpStore.CAS().Has(ctx, oldM1.Files[0].Ref) {
		t.Error("old run's blob must be swept")
	}

	// 恢复新 manifest 仍然可用（内容完好）
	res2, err := cpStore.RestoreTo(ctx, newM.ID, checkpoint.RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreTo: %v", err)
	}
	if len(res2.Written) != 1 || len(res2.Conflicts) != 0 {
		t.Errorf("restore after GC = %s, want 1 written 0 conflicts", res2.Summary())
	}
}

func countManifests(t *testing.T, s *checkpoint.Store) int {
	t.Helper()
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(list)
}

func writeTestFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
