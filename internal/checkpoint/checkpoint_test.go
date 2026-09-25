package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "cp"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ---------------------------------------------------------------- CAS

func TestCASPutGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	content := []byte("hello checkpoint")
	ref, err := s.CAS().Put(ctx, content, "text/plain", 0o644)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", ref.Size, len(content))
	}
	if ref.MediaType != "text/plain" {
		t.Errorf("media type = %q", ref.MediaType)
	}
	got, err := s.CAS().Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
	// blob 落在 blobs/<sha256>
	wantPath := filepath.Join(s.BlobDir(), ref.HashHex())
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("blob file missing at %s: %v", wantPath, err)
	}
}

func TestCASDedup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ref1, err := s.CAS().Put(ctx, []byte("same"), "text/plain", 0o644)
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	ref2, err := s.CAS().Put(ctx, []byte("same"), "text/plain", 0o644)
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if ref1.HashHex() != ref2.HashHex() {
		t.Fatal("same content must map to the same hash")
	}
	hashes, err := s.CAS().ListHashes(ctx)
	if err != nil {
		t.Fatalf("ListHashes: %v", err)
	}
	if len(hashes) != 1 {
		t.Errorf("blob count = %d, want 1 (dedup)", len(hashes))
	}
}

func TestCASDetectsCorruption(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ref, err := s.CAS().Put(ctx, []byte("original"), "text/plain", 0o644)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Tamper with the blob on disk.
	if err := os.WriteFile(s.CAS().Path(ref), []byte("tampered!!"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := s.CAS().Get(ctx, ref); err == nil {
		t.Fatal("Get must fail on hash mismatch (corrupt blob)")
	}
	if err := s.CAS().Verify(ctx, ref); err == nil {
		t.Error("Verify must fail on hash mismatch")
	}
}

func TestCASAtomicWriteLeavesNoTempFiles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if _, err := s.CAS().Put(ctx, []byte{byte(i), byte(i + 1)}, "application/octet-stream", 0o644); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(s.BlobDir())
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != "" && e.Name() != ".tmpkeep" {
			// 允许隐藏临时文件存在一瞬间，但稳态下不应有残留
			if len(e.Name()) > 4 && e.Name()[:4] == ".tmp" {
				t.Errorf("leftover temp file: %s", e.Name())
			}
		}
	}
	if len(entries) != 20 {
		t.Errorf("blob files = %d, want 20", len(entries))
	}
}

func TestCASPutFileAndDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "src.go")
	writeFile(t, p, "package main")
	ref, err := s.CAS().PutFile(ctx, p)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if ref.MediaType != "text/x-source" {
		t.Errorf("media type = %q, want text/x-source", ref.MediaType)
	}
	if err := s.CAS().Delete(ctx, ref.Hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.CAS().Has(ctx, ref) {
		t.Error("blob still present after delete")
	}
	// 删除不存在的 blob 不报错
	if err := s.CAS().Delete(ctx, ref.Hash); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

// ---------------------------------------------------------------- manifest

func TestManifestSaveLoadList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ref, err := s.CAS().Put(ctx, []byte("data"), "text/plain", 0o644)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	id, err := s.Save(ctx, "run-1", []BlobRef{ref})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	refs, err := s.Restore(ctx, id)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(refs) != 1 || refs[0].HashHex() != ref.HashHex() {
		t.Fatalf("restore refs = %+v", refs)
	}
	m, err := s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.RunID != "run-1" || len(m.Files) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	list, err := s.ListByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d, want 1", len(list))
	}
	// manifest 文件确实在 manifests/<id>.json
	if _, err := os.Stat(filepath.Join(s.ManifestDir(), id+".json")); err != nil {
		t.Fatalf("manifest file missing: %v", err)
	}
	// 非法 ID 被拒绝
	if err := s.Delete(ctx, "../escape"); err == nil {
		t.Error("Delete with path traversal must fail")
	}
}

func TestManifestBoundsAndDeleteRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "v1")

	// 两个轮次，各带 MsgIndex 边界
	m1, err := s.Capture(ctx, "run-1", "sess-1", "turn-1", "", []string{p})
	if err != nil {
		t.Fatalf("Capture 1: %v", err)
	}
	m1.MsgIndex = 0
	if err := s.SaveManifest(m1); err != nil {
		t.Fatalf("SaveManifest 1: %v", err)
	}
	writeFile(t, p, "v2")
	m2, err := s.Capture(ctx, "run-1", "sess-1", "turn-2", "", []string{p})
	if err != nil {
		t.Fatalf("Capture 2: %v", err)
	}
	m2.MsgIndex = 4
	if err := s.SaveManifest(m2); err != nil {
		t.Fatalf("SaveManifest 2: %v", err)
	}

	bounds, err := s.Bounds(ctx, "run-1")
	if err != nil {
		t.Fatalf("Bounds: %v", err)
	}
	if bounds["turn-1"] != 0 || bounds["turn-2"] != 4 {
		t.Errorf("bounds = %v, want turn-1:0 turn-2:4", bounds)
	}

	// 会话级清理
	n, err := s.DeleteRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}
	list, err := s.ListByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("manifests after DeleteRun = %d, want 0", len(list))
	}
}

func TestManifestSaveRejectsMissingBlob(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Save(context.Background(), "run-1", []BlobRef{{Size: 3}})
	if err == nil {
		t.Fatal("Save must reject refs whose blob is missing from CAS")
	}
}

func TestCaptureRecordsFingerprintAndNonexistentFiles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	existing := filepath.Join(dir, "a.txt")
	writeFile(t, existing, "v1")
	missing := filepath.Join(dir, "b.txt")

	m, err := s.Capture(ctx, "run-1", "sess-1", "turn-1", "label", []string{existing, missing})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(m.Files))
	}
	if !m.Files[0].Fingerprint.Exists {
		t.Error("existing file must have Exists=true")
	}
	if m.Files[1].Fingerprint.Exists {
		t.Error("missing file must have Exists=false")
	}
	// CaptureFile 同路径去重
	if err := s.CaptureFile(ctx, m, existing); err != nil {
		t.Fatalf("CaptureFile: %v", err)
	}
	if len(m.Files) != 2 {
		t.Errorf("duplicate capture added a file: %d", len(m.Files))
	}
}

// ---------------------------------------------------------------- conflict

func TestCheckConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "v1")
	fp, err := Fingerprint(p)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	// 未修改：无冲突
	conflict, err := s.CheckConflict(ctx, p, fp)
	if err != nil {
		t.Fatalf("CheckConflict: %v", err)
	}
	if conflict {
		t.Error("unchanged file must not conflict")
	}

	// 被别的 Agent 改过：冲突
	writeFile(t, p, "v2 by someone else")
	conflict, err = s.CheckConflict(ctx, p, fp)
	if err != nil {
		t.Fatalf("CheckConflict 2: %v", err)
	}
	if !conflict {
		t.Error("modified file must conflict")
	}

	// 快照时不存在、现在被创建：冲突
	fpMissing := FileFingerprint{Path: filepath.Join(dir, "new.txt"), Exists: false}
	writeFile(t, filepath.Join(dir, "new.txt"), "created")
	conflict, err = s.CheckConflict(ctx, fpMissing.Path, fpMissing)
	if err != nil {
		t.Fatalf("CheckConflict 3: %v", err)
	}
	if !conflict {
		t.Error("file created after snapshot must conflict")
	}

	// 快照时存在、现在被删掉：冲突
	writeFile(t, filepath.Join(dir, "gone.txt"), "x")
	fpGone, _ := Fingerprint(filepath.Join(dir, "gone.txt"))
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	conflict, err = s.CheckConflict(ctx, fpGone.Path, fpGone)
	if err != nil {
		t.Fatalf("CheckConflict 4: %v", err)
	}
	if !conflict {
		t.Error("file deleted after snapshot must conflict")
	}

	// 仅 touch（mtime 变、内容同）：不算冲突
	writeFile(t, p, "v1")
	fp2, _ := Fingerprint(p)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	conflict, err = s.CheckConflict(ctx, p, fp2)
	if err != nil {
		t.Fatalf("CheckConflict 5: %v", err)
	}
	if conflict {
		t.Error("touch-only change (same content) must not conflict")
	}
}

