// prefixshape.go —— 字节稳定前缀诊断（架构文档第 22 章；v1 prefix-shape.ts 的完整保留）。
//
// 这是 v1 最有价值的性能设计之一，必须在 Go 重写时完整保留：
//
//	每轮 API 请求前捕获前缀形状快照（system + tools + rewrite version），
//	轮间对比即可解释 prompt cache miss 的原因。
//
// 设计要点：
//   - tools 先按 name→description→parameters 字典序归一化再哈希。
//     工具列表顺序抖动会改变 tools JSON 字节，破坏 prompt cache 前缀。
//   - 压缩版本号参与哈希：压缩算法变更后前缀必然变化，诊断要能指出是版本原因。
package ctxmgr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ToolSchema 参与前缀哈希的工具定义。
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// PrefixShape 前缀形状快照。字段名与 v1 PrefixShape 对齐，便于前端复用。
type PrefixShape struct {
	SystemHash         string `json:"system_hash"`
	ToolsHash          string `json:"tools_hash"`
	PrefixHash         string `json:"prefix_hash"`
	LogRewriteVersion  int    `json:"log_rewrite_version"`
	ToolSchemaTokens   int    `json:"tool_schema_tokens"`
	CompressionVersion string `json:"compression_version"`
}

// CacheDiagnostics 缓存诊断，随 Usage 事件上报。
type CacheDiagnostics struct {
	PrefixHash          string   `json:"prefix_hash"`
	PrefixChanged       bool     `json:"prefix_changed"`
	PrefixChangeReasons []string `json:"prefix_change_reasons"`
	SystemHash          string   `json:"system_hash"`
	ToolsHash           string   `json:"tools_hash"`
	LogRewriteVersion   int      `json:"log_rewrite_version"`
	ToolSchemaTokens    int      `json:"tool_schema_tokens"`
	CacheMissTokens     int      `json:"cache_miss_tokens"`
	CacheHitTokens      int      `json:"cache_hit_tokens"`
	CompressionVersion  string   `json:"compression_version"`
}

// NormalizeToolSchemas 按 Name → Description → Parameters 字典序排序工具列表。
//
// 这是保证 tools JSON 字节稳定的关键步骤 —— 上游注册表顺序变化不应影响前缀。
func NormalizeToolSchemas(tools []ToolSchema) []ToolSchema {
	out := make([]ToolSchema, len(tools))
	copy(out, tools)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Description != out[j].Description {
			return out[i].Description < out[j].Description
		}
		pi := stableJSON(out[i].Parameters)
		pj := stableJSON(out[j].Parameters)
		return pi < pj
	})
	return out
}

// stableJSON 序列化 map 并保证 key 有序。
//
// Go 的 encoding/json 对 map[string]any 已经按 key 排序输出，
// 这里额外保证 nil 与空 map 产生一致结果。
func stableJSON(v any) string {
	if v == nil {
		return "null"
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}

// shortHash 取 sha256 前 8 位十六进制（v1 同款）。
func shortHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:8]
}

// estimateTokens 粗估 token 数（~4 chars/token）—— 诊断用途足够。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s) / 4
}

// CaptureShape 捕获当前前缀形状快照。
//
// rewriteVersion 由 ContextManager 递增（每次 snip/compact/prune）。
// compressionVersion 让「压缩算法变更」也能被诊断解释。
func CaptureShape(systemPrompt string, tools []ToolSchema, rewriteVersion int, compressionVersion string) PrefixShape {
	normalized := NormalizeToolSchemas(tools)
	toolsJSON := stableJSON(map[string]any{"tools": normalized})

	return PrefixShape{
		SystemHash:         shortHash(systemPrompt),
		ToolsHash:          shortHash(toolsJSON),
		PrefixHash:         shortHash(systemPrompt + toolsJSON + compressionVersion),
		LogRewriteVersion:  rewriteVersion,
		ToolSchemaTokens:   estimateTokens(toolsJSON),
		CompressionVersion: compressionVersion,
	}
}

// CompareShape 对比两轮前缀形状，生成 cache miss 归因。
//
// 归因取值：system / tools / log_rewrite / compression_version。
func CompareShape(prev, cur PrefixShape, cacheHitTokens, cacheMissTokens int) CacheDiagnostics {
	reasons := make([]string, 0, 4)
	if prev.SystemHash != "" && prev.SystemHash != cur.SystemHash {
		reasons = append(reasons, "system")
	}
	if prev.ToolsHash != "" && prev.ToolsHash != cur.ToolsHash {
		reasons = append(reasons, "tools")
	}
	if prev.LogRewriteVersion != cur.LogRewriteVersion {
		reasons = append(reasons, "log_rewrite")
	}
	if prev.CompressionVersion != "" && prev.CompressionVersion != cur.CompressionVersion {
		reasons = append(reasons, "compression_version")
	}

	return CacheDiagnostics{
		PrefixHash:          cur.PrefixHash,
		PrefixChanged:       len(reasons) > 0,
		PrefixChangeReasons: reasons,
		SystemHash:          cur.SystemHash,
		ToolsHash:           cur.ToolsHash,
		LogRewriteVersion:   cur.LogRewriteVersion,
		ToolSchemaTokens:    cur.ToolSchemaTokens,
		CacheMissTokens:     cacheMissTokens,
		CacheHitTokens:      cacheHitTokens,
		CompressionVersion:  cur.CompressionVersion,
	}
}

// ShapeExplain 生成人类可读的前缀变化说明（供日志与前端状态行）。
func ShapeExplain(d CacheDiagnostics) string {
	if !d.PrefixChanged {
		return "前缀未变化，prompt cache 应命中"
	}
	return "前缀变化原因: " + strings.Join(d.PrefixChangeReasons, ", ")
}
