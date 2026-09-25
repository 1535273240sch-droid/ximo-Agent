package unit

import (
	"strings"
	"testing"
)

type Effect string

const (
	EffectAllow Effect = "ALLOW"
	EffectAsk   Effect = "ASK"
	EffectDeny  Effect = "DENY"
)

type RiskLevel string

const (
	RiskLow      RiskLevel = "LOW"
	RiskMedium   RiskLevel = "MEDIUM"
	RiskHigh     RiskLevel = "HIGH"
	RiskCritical RiskLevel = "CRITICAL"
)

type PermissionRequest struct {
	Tool      string
	Action    string
	Path      string
	Host      string
	Port      int
	Command   string
	RiskLevel RiskLevel
}

type PermissionRule struct {
	ID        string
	Effect    Effect
	Tool      string
	PathMatch string
	CmdPrefix string
	RiskLimit RiskLevel
}

type PermissionEngine struct {
	rules []PermissionRule
}

func (pe *PermissionEngine) Evaluate(req PermissionRequest) (Effect, string) {
	// 优先级: DENY > ASK > ALLOW > default (ASK for high risk, ALLOW for low)
	var matchedEffects []struct {
		ruleID string
		effect Effect
	}

	for _, r := range pe.rules {
		if r.Tool != "" && r.Tool != "*" && r.Tool != req.Tool {
			continue
		}
		if r.PathMatch != "" && !strings.HasPrefix(req.Path, r.PathMatch) {
			continue
		}
		if r.CmdPrefix != "" && !strings.HasPrefix(req.Command, r.CmdPrefix) {
			continue
		}
		matchedEffects = append(matchedEffects, struct {
			ruleID string
			effect Effect
		}{ruleID: r.ID, effect: r.Effect})
	}

	// 1. 优先检查是否有任何 DENY
	for _, m := range matchedEffects {
		if m.effect == EffectDeny {
			return EffectDeny, m.ruleID
		}
	}
	// 2. 其次检查是否有 ASK
	for _, m := range matchedEffects {
		if m.effect == EffectAsk {
			return EffectAsk, m.ruleID
		}
	}
	// 3. 再次检查是否有 ALLOW
	for _, m := range matchedEffects {
		if m.effect == EffectAllow {
			return EffectAllow, m.ruleID
		}
	}

	// 4. Default 策略兜底
	if req.RiskLevel == RiskHigh || req.RiskLevel == RiskCritical {
		return EffectAsk, "default_high_risk_ask"
	}
	return EffectAllow, "default_allow"
}

func TestPermissionPrecedence(t *testing.T) {
	engine := &PermissionEngine{
		rules: []PermissionRule{
			{ID: "allow-all-file", Effect: EffectAllow, Tool: "file_read"},
			{ID: "deny-etc-shadow", Effect: EffectDeny, Tool: "file_read", PathMatch: "/etc/shadow"},
			{ID: "ask-config", Effect: EffectAsk, Tool: "file_read", PathMatch: "/etc/"},
		},
	}

	// 1. 命中 DENY：即使有 allow-all-file，/etc/shadow 必须被 DENY
	eff, ruleID := engine.Evaluate(PermissionRequest{
		Tool: "file_read",
		Path: "/etc/shadow",
	})
	if eff != EffectDeny || ruleID != "deny-etc-shadow" {
		t.Fatalf("expected DENY from deny-etc-shadow, got %s by %s", eff, ruleID)
	}

	// 2. 命中 ASK：/etc/hosts 命中 allow 和 ask，ask 优先于 allow
	eff, ruleID = engine.Evaluate(PermissionRequest{
		Tool: "file_read",
		Path: "/etc/hosts",
	})
	if eff != EffectAsk || ruleID != "ask-config" {
		t.Fatalf("expected ASK from ask-config, got %s by %s", eff, ruleID)
	}

	// 3. 命中 ALLOW：/var/log 只命中 allow
	eff, ruleID = engine.Evaluate(PermissionRequest{
		Tool: "file_read",
		Path: "/var/log/app.log",
	})
	if eff != EffectAllow || ruleID != "allow-all-file" {
		t.Fatalf("expected ALLOW from allow-all-file, got %s by %s", eff, ruleID)
	}
}