// ---------------------------------------------------------------- restore

func TestRestoreToWritesBackAndSkipsConflicts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	unchanged := filepath.Join(dir, "unchanged.txt")
	modified := filepath.Join(dir, "modified.txt")
	created := filepath.Join(dir, "created.txt")
	deleted := filepath.Join(dir, "deleted.txt")
	writeFile(t, unchanged, "u1")
	writeFile(t, modified, "m1")
	writeFile(t, deleted, "d1")

	m, err := s.Capture(ctx, "run-1", "sess-1", "t1", "", []string{unchanged, modified, deleted, created})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// 模拟 Agent 的修改：unchanged 保持原样（应被恢复），其余三个被外部改动
	writeFile(t, modified, "m2-by-agent")
	writeFile(t, created, "c-by-agent")
	if err := os.Remove(deleted); err != nil {
		t.Fatalf("remove: %v", err)
	}

	res, err := s.RestoreTo(ctx, m.ID, RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreTo: %v", err)
	}

	// 未被外部改动的文件正常写回
	if readFile(t, unchanged) != "u1" {
		t.Errorf("unchanged file not restored: %q", readFile(t, unchanged))
	}
	// 被改动的文件必须跳过并记 conflict（绝不能静默覆盖）
	if readFile(t, modified) != "m2-by-agent" {
		t.Errorf("conflicting file was overwritten: %q", readFile(t, modified))
	}
	if len(res.Conflicts) != 3 {
		t.Fatalf("conflicts = %d (%+v), want 3", len(res.Conflicts), res.Conflicts)
	}
	if len(res.Written) != 1 {
		t.Errorf("written = %v, want [unchanged]", res.Written)
	}
	// created.txt 快照时不存在、现在被创建：判为冲突跳过（删除它同样会
	// 破坏别人的数据，不能静默删）
	if _, err := os.Stat(created); err != nil {
		t.Errorf("created-after-snapshot file should be left alone: %v", err)
	}
	// deleted.txt 快照时存在但被删除：也是 conflict，不重建
	if _, err := os.Stat(deleted); !os.IsNotExist(err) {
		t.Errorf("deleted-after-snapshot file should stay deleted, stat err = %v", err)
	}
	if !res.HasConflicts() {
		t.Error("HasConflicts must be true")
	}
}

func TestRestoreToForceOverwrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "v1")
	m, err := s.Capture(ctx, "run-1", "s1", "t1", "", []string{p})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	writeFile(t, p, "v2")
	res, err := s.RestoreTo(ctx, m.ID, RestoreOptions{Force: true})
	if err != nil {
		t.Fatalf("RestoreTo force: %v", err)
	}
	if readFile(t, p) != "v1" {
		t.Errorf("force restore did not overwrite: %q", readFile(t, p))
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("force restore reported conflicts: %+v", res.Conflicts)
	}
}

func TestRestoreToOnlySubset(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	writeFile(t, a, "a1")
	writeFile(t, b, "b1")
	m, err := s.Capture(ctx, "run-1", "s1", "t1", "", []string{a, b})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	writeFile(t, b, "b2") // 只改 b
	res, err := s.RestoreTo(ctx, m.ID, RestoreOptions{Only: []string{a}})
	if err != nil {
		t.Fatalf("RestoreTo: %v", err)
	}
	if readFile(t, a) != "a1" {
		t.Errorf("a not restored: %q", readFile(t, a))
	}
	if readFile(t, b) != "b2" {
		t.Errorf("b should be untouched: %q", readFile(t, b))
	}
	if len(res.Written) != 1 {
		t.Errorf("written = %v, want 1 entry", res.Written)
	}
}
