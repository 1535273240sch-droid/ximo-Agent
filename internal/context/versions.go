// versions.go —— 压缩结果版本化（架构文档第 22.2 章）。
//
// 硬性要求：每次压缩结果必须带四个版本号，这样未来改算法不会让历史 Run 无法解释复现。
//
//	ContextVersion        上下文布局版本（预算分段方案变更时递增）
//	CompressionVersion    压缩算法版本（snip/prune/compact 策略变更时递增）
//	TokenizerVersion      分词器版本（词表/预分词正则变更时递增）
//	PromptSchemaVersion   提示词结构版本（system/tool 包装格式变更时递增）
//
// 版本号参与 PrefixShape 哈希，因此「版本变了」能被缓存诊断解释为 prompt cache miss 原因。
package ctxmgr

import (
	"fmt"
	"strings"
)

// 四个版本常量。改动对应实现时必须同步递增，并在 NOTICE/变更记录里说明原因。
const (
	// ContextVersion 上下文布局版本。v2 采用第 22.1 章的七段预算模型。
	ContextVersion = "ctx-v2.0"

	// CompressionVersion 压缩算法版本。v2 为四级 tier（soft/snip/compact/force）+ stuck 保护。
	CompressionVersion = "compress-v2.0"

	// TokenizerVersion 分词器版本。对应 DeepSeek V4 tokenizer.json 的词表快照。
	TokenizerVersion = "deepseek-v4-bpe-1.0"

	// PromptSchemaVersion 提示词结构版本。
	PromptSchemaVersion = "prompt-schema-v2.0"
)

// Versions 一次压缩结果携带的完整版本集合。
type Versions struct {
	ContextVersion      string `json:"context_version"`
	CompressionVersion  string `json:"compression_version"`
	TokenizerVersion    string `json:"tokenizer_version"`
	PromptSchemaVersion string `json:"prompt_schema_version"`
}

// CurrentVersions 返回当前生效的版本集合。
func CurrentVersions() Versions {
	return Versions{
		ContextVersion:      ContextVersion,
		CompressionVersion:  CompressionVersion,
		TokenizerVersion:    TokenizerVersion,
		PromptSchemaVersion: PromptSchemaVersion,
	}
}

// String 返回可用于日志/哈希的稳定表示。
func (v Versions) String() string {
	return strings.Join([]string{
		v.ContextVersion,
		v.CompressionVersion,
		v.TokenizerVersion,
		v.PromptSchemaVersion,
	}, "|")
}

// Validate 校验四个版本号都不为空 —— 压缩产物落库前的守门检查。
func (v Versions) Validate() error {
	missing := make([]string, 0, 4)
	if v.ContextVersion == "" {
		missing = append(missing, "context_version")
	}
	if v.CompressionVersion == "" {
		missing = append(missing, "compression_version")
	}
	if v.TokenizerVersion == "" {
		missing = append(missing, "tokenizer_version")
	}
	if v.PromptSchemaVersion == "" {
		missing = append(missing, "prompt_schema_version")
	}
	if len(missing) > 0 {
		return fmt.Errorf("ctxmgr: 压缩结果缺少版本号: %s", strings.Join(missing, ", "))
	}
	return nil
}
