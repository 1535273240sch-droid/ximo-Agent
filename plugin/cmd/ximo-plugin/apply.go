package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/1535273240sch-droid/ximo-plugin/internal/agents"
	"github.com/1535273240sch-droid/ximo-plugin/internal/engine"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// dryRunKeyPlaceholder 是 dry-run 且没有密钥时的占位值：只为让差异能生成，
// 绝不写进任何文件（dry-run 不落盘）。
const dryRunKeyPlaceholder = "<dry-run-placeholder>"

// accessTokenPrefix 是网关 access token 的前缀（主仓库 internal/account/token.go 的
// accessPrefix）。带这个前缀的是**短期**凭据：网关侧寿命约 15 分钟
// （account.DefaultAccessTTL = 15m），写进 Agent 配置后用户过一刻钟就会开始 401。
const accessTokenPrefix = "gwa_"

// shortLivedRefusal 解释为什么默认拒绝把 access token 写进 Agent 配置，以及正确做法。
const shortLivedRefusal = `本机没有长期密钥，只有登录得到的临时访问令牌（access token，网关侧寿命约 15 分钟）。
写进 Agent 配置会让 Agent 十几分钟后开始报 401，而且本插件不会自动续期（配置里的密钥是死值），已拒绝写入。
  改用长期密钥：ximo-plugin apply … --api-key <ximo_sk_ 开头的长期 API Key>
                 （网关侧签发入口 POST /admin/keys；或请管理员给你一个长期密钥）
  确实要写临时令牌：加 --allow-short-lived-token（不推荐；到期后需要重跑本命令）`

type applyResult struct {
	SpecID string            `json:"spec_id"`
	Name   string            `json:"name,omitempty"`
	Status string            `json:"status"`         // PASS | FAIL
	Mode   string            `json:"mode,omitempty"` // file | env | ipc
	Steps  []engine.PlanStep `json:"steps,omitempty"`
	Values map[string]any    `json:"values,omitempty"`
	Error  string            `json:"error,omitempty"`
}

type planItem struct {
	spec   spec.Spec
	steps  []engine.PlanStep
	values map[string]any
	err    error
}

