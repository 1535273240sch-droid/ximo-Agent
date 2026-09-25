package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestFileFingerprintMatchesAuthoritativeType 是 D-6 裁决的可执行防线：
// FileFingerprint 必须与 08 权威 internal/types/context.go 的定义一致——
// ModTime 为 Unix 毫秒、JSON tag 为 mod_time、保留 Exists。单位不一致会让
// I11 的冲突检测恒为真或恒为假（静默失效），这是最危险的一类集成缺陷。
func TestFileFingerprintMatchesAuthoritiveType(t *testing.T) {
	want := map[string]string{
		"Path":    "path",
		"Size":    "size",
		"ModTime": "mod_time",
		"SHA256":  "sha256",
		"Exists":  "exists",
	}
	typ := reflect.TypeOf(FileFingerprint{})
	if typ.NumField() != len(want) {
		t.Fatalf("FileFingerprint has %d fields, want %d", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected field %q", f.Name)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("field %s json tag = %q, want %q", f.Name, got, tag)
		}
	}
	if typ.NumField() < 5 {
		t.Error("Exists must be kept (D-6 ruling: distinguishes missing file from size 0)")
	}
}

// TestBlobRefMatchesAuthoritativeType 同样对齐权威 types.BlobRef。
func TestBlobRefMatchesAuthoritativeType(t *testing.T) {
	want := map[string]string{
		"Hash":      "hash",
		"Size":      "size",
		"MediaType": "media_type",
		"Mode":      "mode",
	}
	typ := reflect.TypeOf(BlobRef{})
	if typ.NumField() != len(want) {
		t.Fatalf("BlobRef has %d fields, want %d", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected field %q", f.Name)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("field %s json tag = %q, want %q", f.Name, got, tag)
		}
	}
	// HashHex/IsZero 与权威 API 一致（04/05 消费）。
	var ref BlobRef
	if !ref.IsZero() {
		t.Error("zero BlobRef must report IsZero")
	}
	if ref.HashHex() == "" {
		t.Error("HashHex must render 64 hex chars")
	}
}

// TestFingerprintUsesMilliseconds 锁死 D-6 的时间单位。
func TestFingerprintUsesMilliseconds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fp, err := Fingerprint(p)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	now := time.Now().UnixMilli()
	if fp.ModTime <= 0 {
		t.Fatal("ModTime must be populated")
	}
	// 毫秒量级（与 now 同数量级）；纳秒会大 6 个数量级。
	if fp.ModTime > now+60_000 || fp.ModTime < now-60_000 {
		t.Errorf("ModTime = %d, expected unix milliseconds near %d", fp.ModTime, now)
	}
	// manifest JSON 序列化用 mod_time
	m := &Manifest{ID: "m1", RunID: "r1", Files: []ManifestFile{{Path: p, Fingerprint: fp}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	files := generic["files"].([]any)
	entry := files[0].(map[string]any)
	fpJSON := entry["fingerprint"].(map[string]any)
	for _, key := range []string{"path", "size", "mod_time", "sha256", "exists"} {
		if _, ok := fpJSON[key]; !ok {
			t.Errorf("fingerprint JSON missing key %q", key)
		}
	}
}
