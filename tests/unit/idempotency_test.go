package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
)

type IdempotencyClass string

const (
	ClassIdempotent    IdempotencyClass = "IDEMPOTENT"
	ClassDetectable    IdempotencyClass = "DETECTABLE"
	ClassNonIdempotent IdempotencyClass = "NON_IDEMPOTENT"
)

type IdempotencyRecord struct {
	Key       string
	Status    string // PENDING, COMMITTED, FAILED
	Result    []byte
	CreatedAt int64
}

type IdempotencyManager struct {
	mu      sync.Mutex
	records map[string]*IdempotencyRecord
}

func NewIdempotencyManager() *IdempotencyManager {
	return &IdempotencyManager{records: make(map[string]*IdempotencyRecord)}
}

func ComputeIdempotencyKey(runID, toolCallID string) string {
	h := sha256.Sum256([]byte(runID + ":" + toolCallID))
	return hex.EncodeToString(h[:])
}

func ClassifyTool(toolName string) IdempotencyClass {
	switch toolName {
	case "file_read", "web_fetch", "knowledge_search":
		return ClassIdempotent
	case "git_status", "file_write_with_hash":
		return ClassDetectable
	case "send_message", "delete_file", "execute_command", "mcp_mutation":
		return ClassNonIdempotent
	default:
		return ClassNonIdempotent
	}
}

func (m *IdempotencyManager) AcquireOrGet(key string) (*IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rec, exists := m.records[key]; exists {
		if rec.Status == "COMMITTED" {
			// 直接返回已落盘结果，避免重复执行副作用
			return rec, nil
		}
		return rec, errors.New("concurrent execution or pending")
	}

	rec := &IdempotencyRecord{
		Key:    key,
		Status: "PENDING",
	}
	m.records[key] = rec
	return rec, nil
}

func (m *IdempotencyManager) Commit(key string, result []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, exists := m.records[key]; exists {
		rec.Status = "COMMITTED"
		rec.Result = result
	}
}

func TestIdempotencyClassificationAndDeduplication(t *testing.T) {
	// 1. 分类测试
	if ClassifyTool("file_read") != ClassIdempotent {
		t.Fatalf("expected file_read to be idempotent")
	}
	if ClassifyTool("git_status") != ClassDetectable {
		t.Fatalf("expected git_status to be detectable")
	}
	if ClassifyTool("send_message") != ClassNonIdempotent {
		t.Fatalf("expected send_message to be non-idempotent")
	}

	// 2. 幂等防护与结果缓存测试（I5不变量：同一key不产生两个durable side effects）
	im := NewIdempotencyManager()
	key := ComputeIdempotencyKey("run-001", "call-001")

	rec, err := im.AcquireOrGet(key)
	if err != nil || rec.Status != "PENDING" {
		t.Fatalf("expected acquire pending record")
	}

	im.Commit(key, []byte("ok-result"))

	// 再次调用 AcquireOrGet 时应直接命中缓存 COMMITTED
	rec2, err := im.AcquireOrGet(key)
	if err != nil {
		t.Fatalf("unexpected error getting committed record: %v", err)
	}
	if string(rec2.Result) != "ok-result" {
		t.Fatalf("expected cached result 'ok-result', got '%s'", string(rec2.Result))
	}
}