func (a *app) cmdApply(args []string) int {
	fs := a.flags("apply")
	gatewayURL := fs.String("gateway", "", "中转站地址")
	specID := fs.String("spec", "", "只应用某个适配器 id")
	all := fs.Bool("all", false, "对检测到的所有 Agent 依次应用")
	model := fs.String("model", "", "写入配置的模型 id（省略时尝试从网关挑选）")
	apiKey := fs.String("api-key", "", "直接给密钥，跳过本机凭据")
	dryRun := fs.Bool("dry-run", false, "只打印差异，不落盘")
	yes := fs.Bool("yes", false, "跳过交互确认")
	noIPC := fs.Bool("no-ipc", false, "不走 ximo-agent 的 IPC 帧，只改配置文件")
	ipcEndpoint := fs.String("ipc-endpoint", "",
		"ximo-agent 的 IPC 端点（默认按 $XIMO_IPC_ENDPOINT → 平台默认管道 的顺序发现）")
	showSecrets := fs.Bool("show-secrets", false, "dry-run 差异里显示真实密钥（默认打码，避免泄漏到日志）")
	allowShortLived := fs.Bool("allow-short-lived-token", false,
		"允许把登录得到的短期 access token（约 15 分钟寿命）写入 Agent 配置；默认拒绝")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	a.ipcEndpoint = strings.TrimSpace(*ipcEndpoint)
	engine.MaskSecretsInDryRun = !*showSecrets
	switch {
	case strings.TrimSpace(*gatewayURL) == "":
		return a.usageErr(errors.New("用法: ximo-plugin apply --gateway <url> [--spec <id>|--all] [--model <m>] [--api-key <k>] [--dry-run] [--yes]"))
	case *specID == "" && !*all:
		return a.usageErr(errors.New("必须指定 --spec <id> 或 --all"))
	case *specID != "" && *all:
		return a.usageErr(errors.New("--spec 与 --all 不能同时使用"))
	}
	base, err := a.resolveGateway(*gatewayURL)
	if err != nil {
		return a.usageErr(err)
	}

	specs, err := loadSpecsFn(a.home)
	if err != nil {
		return a.fail(fmt.Errorf("加载适配器规格失败: %w", err))
	}
	targets, code := a.applyTargets(specs, *specID, *all)
	if code != exitOK {
		return code
	}
	if len(targets) == 0 {
		a.warnf("没有可应用的目标：未检测到任何 Agent。用 `ximo-plugin detect --all` 查看证据，或用 --spec <id> 指定。")
		return exitOK
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := strings.TrimSpace(*apiKey)
	// 短期凭据判定：--api-key 给的就是 gwa_ 令牌，或本机凭据文件里只有 access token
	// （登录链的产物，寿命约 15 分钟）。这两种都不该写进 Agent 配置。
	shortLived := strings.HasPrefix(key, accessTokenPrefix)
	if key == "" {
		if cred, cerr := newGatewayClient(base, a.home).EnsureCred(ctx); cerr == nil {
			key = cred.Bearer()
			shortLived = shortLived || (cred.APIKey == "" && cred.AccessToken != "")
		}
	}
	if key == "" && !*dryRun {
		return a.fail(fmt.Errorf("没有可用密钥：用 --api-key 指定，或先执行 `ximo-plugin login --gateway %s`", base))
	}
	if shortLived && !*allowShortLived {
		return a.fail(errors.New(shortLivedRefusal))
	}
	if shortLived {
		a.warnf("即将写入的凭据是临时访问令牌（access token，寿命约 15 分钟）：到期后 Agent 会开始报 401，本插件不会自动续期。")
		a.notef("      长期方案：改用 --api-key <ximo_sk_ 开头的长期密钥>（网关侧签发入口 POST /admin/keys）")
	}

	modelID := strings.TrimSpace(*model)
	if modelID == "" && key != "" && specsNeedModel(targets) {
		if picked, perr := a.pickModel(ctx, base, key); perr == nil {
			modelID = picked
			a.notef("未指定 --model，已从网关挑选：%s", modelID)
		} else {
			a.warnf("无法从网关自动挑选模型：%v", perr)
		}
	}

	in := engine.Inputs{GatewayURL: base, Model: modelID, Home: a.home, IsolateHome: a.isolateHome, APIKey: key}
	if in.APIKey == "" {
		in.APIKey = dryRunKeyPlaceholder
		a.warnf("dry-run：没有可用密钥，差异里用占位符代替（不落盘、不显示真实密钥）")
	}

	items := make([]planItem, 0, len(targets))
	for _, s := range targets {
		steps, values, perr := engine.BuildPlan(s, in)
		items = append(items, planItem{spec: s, steps: steps, values: values, err: perr})
	}

	mode := "将执行以下变更"
	if *dryRun {
		mode = "（dry-run）以下变更不会被写入"
	}
	a.notef("目标网关：%s %s", base, mode)
	applicable := 0
	for _, it := range items {
		viaIPC := a.ipcEligible(it.spec, *noIPC)
		if it.err != nil && !viaIPC {
			a.failf("[%s] 无法生成计划: %v", it.spec.ID, it.err)
			continue
		}
		applicable++
		if viaIPC {
			a.notef("[%s] %s（走 IPC 帧，ximo-agent 运行时即时生效；配置文件是回退路径）", it.spec.ID, it.spec.Name)
			if it.err != nil {
				a.notef("    规格声明的文件落点不可用：%v", it.err)
				a.notef("    若 IPC 也不可用，回退改文件由 agents 侧按平台默认路径处理：%s", agents.DefaultConfigPath(a.home))
			}
		} else {
			a.notef("[%s] %s", it.spec.ID, it.spec.Name)
		}
		if viaIPC && *dryRun {
			a.notef("    dry-run 会只读地连一次 IPC（system.config.get）打印真实差异：不写安全存储、不改配置")
		}
		for _, st := range it.steps {
			switch st.Kind {
			case engine.KindFile:
				state := "新建"
				if st.Exists {
					state = "修改"
				}
				a.notef("    %s %s  %s", state, st.Target, st.Summary)
			case engine.KindEnv:
				a.notef("    环境变量 %s  %s", strings.Join(st.Vars, " "), st.Summary)
			case engine.KindIPC:
				a.notef("    IPC %s  %s", st.Target, st.Summary)
			}
		}
		if it.spec.RestartNote != "" {
			a.notef("    重启提示：%s", it.spec.RestartNote)
		}
	}
	if applicable == 0 {
		var results []applyResult
		for _, it := range items {
			results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "FAIL", Error: it.err.Error()})
		}
		a.failf("没有可执行的计划（全部在生成阶段失败）")
		return a.applySummary(results)
	}

	if !*dryRun && !*yes {
		ok, cerr := a.confirm(fmt.Sprintf("确认对以上 %d 个适配器写入配置？输入 yes 继续：", applicable))
		if cerr != nil {
			return a.fail(cerr)
		}
		if !ok {
			a.notef("已取消，未做任何修改。")
			return exitFail
		}
	}

	// 单个适配器失败不中断其余：逐个执行并记录结果。
	results := make([]applyResult, 0, len(items))
	for _, it := range items {
		if a.ipcEligible(it.spec, *noIPC) {
			// 备份、dry-run 差异、安全存储不可用时的逐条告警、以及「只回传该回传
			// 的字段」都由 agents.ApplyXimoAgent 负责（P-AgentIPC 的权威实现）。
			ierr := a.applyXimoAgentIPC(in, ximoAgentFallbackPath(it.steps), key, *dryRun)
			if ierr == nil {
				results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "PASS", Mode: "ipc", Values: it.values})
				if *dryRun {
					a.notef("[%s] dry-run 完成（未写入；IPC 侧只读了运行时配置）", it.spec.ID)
				} else {
					a.passf("[%s] 已通过 IPC 帧写入运行时配置（即时生效；配置文件已备份）", it.spec.ID)
				}
				continue
			}
			// 走到这里说明 IPC 与配置文件回退两条路都失败了：ApplyXimoAgent 只在
			// 「连不上 agent」时回退，且回退本身也会告警/报错。
			results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "FAIL", Error: ierr.Error()})
			a.failf("[%s] %v", it.spec.ID, ierr)
			continue
		}
		if it.err != nil {
			results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "FAIL", Error: it.err.Error()})
			continue
		}
		if len(it.steps) == 0 {
			msg := "适配器没有可执行的步骤（spec 未声明 files/env）"
			results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "FAIL", Error: msg})
			a.failf("[%s] %s", it.spec.ID, msg)
			continue
		}
		// 只有环境变量形态的规格：插件本就不落盘，绝不能报"已写入配置"。
		// 按 spec 的 style 打印可直接执行的语句，并如实标注"未写入文件"。
		if !hasFileStep(it.steps) {
			lines, lerr := engine.EnvLines(it.spec, in, "")
			if lerr != nil {
				results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "FAIL", Mode: "env", Steps: it.steps, Error: lerr.Error()})
				a.failf("[%s] 生成环境变量语句失败: %v", it.spec.ID, lerr)
				continue
			}
			results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "PASS", Mode: "env", Steps: it.steps, Values: it.values})
			a.passf("[%s] 已输出环境变量（未写入文件）", it.spec.ID)
			for _, l := range lines {
				a.notef("      %s", l)
			}
			continue
		}
		if aerr := engine.Apply(it.steps, *dryRun); aerr != nil {
			results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "FAIL", Mode: "file", Steps: it.steps, Error: aerr.Error()})
			a.failf("[%s] %v", it.spec.ID, aerr)
			continue
		}
		results = append(results, applyResult{SpecID: it.spec.ID, Name: it.spec.Name, Status: "PASS", Mode: "file", Steps: it.steps, Values: it.values})
		if *dryRun {
			a.notef("[%s] dry-run 完成（未写入）", it.spec.ID)
		} else {
			a.passf("[%s] 已写入配置（原文件备份为 *.bak-<unix>）", it.spec.ID)
		}
	}

	// 同时声明了文件与环境变量的适配器：文件已写，再提示一遍环境变量怎么设。
	// （纯环境变量形态的已经在上面打印过可直接执行的语句了，不重复。）
	for _, it := range items {
		if !hasFileStep(it.steps) {
			continue
		}
		for _, st := range it.steps {
			if st.Kind == engine.KindEnv && len(st.Vars) > 0 {
				a.notef("[%s] 需要环境变量（插件不落盘）：%s", it.spec.ID, strings.Join(st.Vars, " "))
				a.notef("      生成设置语句：ximo-plugin print-env --gateway %s --spec %s", base, it.spec.ID)
			}
		}
	}
	return a.applySummary(results)
}

