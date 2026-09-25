package unit

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

type BlobRef struct {
	Hash      [32]byte
	Size      int64
	MediaType string
	Mode      uint32
}

type FileFingerprint struct {
	Path   string
	Size   int64
	MTime  int64
	SHA256 [32]byte
}

type CASStorage struct {
	baseDir string
}

func NewCASStorage(baseDir string) *CASStorage {
	_ = os.MkdirAll(filepath.Join(baseDir, "blobs"), 0755)
	return &CASStorage{baseDir: baseDir}
}

// Put 原子写入：temp file -> sync -> rename
func (c *CASStorage) Put(data []byte, mediaType string) (BlobRef, error) {
	h := sha256.Sum256(data)
	hexHash := fmtHex(h[:])

	blobDir := filepath.Join(c.baseDir, "blobs")
	finalPath := filepath.Join(blobDir, hexHash)

	if _, err := os.Stat(finalPath); err == nil {
		// 已存在
		return BlobRef{Hash: h, Size: int64(len(data)), MediaType: mediaType, Mode: 0644}, nil
	}

	tmpFile, err := os.CreateTemp(blobDir, "blob-*")
	if err != nil {
		return BlobRef{}, err
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	if _, err := tmpFile.Write(data); err != nil {
		return BlobRef{}, err
	}
	if err := tmpFile.Sync(); err != nil {
		return BlobRef{}, err
	}
	_ = tmpFile.Close()

	if err := os.Rename(tmpFile.Name(), finalPath); err != nil {
		return BlobRef{}, err
	}

	return BlobRef{
		Hash:      h,
		Size:      int64(len(data)),
		MediaType: mediaType,
		Mode:      0644,
	}, nil
}

func (c *CASStorage) CheckConflict(current Fingerprinter, path string, expected FileFingerprint) (bool, error) {
	currFp, err := current.GetFingerprint(path)
	if err != nil {
		return false, err
	}

	if currFp.Size != expected.Size || currFp.SHA256 != expected.SHA256 {
		return true, nil // 发生冲突
	}
	return false, nil
}

type Fingerprinter interface {
	GetFingerprint(path string) (FileFingerprint, error)
}

type mockFingerprinter struct {
	fp FileFingerprint
}

func (m *mockFingerprinter) GetFingerprint(path string) (FileFingerprint, error) {
	return m.fp, nil
}

func fmtHex(b []byte) string {
	var res string
	for _, x := range b {
		res += string("0123456789abcdef"[x>>4]) + string("0123456789abcdef"[x&0x0f])
	}
	return res
}

func TestCASAtomicWriteAndConflictDetection(t *testing.T) {
	tmpDir := t.TempDir()
	cas := NewCASStorage(tmpDir)

	data := []byte("hello ximo-agent durable storage")
	ref, err := cas.Put(data, "text/plain")
	if err != nil {
		t.Fatalf("failed to put data into CAS: %v", err)
	}

	expectedHash := sha256.Sum256(data)
	if !bytes.Equal(ref.Hash[:], expectedHash[:]) {
		t.Fatalf("hash mismatch in BlobRef")
	}

	// 冲突检查：预期的 Hash 与当前实际 Hash 不一致（模拟并发修改）
	fpExpected := FileFingerprint{
		Path:   "test.txt",
		Size:   int64(len(data)),
		SHA256: expectedHash,
	}

	differentHash := sha256.Sum256([]byte("modified by someone else"))
	fpActual := FileFingerprint{
		Path:   "test.txt",
		Size:   int64(len(data)),
		SHA256: differentHash,
	}

	conflict, err := cas.CheckConflict(&mockFingerprinter{fp: fpActual}, "test.txt", fpExpected)
	if err != nil {
		t.Fatalf("unexpected error checking conflict: %v", err)
	}
	if !conflict {
		t.Fatalf("expected conflict detection when file hashes mismatch")
	}
}
