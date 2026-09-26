package tool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Effect 权限决策效果。优先级固定为 deny > ask > allow > default（与 v1 一致）。
type Effect int

const (
	// EffectAllow 直接执行，无需确认。
	EffectAllow Effect = iota
	// EffectAsk 需要用户确认（由 ConfirmationHandler 解析）。
	EffectAsk
	// EffectDeny 拒绝执行。
	EffectDeny
)

func (e Effect) String() string {
	switch e {
	case EffectAllow:
		return "allow"
	case EffectAsk:
		return "ask"
	case EffectDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// Decision 权限引擎 v2 的决策结果。Reason 面向 UI 可解释：
// “为什么被拒绝 / 是哪条规则 / 为什么需要确认”都从 Reason + RuleID 得出。
type Decision struct {
	Effect Effect
	Reason string
	RuleID string
	Risk   RiskLevel
}

// Allowed 报告决策是否放行（Allow，或 Ask 且用户已确认）。
func (d Decision) Allowed() bool { return d.Effect == EffectAllow }

// ---------------------------------------------------------------------------
// 规则
// ---------------------------------------------------------------------------

// Rule 权限规则。v1 的 PermissionRule 只有 tool + subject；v2 把匹配维度扩展到
// tool/action/path/host/port/command/mode/risk level/user confirmation。
// 所有维度为空表示“任意值”；glob 模式支持 *（任意串）与 ?（单字符），
// 匹配大小写不敏感（与 v1 matchGlob 的 /i 行为保持一致）。
type Rule struct {
	// ID 规则唯一标识，进入 Decision.RuleID 供 UI/审计引用。
	ID string
	// Tool 工具名 glob。
	Tool string
	// Action 工具内的动作 glob（如 git action、knowledge action）。
	Action string
	// Path 文件路径 glob（file_* 系列）。
	Path string
	// Host 主机名 glob（web_fetch / http 能力）。
	Host string
	// Port 端口规格："443"、"8000-9000"、"80,443"、"*"/"" = 任意。
	Port string
	// Command 命令文本 glob（terminal_exec）。
	Command string
	// Mode 适用模式；空 = 所有模式。
	Mode Mode
	// MinRisk 规则仅对有效风险 >= MinRisk 的请求生效；0 = 不限。
	MinRisk RiskLevel
	// RequireConfirmation 命中后即使 Effect=Allow 也升级为 Ask（用户确认维度）。
	RequireConfirmation bool
	// BypassRiskFloor 允许该规则突破全局风险地板（显式放行的高危操作，
	// 例如用户在 UI 中固定允许的 git push）。
	BypassRiskFloor bool
	// Effect 规则效果。
	Effect Effect
	// Description 人类可读描述，进入 Reason。
	Description string
}

// specificity 计算规则的具体度（用于同效果多规则命中时选择最具体的一条）。
func (r Rule) specificity() int {
	score := 0
	for _, dim := range []string{r.Tool, r.Action, r.Path, r.Host, r.Command} {
		if dim != "" && dim != "*" {
			score += 2
		}
	}
	if r.Port != "" && r.Port != "*" {
		score++
	}
	if r.Mode != "" {
		score++
	}
	if r.MinRisk != RiskUnknown {
		score++
	}
	return score
}

// matches 报告规则是否匹配请求。
func (r Rule) matches(req PermissionRequest, effectiveRisk RiskLevel) bool {
	if r.Mode != "" && req.Mode != "" && !strings.EqualFold(string(r.Mode), string(req.Mode)) {
		return false
	}
	if r.MinRisk != RiskUnknown && effectiveRisk < r.MinRisk {
		return false
	}
	if r.Tool != "" && !globMatch(r.Tool, req.Tool) {
		return false
	}
	if r.Action != "" && !globMatch(r.Action, req.Action) {
		return false
	}
	if r.Path != "" && !globMatch(r.Path, req.Path) {
		return false
	}
	if r.Host != "" && !globMatch(r.Host, req.Host) {
		return false
	}
	if r.Command != "" && !globMatch(r.Command, req.Command) {
		return false
	}
	if !matchPort(r.Port, req.Port) {
		return false
	}
	return true
}

// globMatch 简化 glob：* = 任意序列，? = 单字符，大小写不敏感（对齐 v1）。
func globMatch(pattern, text string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if pattern == text {
		return true
	}
	return globMatchSlow(strings.ToLower(pattern), strings.ToLower(text))
}

func globMatchSlow(pattern, text string) bool {
	// 迭代式通配匹配，避免正则编译开销与灾难性回溯。
	p, t := []rune(pattern), []rune(text)
	var (
		pi, ti int
		star   = -1
		starTi int
	)
	for ti < len(t) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]) {
			pi++
			ti++
			continue
		}
		if pi < len(p) && p[pi] == '*' {
			star = pi
			starTi = ti
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			starTi++
			ti = starTi
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// matchPort 端口规格匹配。空/"*" = 任意。
func matchPort(spec string, port int) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "*" {
		return true
	}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if dash := strings.IndexByte(item, '-'); dash > 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(item[:dash]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(item[dash+1:]))
			if err1 == nil && err2 == nil && port >= lo && port <= hi {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(item); err == nil && n == port {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 权限请求 / 引擎
// ---------------------------------------------------------------------------

// PermissionRequest 权限评估请求，覆盖 v2 的全部匹配维度。
type PermissionRequest struct {
	Tool    string
	Action  string
	Path    string
	Host    string
	Port    int
	Command string
	Mode    Mode
	Risk    RiskLevel
	// Confirmed / ApprovedRuleID / Scope 来自 ToolRequest.Confirmation。
	// Scope 决定确认的覆盖范围：single 仅本次调用，session 绑定具体规则。
	Confirmed      bool
	ApprovedRuleID string
	Scope          ConfirmationScope
	SessionID      string
	RunID          string
	ToolCallID     string
	Arguments      map[string]any
}

// PermissionConfig 一套规则配置（对应 v1 的 PermissionConfig）。
type PermissionConfig struct {
	Allow           []Rule
	Ask             []Rule
	Deny            []Rule
	DefaultDecision Effect
}

// PermissionEngine 权限引擎 v2。
type PermissionEngine interface {
	// Evaluate 评估请求并返回决策。实现必须并发安全。
	Evaluate(ctx context.Context, req PermissionRequest) Decision
	// Explain 返回完整解释（命中规则链、是否触发风险地板、默认策略）。
	Explain(ctx context.Context, req PermissionRequest) Explanation
	// AddUserRule 动态添加用户规则（立即生效并持久化）。
	AddUserRule(ctx context.Context, rule Rule) error
	// RemoveUserRule 移除用户规则。
	RemoveUserRule(ctx context.Context, ruleID string) error
}

// Explanation 面向 UI/审计的完整解释。
type Explanation struct {
	Decision         Decision
	MatchedRules     []MatchedRule
	DefaultUsed      bool
	RiskFloorApplied bool
	EffectiveRisk    RiskLevel
}

// MatchedRule 一条命中规则的说明。
type MatchedRule struct {
	RuleID      string
	Effect      Effect
	Specificity int
	Description string
}

// RiskFloorAsk 全局风险地板：有效风险 >= 该值的请求至少需要 Ask，
// 除非命中显式 BypassRiskFloor 的 allow 规则。这是 v2 相对 v1 的硬化
// （v1 YOLO 模式会放行一切）。
var RiskFloorAsk = RiskCritical

// engine 是 PermissionEngine 的实现。
type engine struct {
	mu sync.RWMutex

	// configs 按模式存放静态配置。
	configs map[Mode]PermissionConfig
	// userRules 用户动态规则（UI 中添加的 allow/deny），优先于静态规则。
	userRules []Rule
	// store 可选的用户规则持久化（任务03 提供后接入）。
	store RuleStore

	// riskFloor 风险地板；0 值表示使用 RiskFloorAsk。
	riskFloor RiskLevel
	// defaultDecision 未配置模式时的回退决策（fail closed = Ask）。
	defaultDecision Effect
}

// RuleStore 用户动态规则的持久化接口（先用内存 mock 顶上）。
type RuleStore interface {
	List(ctx context.Context) ([]Rule, error)
	Add(ctx context.Context, rule Rule) error
	Remove(ctx context.Context, ruleID string) error
}

// NewPermissionEngine 创建权限引擎。configs 为各模式静态配置；
// store 可为 nil（无持久化用户规则）。
func NewPermissionEngine(configs map[Mode]PermissionConfig, store RuleStore) (PermissionEngine, error) {
	e := &engine{
		configs:         make(map[Mode]PermissionConfig, len(configs)),
		store:           store,
		riskFloor:       RiskFloorAsk,
		defaultDecision: EffectAsk,
	}
	for mode, cfg := range configs {
		if err := validateConfig(cfg); err != nil {
			return nil, fmt.Errorf("tool: 模式 %q 权限配置无效: %w", mode, err)
		}
		e.configs[mode] = cfg
	}
	if store != nil {
		rules, err := store.List(context.Background())
		if err != nil {
			return nil, fmt.Errorf("tool: 加载用户权限规则失败: %w", err)
		}
		e.userRules = rules
	}
	return e, nil
}

func validateConfig(cfg PermissionConfig) error {
	for _, list := range [][]Rule{cfg.Allow, cfg.Ask, cfg.Deny} {
		for _, r := range list {
			if r.Port != "" && r.Port != "*" {
				for _, item := range strings.Split(r.Port, ",") {
					item = strings.TrimSpace(item)
					if item == "" {
						continue
					}
					if dash := strings.IndexByte(item, '-'); dash > 0 {
						if _, err := strconv.Atoi(strings.TrimSpace(item[:dash])); err != nil {
							return fmt.Errorf("规则 %q 端口规格 %q 无效", r.ID, r.Port)
						}
						if _, err := strconv.Atoi(strings.TrimSpace(item[dash+1:])); err != nil {
							return fmt.Errorf("规则 %q 端口规格 %q 无效", r.ID, r.Port)
						}
						continue
					}
					if _, err := strconv.Atoi(item); err != nil {
						return fmt.Errorf("规则 %q 端口规格 %q 无效", r.ID, r.Port)
					}
				}
			}
		}
	}
	return nil
}

// AddUserRule 动态添加用户规则（立即生效并持久化）。
func (e *engine) AddUserRule(ctx context.Context, rule Rule) error {
	if rule.ID == "" {
		return fmt.Errorf("tool: 用户规则缺少 ID")
	}
	e.mu.Lock()
	for _, existing := range e.userRules {
		if existing.ID == rule.ID {
			e.mu.Unlock()
			return fmt.Errorf("tool: 用户规则 %q 已存在", rule.ID)
		}
	}
	e.userRules = append(e.userRules, rule)
	store := e.store
	e.mu.Unlock()

	if store != nil {
		if err := store.Add(ctx, rule); err != nil {
			// 持久化失败时回滚内存态，保证内存与存储一致。
			e.mu.Lock()
			for i, existing := range e.userRules {
				if existing.ID == rule.ID {
					e.userRules = append(e.userRules[:i], e.userRules[i+1:]...)
					break
				}
			}
			e.mu.Unlock()
			return fmt.Errorf("tool: 持久化用户规则失败: %w", err)
		}
	}
	return nil
}

// RemoveUserRule 移除用户规则。
func (e *engine) RemoveUserRule(ctx context.Context, ruleID string) error {
	e.mu.Lock()
	idx := -1
	for i, existing := range e.userRules {
		if existing.ID == ruleID {
			idx = i
			break
		}
	}
	if idx < 0 {
		e.mu.Unlock()
		return fmt.Errorf("tool: 用户规则 %q 不存在", ruleID)
	}
	e.userRules = append(e.userRules[:idx], e.userRules[idx+1:]...)
	store := e.store
	e.mu.Unlock()

	if store != nil {
		if err := store.Remove(ctx, ruleID); err != nil {
			return fmt.Errorf("tool: 移除持久化用户规则失败: %w", err)
		}
	}
	return nil
}

// effectiveRisk 计算有效风险：请求风险与工具默认风险取较大者。
func effectiveRisk(req PermissionRequest) RiskLevel {
	risk := req.Risk
	if def := DefaultRisk(req.Tool, req.Action); def > risk {
		risk = def
	}
	return risk
}

// rulesForMode 汇总某模式下参与匹配的规则。
// 求值优先级：deny（静态+用户）> 用户规则 > 静态 ask > 静态 allow > default。
// 用户显式添加的规则代表用户意图，压过静态默认；但 deny 永远最高。
func (e *engine) rulesForMode(mode Mode) (userRules, deny, staticAsk, staticAllow []Rule) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	cfg, ok := e.configs[mode]
	if !ok {
		cfg, ok = e.configs[ModeCoding]
	}
	if ok {
		staticAsk = append(staticAsk, cfg.Ask...)
		staticAllow = append(staticAllow, cfg.Allow...)
		deny = append(deny, cfg.Deny...)
	}
	for _, r := range e.userRules {
		if r.Mode != "" && mode != "" && !strings.EqualFold(string(r.Mode), string(mode)) {
			continue
		}
		switch r.Effect {
		case EffectAllow, EffectAsk:
			userRules = append(userRules, r)
		case EffectDeny:
			deny = append(deny, r)
		}
	}
	return userRules, deny, staticAsk, staticAllow
}

// bestMatch 在规则列表中找最具体的命中规则。
func bestMatch(rules []Rule, req PermissionRequest, risk RiskLevel) (Rule, bool) {
	var (
		best     Rule
		found    bool
		bestSpec int
	)
	for _, r := range rules {
		if !r.matches(req, risk) {
			continue
		}
		spec := r.specificity()
		if !found || spec > bestSpec {
			best, found, bestSpec = r, true, spec
		}
	}
	return best, found
}

// Explain 实现 PermissionEngine。
func (e *engine) Explain(ctx context.Context, req PermissionRequest) Explanation {
	risk := effectiveRisk(req)
	userRules, deny, staticAsk, staticAllow := e.rulesForMode(req.Mode)

	exp := Explanation{EffectiveRisk: risk}
	floor := e.riskFloor
	if floor == RiskUnknown {
		floor = RiskFloorAsk
	}

	// 1. deny 永远最高（静态与用户 deny 合并）。
	if r, ok := bestMatch(deny, req, risk); ok {
		exp.MatchedRules = append(exp.MatchedRules, MatchedRule{RuleID: r.ID, Effect: EffectDeny, Specificity: r.specificity(), Description: r.Description})
		exp.Decision = Decision{
			Effect: EffectDeny,
			Reason: fmt.Sprintf("命中 deny 规则 %q%s：%s 被安全策略禁止", r.ID, describeRule(r), describeSubject(req)),
			RuleID: r.ID,
			Risk:   risk,
		}
		return exp
	}

	// 2. 用户显式规则（allow 或 ask）压过静态默认。
	if r, ok := bestMatch(userRules, req, risk); ok {
		effect := r.Effect
		verb := "已放行"
		if r.Effect == EffectAsk {
			verb = "需要用户确认"
		}
		if r.RequireConfirmation {
			effect = EffectAsk
			verb = "需要用户确认（规则要求确认）"
		}
		// 风险地板：有效风险达到上限时，allow 升级为 Ask，
		// 除非规则显式 BypassRiskFloor（用户在 UI 固定允许的高危操作）。
		if effect == EffectAllow && risk >= floor && !r.BypassRiskFloor {
			effect = EffectAsk
			verb = fmt.Sprintf("风险等级 %s 达到自动放行上限（%s），需要用户确认", risk, floor)
			exp.RiskFloorApplied = true
		}
		reason := fmt.Sprintf("命中用户规则 %q%s：%s %s", r.ID, describeRule(r), describeSubject(req), verb)
		exp.MatchedRules = append(exp.MatchedRules, MatchedRule{RuleID: r.ID, Effect: effect, Specificity: r.specificity(), Description: r.Description})
		exp.Decision = Decision{Effect: effect, Reason: reason, RuleID: r.ID, Risk: risk}
		if effect == EffectAsk && req.Confirmed && confirmationCovers(req, r.ID) {
			exp.Decision.Effect = EffectAllow
			exp.Decision.Reason = fmt.Sprintf("用户已确认（规则 %q，scope=%s）", r.ID, confirmationScopeName(req))
		}
		return exp
	}

	// 3. 静态 ask。
	if r, ok := bestMatch(staticAsk, req, risk); ok {
		exp.MatchedRules = append(exp.MatchedRules, MatchedRule{RuleID: r.ID, Effect: EffectAsk, Specificity: r.specificity(), Description: r.Description})
		exp.Decision = Decision{
			Effect: EffectAsk,
			Reason: fmt.Sprintf("命中 ask 规则 %q%s：%s 需要用户确认", r.ID, describeRule(r), describeSubject(req)),
			RuleID: r.ID,
			Risk:   risk,
		}
		if req.Confirmed && confirmationCovers(req, r.ID) {
			exp.Decision.Effect = EffectAllow
			exp.Decision.Reason = fmt.Sprintf("用户已确认（规则 %q，scope=%s）", r.ID, confirmationScopeName(req))
		}
		return exp
	}

	// 4. 静态 allow。
	if r, ok := bestMatch(staticAllow, req, risk); ok {
		effect := EffectAllow
		reason := fmt.Sprintf("命中 allow 规则 %q%s：%s 已放行", r.ID, describeRule(r), describeSubject(req))
		if r.RequireConfirmation {
			effect = EffectAsk
			reason = fmt.Sprintf("命中 allow 规则 %q%s，但规则要求确认：%s 需要用户确认", r.ID, describeRule(r), describeSubject(req))
		}
		// 风险地板（同用户规则分支）：BypassRiskFloor 是唯一出口。
		if effect == EffectAllow && risk >= floor && !r.BypassRiskFloor {
			effect = EffectAsk
			reason = fmt.Sprintf("命中 allow 规则 %q%s，但风险等级 %s 达到自动放行上限（%s）：%s 需要用户确认",
				r.ID, describeRule(r), risk, floor, describeSubject(req))
			exp.RiskFloorApplied = true
		}
		exp.MatchedRules = append(exp.MatchedRules, MatchedRule{RuleID: r.ID, Effect: effect, Specificity: r.specificity(), Description: r.Description})
		exp.Decision = Decision{Effect: effect, Reason: reason, RuleID: r.ID, Risk: risk}
		if effect == EffectAsk && req.Confirmed && confirmationCovers(req, r.ID) {
			exp.Decision.Effect = EffectAllow
			exp.Decision.Reason = fmt.Sprintf("用户已确认（规则 %q，scope=%s）", r.ID, confirmationScopeName(req))
		}
		return exp
	}

	// 5. 未命中任何规则：该模式的默认决策 + 风险地板（floor 已在函数开头解析）。
	//
	// 这里必须取「当前模式」的 DefaultDecision，而不是引擎级的 e.defaultDecision：
	// 后者是「模式未配置」时的兜底（恒为 Ask/fail closed）。若在此处误用兜底值，
	// 各模式在 DefaultPermissionConfigs 里声明的策略（yolo/safe 为 allow）就永远
	// 不生效——表现是所有未列规则的调用一律被问询，在无 UI 确认方时即被拒绝。
	decision := e.defaultDecisionForMode(req.Mode)
	reason := "未命中任何规则，按默认策略处理"
	if risk >= floor {
		exp.RiskFloorApplied = true
		if decision == EffectAllow {
			decision = EffectAsk
		}
		reason = fmt.Sprintf("未命中任何规则，且有效风险 %s 达到自动放行上限（%s），按默认策略询问", risk, floor)
	}
	exp.DefaultUsed = true
	exp.Decision = Decision{Effect: decision, Reason: reason, RuleID: "", Risk: risk}
	return exp
}

// defaultDecisionForMode 返回该模式的默认决策。
//
// 与 rulesForMode 的回退链保持一致：模式未配置时退回 ModeCoding 的配置，
// 连 ModeCoding 也没有配置时才用引擎级兜底（Ask，fail closed）。
func (e *engine) defaultDecisionForMode(mode Mode) Effect {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if cfg, ok := e.configs[mode]; ok {
		return cfg.DefaultDecision
	}
	if cfg, ok := e.configs[ModeCoding]; ok {
		return cfg.DefaultDecision
	}
	return e.defaultDecision
}

// Evaluate 实现 PermissionEngine。deny > ask > allow > default 的优先级在
// Explain 的顺序检查中体现；用户确认只能把 Ask 提升为 Allow，绝不能覆盖 Deny。
func (e *engine) Evaluate(ctx context.Context, req PermissionRequest) Decision {
	return e.Explain(ctx, req).Decision
}

// confirmationCovers 报告一次用户确认是否覆盖目标规则。
//   - ScopeSingle：确认只针对本次调用（ToolCallID 已由运行时限定），覆盖任意规则；
//   - ScopeSession：确认绑定在具体规则上，只有 ApprovedRuleID 与目标规则
//     一致时才覆盖（防止"允许了 A"被拿来放行 B）。
func confirmationCovers(req PermissionRequest, ruleID string) bool {
	if !req.Confirmed {
		return false
	}
	// ScopeSession：确认绑定在具体规则上，防止"允许了 A"被拿去放行 B。
	if req.Scope == ScopeSession {
		return req.ApprovedRuleID != "" && req.ApprovedRuleID == ruleID
	}
	// ScopeSingle：确认只针对本次调用（ToolCallID 已由运行时限定）。
	return true
}

func confirmationScopeName(req PermissionRequest) string {
	if req.Scope == ScopeSession {
		return "session"
	}
	return "single"
}

func describeRule(r Rule) string {
	var dims []string
	if r.Tool != "" {
		dims = append(dims, "tool="+r.Tool)
	}
	if r.Action != "" {
		dims = append(dims, "action="+r.Action)
	}
	if r.Path != "" {
		dims = append(dims, "path="+r.Path)
	}
	if r.Host != "" {
		dims = append(dims, "host="+r.Host)
	}
	if r.Port != "" && r.Port != "*" {
		dims = append(dims, "port="+r.Port)
	}
	if r.Command != "" {
		dims = append(dims, "command="+r.Command)
	}
	if r.Mode != "" {
		dims = append(dims, "mode="+string(r.Mode))
	}
	if r.MinRisk != RiskUnknown {
		dims = append(dims, "min_risk="+r.MinRisk.String())
	}
	if r.Description != "" {
		dims = append(dims, "desc="+r.Description)
	}
	if len(dims) == 0 {
		return ""
	}
	return "(" + strings.Join(dims, ", ") + ")"
}

func describeSubject(req PermissionRequest) string {
	switch {
	case req.Command != "":
		return "命令 " + strconv.Quote(req.Command)
	case req.Path != "":
		return "路径 " + req.Path
	case req.Host != "":
		return "主机 " + req.Host
	default:
		return "工具 " + req.Tool
	}
}

// ---------------------------------------------------------------------------
// 工具风险等级表（v2 新增维度：risk level）
// ---------------------------------------------------------------------------

// DefaultRiskTable 工具/动作 → 默认风险等级。运行时在请求未携带风险时使用，
// 权限引擎也会用它兜底提升有效风险。key 可以是 "tool" 或 "tool:action"
// （动作级优先，见 DefaultRisk）。
//
// 一致性约束（TestToolPoliciesConsistent 固化）：本表与
// idempotency.DefaultClassPolicy 必须同步演进——C 类非幂等的工具/动作
// 风险不得低于 Medium，A 类可自动重试的风险不得为 Critical。
var DefaultRiskTable = map[string]RiskLevel{
	"file_read":          RiskLow,
	"file_list":          RiskLow,
	"file_search":        RiskLow,
	"web_fetch":          RiskLow,
	"web_search":         RiskLow,
	"knowledge":          RiskLow,
	"git_log":            RiskLow,
	"git_diff":           RiskLow,
	"git_branch":         RiskLow,
	"git_status":         RiskLow,
	"todo_write":         RiskLow,
	"file_write":         RiskMedium,
	"file_edit":          RiskMedium,
	"multi_edit":         RiskMedium,
	"move_file":          RiskMedium,
	"create_tool":        RiskMedium,
	"file_delete":        RiskHigh,
	"terminal_exec":      RiskHigh,
	"git_operations":     RiskHigh,
	"send_message":       RiskHigh,
	"office_docs":        RiskHigh,
	"browser_execute_js": RiskHigh,
	"act_ui":             RiskCritical,
	"computer_use":       RiskCritical,
	"network_replay":     RiskCritical,

	// 动作级细化：同一工具的读写动作风险不同（S-3 一致性修正）
	"knowledge:add":         RiskMedium,
	"knowledge:update":      RiskMedium,
	"knowledge:delete":      RiskHigh,
	"git_operations:push":   RiskHigh,
	"git_operations:commit": RiskMedium,
}

// DefaultRisk 返回工具默认风险等级（含动作级细化）。
func DefaultRisk(toolName, action string) RiskLevel {
	if action != "" {
		if risk, ok := DefaultRiskTable[toolName+":"+action]; ok {
			return risk
		}
	}
	if risk, ok := DefaultRiskTable[toolName]; ok {
		return risk
	}
	return RiskMedium
}

// ---------------------------------------------------------------------------
// 默认配置（从 v1 Permission.ts 平移并扩展维度）
// ---------------------------------------------------------------------------

// DefaultPermissionConfigs 返回 v1 三套模式配置的 v2 版本。
// YOLO/Safe/Off 三档通过 autoModeLevel 叠加在 Mode 上（由调用方选择 config）。
func DefaultPermissionConfigs() map[Mode]PermissionConfig {
	configs := map[Mode]PermissionConfig{
		ModeYolo: {DefaultDecision: EffectAllow},
		ModeSafe: {
			Ask:             []Rule{{ID: "safe:file_delete", Tool: "file_delete", Effect: EffectAsk, Description: "删除文件需确认"}},
			Deny:            []Rule{{ID: "safe:act_ui", Tool: "act_ui", Effect: EffectDeny}, {ID: "safe:network_replay", Tool: "network_replay", Effect: EffectDeny}, {ID: "safe:browser_execute_js", Tool: "browser_execute_js", Effect: EffectDeny}},
			DefaultDecision: EffectAllow,
		},
		ModeCoding: {
			Allow: []Rule{
				{ID: "coding:file_read", Tool: "file_read", Effect: EffectAllow},
				{ID: "coding:file_list", Tool: "file_list", Effect: EffectAllow},
				{ID: "coding:file_search", Tool: "file_search", Effect: EffectAllow},
				{ID: "coding:web_search", Tool: "web_search", Effect: EffectAllow},
				{ID: "coding:web_fetch", Tool: "web_fetch", Effect: EffectAllow},
				{ID: "coding:knowledge", Tool: "knowledge", Action: "search", Effect: EffectAllow},
				{ID: "coding:knowledge:read", Tool: "knowledge", Action: "list", Effect: EffectAllow},
				{ID: "coding:git_read", Tool: "git_log", Effect: EffectAllow},
				{ID: "coding:git_diff", Tool: "git_diff", Effect: EffectAllow},
				{ID: "coding:git_branch", Tool: "git_branch", Effect: EffectAllow},
				{ID: "coding:todo_write", Tool: "todo_write", Effect: EffectAllow},
				{ID: "coding:create_tool", Tool: "create_tool", Effect: EffectAllow},
				{ID: "coding:file_write", Tool: "file_write", Effect: EffectAllow},
				{ID: "coding:file_edit", Tool: "file_edit", Effect: EffectAllow},
				{ID: "coding:multi_edit", Tool: "multi_edit", Effect: EffectAllow},
				{ID: "coding:move_file", Tool: "move_file", Effect: EffectAllow},
			},
			Ask: []Rule{
				{ID: "coding:terminal_exec", Tool: "terminal_exec", Effect: EffectAsk},
				{ID: "coding:git_operations", Tool: "git_operations", Effect: EffectAsk},
				{ID: "coding:git_push", Tool: "git_operations", Action: "push", Effect: EffectAsk, MinRisk: RiskHigh},
				{ID: "coding:file_delete", Tool: "file_delete", Effect: EffectAsk},
				{ID: "coding:knowledge:write", Tool: "knowledge", Action: "add", Effect: EffectAsk},
				{ID: "coding:knowledge:update", Tool: "knowledge", Action: "update", Effect: EffectAsk},
				{ID: "coding:knowledge:delete", Tool: "knowledge", Action: "delete", Effect: EffectAsk},
				{ID: "coding:code_execute", Tool: "code_execute", Effect: EffectAsk},
			},
			Deny: []Rule{
				{ID: "coding:act_ui", Tool: "act_ui", Effect: EffectDeny},
				{ID: "coding:network_replay", Tool: "network_replay", Effect: EffectDeny},
				{ID: "coding:browser_execute_js", Tool: "browser_execute_js", Effect: EffectDeny},
				{ID: "coding:sensitive_path", Tool: "*", Path: "*/.ssh/*", Effect: EffectDeny, Description: "SSH 私钥目录"},
				{ID: "coding:sensitive_env", Tool: "*", Path: "*/.env", Effect: EffectDeny, Description: "环境变量密钥文件"},
			},
			DefaultDecision: EffectAsk,
		},
		ModeOffice: {
			Allow: []Rule{
				{ID: "office:web_search", Tool: "web_search", Effect: EffectAllow},
				{ID: "office:web_fetch", Tool: "web_fetch", Effect: EffectAllow},
				{ID: "office:file_read", Tool: "file_read", Effect: EffectAllow},
				{ID: "office:file_list", Tool: "file_list", Effect: EffectAllow},
				{ID: "office:file_search", Tool: "file_search", Effect: EffectAllow},
				{ID: "office:knowledge", Tool: "knowledge", Action: "search", Effect: EffectAllow},
				{ID: "office:knowledge:list", Tool: "knowledge", Action: "list", Effect: EffectAllow},
				{ID: "office:todo_write", Tool: "todo_write", Effect: EffectAllow},
				{ID: "office:create_tool", Tool: "create_tool", Effect: EffectAllow},
			},
			Ask: []Rule{
				{ID: "office:terminal_exec", Tool: "terminal_exec", Effect: EffectAsk},
				{ID: "office:git_operations", Tool: "git_operations", Effect: EffectAsk},
				{ID: "office:file_write", Tool: "file_write", Effect: EffectAsk},
				{ID: "office:file_edit", Tool: "file_edit", Effect: EffectAsk},
				{ID: "office:multi_edit", Tool: "multi_edit", Effect: EffectAsk},
				{ID: "office:file_delete", Tool: "file_delete", Effect: EffectAsk},
				{ID: "office:move_file", Tool: "move_file", Effect: EffectAsk},
				{ID: "office:act_ui", Tool: "act_ui", Effect: EffectAsk},
				{ID: "office:code_execute", Tool: "code_execute", Effect: EffectAsk},
				{ID: "office:knowledge:write", Tool: "knowledge", Action: "add", Effect: EffectAsk},
				{ID: "office:knowledge:delete", Tool: "knowledge", Action: "delete", Effect: EffectAsk},
			},
			Deny: []Rule{
				{ID: "office:sensitive_path", Tool: "*", Path: "*/.ssh/*", Effect: EffectDeny, Description: "SSH 私钥目录"},
				{ID: "office:sensitive_env", Tool: "*", Path: "*/.env", Effect: EffectDeny, Description: "环境变量密钥文件"},
			},
			DefaultDecision: EffectAsk,
		},
	}
	applyMemoryRules(configs)
	return configs
}

// applyMemoryRules 给各交互模式补上长期记忆（memory）工具的规则。
//
// 单独一个函数而不是在每个模式字面量里重复一遍：memory 的读/写分档在所有模式里
// 完全一致，重复四份只会让后来改规则的人漏掉其中一处。yolo / safe 的默认决策是
// 放行，本工具在那两档天然可用，因此这里只需要收紧 coding / office。
//
// 分档理由：search / list 只读远端记忆，不改变任何状态 → 放行；add / forget 会
// 改写长期记忆（影响之后每一次对话的上下文），属于「用户会想知道」的写操作 → 确认。
func applyMemoryRules(configs map[Mode]PermissionConfig) {
	read := func(prefix string) []Rule {
		return []Rule{
			{ID: prefix + ":memory:search", Tool: "memory", Action: "search", Effect: EffectAllow},
			{ID: prefix + ":memory:list", Tool: "memory", Action: "list", Effect: EffectAllow},
		}
	}
	write := func(prefix string) []Rule {
		return []Rule{
			{ID: prefix + ":memory:add", Tool: "memory", Action: "add", Effect: EffectAsk, Description: "写入长期记忆需确认"},
			{ID: prefix + ":memory:forget", Tool: "memory", Action: "forget", Effect: EffectAsk, Description: "删除长期记忆需确认"},
		}
	}
	for _, spec := range []struct {
		mode   Mode
		prefix string
	}{{ModeCoding, "coding"}, {ModeOffice, "office"}} {
		cfg, ok := configs[spec.mode]
		if !ok {
			continue
		}
		cfg.Allow = append(cfg.Allow, read(spec.prefix)...)
		cfg.Ask = append(cfg.Ask, write(spec.prefix)...)
		configs[spec.mode] = cfg
	}
}