// hasFileStep 报告计划里是否有真正落盘的步骤。没有就说明这个适配器只有环境变量形态。
func hasFileStep(steps []engine.PlanStep) bool {
	for _, st := range steps {
		if st.Kind == engine.KindFile {
			return true
		}
	}
	return false
}

// ximoAgentFallbackPath 取计划里声明的目标文件路径，作为 agents 侧「IPC 不可用
// 时回退改文件」的落点：规格是本 CLI 文件路径的权威，用户改了规格的
// files[0].path，回退写入就必须跟着改，否则打印的计划与实际落点会不一致。
// 计划生成失败（没有文件步骤）时返回空串，交给 agents 用它自己的平台默认路径。
func ximoAgentFallbackPath(steps []engine.PlanStep) string {
	for _, st := range steps {
		if st.Kind == engine.KindFile && st.Target != "" {
			return st.Target
		}
	}
	return ""
}

func (a *app) applyTargets(specs []spec.Spec, specID string, all bool) ([]spec.Spec, int) {
	if !all {
		s, ok := engine.FindSpec(specs, specID)
		if !ok {
			var ids []string
			for _, x := range specs {
				ids = append(ids, x.ID)
			}
			return nil, a.fail(fmt.Errorf("没有 id=%q 的适配器（可用：%s）", specID, strings.Join(ids, ", ")))
		}
		if len(engine.Found(engine.DetectIn([]spec.Spec{s}, a.expand()))) == 0 {
			a.warnf("适配器 %s 在本机没有检测到证据，仍按 --spec 继续。", specID)
		}
		return []spec.Spec{s}, exitOK
	}
	var out []spec.Spec
	for _, d := range engine.Found(engine.DetectIn(specs, a.expand())) {
		if s, ok := engine.FindSpec(specs, d.SpecID); ok {
			out = append(out, s)
		}
	}
	return out, exitOK
}

