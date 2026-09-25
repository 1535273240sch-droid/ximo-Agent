package unit

import (
	"fmt"
	"testing"
)

type CompressionLevel string

const (
	LevelNone    CompressionLevel = "none"
	LevelSoft    CompressionLevel = "soft"
	LevelSnip    CompressionLevel = "snip"
	LevelCompact CompressionLevel = "compact"
	LevelForce   CompressionLevel = "force"
)

type CompactedContextResult struct {
	Level               CompressionLevel
	ContextVersion      string
	CompressionVersion  string
	TokenizerVersion    string
	PromptSchemaVersion string
	FinalTokens         int
}

type ContextCompressor struct {
	ContextWindow int
}

func (cc *ContextCompressor) Compress(currentTokens int) CompactedContextResult {
	ratio := float64(currentTokens) / float64(cc.ContextWindow)
	level := LevelNone
	targetTokens := currentTokens

	switch {
	case ratio >= 0.90:
		level = LevelForce
		targetTokens = int(float64(cc.ContextWindow) * 0.40)
	case ratio >= 0.80:
		level = LevelCompact
		targetTokens = int(float64(cc.ContextWindow) * 0.60)
	case ratio >= 0.60:
		level = LevelSnip
		targetTokens = int(float64(cc.ContextWindow) * 0.70)
	case ratio >= 0.50:
		level = LevelSoft
		targetTokens = int(float64(cc.ContextWindow) * 0.85)
	}

	return CompactedContextResult{
		Level:               level,
		ContextVersion:      "v2.1",
		CompressionVersion:  "1.0.0",
		TokenizerVersion:    "bpe-v1",
		PromptSchemaVersion: "2026-q3",
		FinalTokens:         targetTokens,
	}
}

func TestContextCompressionTiers(t *testing.T) {
	compressor := &ContextCompressor{ContextWindow: 100000}

	// 1. 低于 50%：不压缩
	r1 := compressor.Compress(40000)
	if r1.Level != LevelNone || r1.FinalTokens != 40000 {
		t.Fatalf("expected none, got %s", r1.Level)
	}

	// 2. 55%：Soft
	r2 := compressor.Compress(55000)
	if r2.Level != LevelSoft {
		t.Fatalf("expected soft, got %s", r2.Level)
	}

	// 3. 65%：Snip
	r3 := compressor.Compress(65000)
	if r3.Level != LevelSnip {
		t.Fatalf("expected snip, got %s", r3.Level)
	}

	// 4. 85%：Compact
	r4 := compressor.Compress(85000)
	if r4.Level != LevelCompact {
		t.Fatalf("expected compact, got %s", r4.Level)
	}

	// 5. 95%：Force
	r5 := compressor.Compress(95000)
	if r5.Level != LevelForce {
		t.Fatalf("expected force, got %s", r5.Level)
	}

	// 校验4个版本标识是否完全包含
	for idx, r := range []CompactedContextResult{r2, r3, r4, r5} {
		if r.ContextVersion == "" || r.CompressionVersion == "" ||
			r.TokenizerVersion == "" || r.PromptSchemaVersion == "" {
			t.Fatalf("tier %d missing required version metadata: %+v", idx, r)
		}
	}
	fmt.Printf("[TestContextCompressionTiers] All tiers validated with versions.\n")
}