// specsNeedModel 判断这些规格是否真的需要 model（只有需要时才去问网关挑模型）。
func specsNeedModel(specs []spec.Spec) bool {
	for _, s := range specs {
		for _, t := range s.Files {
			for _, f := range t.Fields {
				if f.From == spec.FromModel || f.From == spec.FromDisplayName {
					return true
				}
			}
		}
	}
	return false
}

func (a *app) pickModel(ctx context.Context, base, key string) (string, error) {
	client := newGatewayClient(base, a.home)
	cred := gatewayCred(base, key)
	models, err := client.ListModels(ctx, cred)
	if err != nil {
		return "", err
	}
	m, err := client.PickModel(models, "")
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

// applySummary 打印 PASS/FAIL 汇总；有 FAIL 时返回非 0 退出码。
// 只输出环境变量、没写任何文件的适配器会单独计数，免得"PASS"被当成"已落盘"。
func (a *app) applySummary(results []applyResult) int {
	pass, failed, envOnly := 0, 0, 0
	for _, r := range results {
		if r.Status == "PASS" {
			pass++
		} else {
			failed++
		}
		if r.Mode == "env" && r.Status == "PASS" {
			envOnly++
		}
	}
	if envOnly > 0 {
		a.notef("汇总：PASS %d / FAIL %d（其中 %d 个只输出了环境变量，未写入任何文件）", pass, failed, envOnly)
	} else {
		a.notef("汇总：PASS %d / FAIL %d", pass, failed)
	}
	if a.json {
		payload := map[string]any{"results": results, "pass": pass, "fail": failed, "ok": failed == 0}
		if b, err := marshalIndent(payload); err == nil {
			fmt.Fprintln(a.out, string(b))
		}
	}
	if failed > 0 {
		return exitFail
	}
	return exitOK
}

// confirm 交互确认：非 tty 不等待输入，直接报错要求 --yes。
func (a *app) confirm(prompt string) (bool, error) {
	if !a.tty {
		return false, errors.New("stdin 不是交互终端，不会等待确认；请加 --yes 明确同意，或加 --dry-run 只看差异")
	}
	fmt.Fprintf(a.err, "%s ", prompt)
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil {
		// stdin 已到末尾（如 </dev/null 或 Ctrl-D）：当作"没确认"，不做任何修改。
		if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
			return false, nil
		}
		if strings.TrimSpace(line) == "" {
			return false, fmt.Errorf("读取确认输入失败: %w", err)
		}
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "yes", "y":
		return true, nil
	default:
		return false, nil
	}
}
