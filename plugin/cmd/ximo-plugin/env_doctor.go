package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/1535273240sch-droid/ximo-plugin/internal/agents"
	"github.com/1535273240sch-droid/ximo-plugin/internal/engine"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

func (a *app) cmdPrintEnv(args []string) int {
	fs := a.flags("print-env")
	gatewayURL := fs.String("gateway", "", "中转站地址")
	specID := fs.String("spec", "", "按哪个适配器声明变量名（默认 env-openai）")
	model := fs.String("model", "", "模型 id")
	apiKey := fs.String("api-key", "", "直接给密钥，跳过本机凭据")
	style := fs.String("style", "", "export|set|powershell（默认按规格声明或当前系统）")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	base, err := a.resolveGateway(*gatewayURL)
	if err != nil {
		return a.usageErr(err)
	}
	specs, err := loadSpecsFn(a.home)
	if err != nil {
		return a.fail(fmt.Errorf("加载适配器规格失败: %w", err))
	}
	var s spec.Spec
	if *specID != "" {
		found, ok := engine.FindSpec(specs, *specID)
		if !ok {
			return a.fail(fmt.Errorf("没有 id=%q 的适配器", *specID))
		}
		s = found
	} else {
		picked, ok := pickEnvSpec(specs)
		if !ok {
			return a.fail(errors.New("没有声明环境变量的适配器，请用 --spec 指定"))
		}
		s = picked
	}

	key := strings.TrimSpace(*apiKey)
	if key == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cred, cerr := newGatewayClient(base, a.home).EnsureCred(ctx); cerr == nil {
			key = cred.Bearer()
		} else {
			a.warnf("%v", cerr)
		}
	}
	in := engine.Inputs{GatewayURL: base, Model: *model, Home: a.home, IsolateHome: a.isolateHome, APIKey: key}
	lines, lerr := engine.EnvLines(s, in, *style)
	if lerr != nil {
		return a.usageErr(lerr)
	}
	if a.json {
		return a.emitJSON(map[string]any{"spec_id": s.ID, "lines": lines})
	}
	for _, l := range lines {
		fmt.Fprintln(a.out, l)
	}
	return exitOK
}

// pickEnvSpec 优先 env-openai，其次 env-anthropic，最后第一个声明了环境变量的规格。
func pickEnvSpec(specs []spec.Spec) (spec.Spec, bool) {
	for _, want := range []string{"env-openai", "env-anthropic"} {
		if s, ok := engine.FindSpec(specs, want); ok && hasEnv(s) {
			return s, true
		}
	}
	for _, s := range specs {
		if hasEnv(s) {
			return s, true
		}
	}
	return spec.Spec{}, false
}

func hasEnv(s spec.Spec) bool { return !s.Env.Empty() }

type doctorItem struct {
	Name   string `json:"name"`
	Status string `json:"status"` // PASS | FAIL | WARN
	Detail string `json:"detail"`
}

func (a *app) cmdDoctor(args []string) int {
	fs := a.flags("doctor")
	gatewayURL := fs.String("gateway", "", "中转站地址（可选）")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	var items []doctorItem
	add := func(name, status, detail string) {
		items = append(items, doctorItem{Name: name, Status: status, Detail: detail})
	}

	var detected []engine.Detection
	specs, err := loadSpecsFn(a.home)
	switch {
	case err != nil:
		add("适配器规格", "FAIL", err.Error())
	case len(specs) == 0:
		add("适配器规格", "FAIL", "没有加载到任何规格（内置库为空？）")
	default:
		detected = engine.Found(engine.DetectIn(specs, a.expand()))
		add("适配器规格", "PASS", fmt.Sprintf("%d 个（home=%s）", len(specs), a.home))
		if len(detected) == 0 {
			add("本机 Agent 检测", "WARN", "未检测到任何 Agent；仍可用 --spec 显式指定")
		} else {
			var ids []string
			for _, d := range detected {
				ids = append(ids, d.SpecID)
			}
			add("本机 Agent 检测", "PASS", fmt.Sprintf("%d 个：%s", len(detected), strings.Join(ids, ", ")))
		}
	}

	// 凭据文件权限由 P-Client 判定（Windows 上无法用权限位验证，一律 WARN）。
	perm, perr := newGatewayClient(strings.TrimSpace(*gatewayURL), a.home).CredPerm()
	switch {
	case perr != nil:
		add("凭据文件", "WARN", perr.Error())
	case !perm.Exists:
		add("凭据文件", "WARN", fmt.Sprintf("%s 不存在（未 login；apply 需要 --api-key）", perm.Path))
	case perm.Warning != "":
		add("凭据文件", "WARN", perm.Warning)
	case perm.Secure:
		add("凭据文件", "PASS", fmt.Sprintf("%s（%04o）", perm.Path, perm.Mode))
	default:
		add("凭据文件", "PASS", fmt.Sprintf("%s（权限由平台决定）", perm.Path))
	}

	// 写进 Agent 配置的到底是什么凭据：access token 只有约 15 分钟寿命，写进配置的
	// 那一刻起就开始倒计时，用户过一会儿就会发现 Agent 全报 401 却查不出原因。
	if it, ok := a.inspectConfigCredentials(specs, strings.TrimSpace(*gatewayURL)); ok {
		items = append(items, it)
	}

	// ximo-agent 的 IPC 端点：只在检测到它且平台支持时探测，探测失败只是 WARN（agent 没在跑很正常）。
	for _, d := range detected {
		if d.SpecID != ximoAgentSpecID {
			continue
		}
		endpoint := defaultIPCEndpoint()
		if endpoint == "" {
			add("ximo-agent IPC", "WARN", "本平台不支持命名管道（P-AgentIPC 仅 Windows），将走配置文件回退路径")
			break
		}
		if perr := pingIPC(endpoint); perr != nil {
			add("ximo-agent IPC", "WARN", fmt.Sprintf("%s 不可达（%v）；将走配置文件回退路径", endpoint, perr))
		} else {
			add("ximo-agent IPC", "PASS", fmt.Sprintf("%s 可达（配置可即时生效）", endpoint))
		}
	}

	if strings.TrimSpace(*gatewayURL) == "" {
		add("网关连通性", "WARN", "未提供 --gateway，已跳过")
	} else {
		base, gerr := a.resolveGateway(*gatewayURL)
		if gerr != nil {
			add("网关连通性", "FAIL", gerr.Error())
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client := newGatewayClient(base, a.home)
			if h, herr := client.Health(ctx); herr != nil {
				add("网关健康", "FAIL", fmt.Sprintf("%s：%v", base, herr))
			} else {
				add("网关健康", "PASS", fmt.Sprintf("%s status=%s version=%s", base, h.Status, h.Version))
			}
			if cred, cerr := client.EnsureCred(ctx); cerr != nil {
				add("网关模型", "WARN", fmt.Sprintf("无可用凭据（%v），已跳过", cerr))
			} else if models, merr := client.ListModels(ctx, cred); merr != nil {
				add("网关模型", "FAIL", merr.Error())
			} else {
				add("网关模型", "PASS", fmt.Sprintf("%d 个可用模型", len(models)))
			}
		}
	}

	if a.json {
		ok := true
		for _, it := range items {
			if it.Status == "FAIL" {
				ok = false
			}
		}
		return a.emitJSONResult(map[string]any{"items": items, "ok": ok}, ok)
	}
	failed := 0
	for _, it := range items {
		switch it.Status {
		case "PASS":
			a.passf("%s：%s", it.Name, it.Detail)
		case "FAIL":
			failed++
			a.failf("%s：%s", it.Name, it.Detail)
		default:
			a.warnf("%s：%s", it.Name, it.Detail)
		}
	}
	if failed > 0 {
		return exitFail
	}
	return exitOK
}

// pingIPC 用 agents 包的心跳探活（一问一答，不重连）。
func pingIPC(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := agents.NewClient(endpoint, 3*time.Second)
	if err := client.Connect(ctx); err != nil {
		return err
	}
	defer client.Close()
	return client.Ping(ctx)
}

// emitJSONResult 输出 JSON 后按 ok 决定退出码。
func (a *app) emitJSONResult(v any, ok bool) int {
	if code := a.emitJSON(v); code != exitOK {
		return code
	}
	if !ok {
		return exitFail
	}
	return exitOK
}

// accessTokenShape 匹配网关签发形态的 access token（gwa_ 前缀），用于识别"不知道是哪张
// 令牌、但一看就不是长期 API Key"的配置。
var accessTokenShape = regexp.MustCompile(`gwa_[A-Za-z0-9_-]{8,}`)

// inspectConfigCredentials 检查各适配器目标配置文件里写的凭据是长期 API Key 还是短期
// access token。返回 (体检项, 是否值得报告)：一个已存在的目标配置都没有时没什么可查的。
func (a *app) inspectConfigCredentials(specs []spec.Spec, gatewayURL string) (doctorItem, bool) {
	const name = "Agent 配置里的凭据"
	var files []string
	// 路径展开只走 spec 这一份实现（与 engine.BuildPlan / Detect 同一套语义）。
	// 展不开（引用了未定义的变量等）的目标跳过：doctor 不该因此报错。
	for _, s := range specs {
		for _, t := range s.Files {
			p, eerr := spec.ExpandPathOpts(t.Path, a.expand())
			if eerr != nil || p == "" {
				continue
			}
			if _, err := os.Stat(p); err == nil {
				files = append(files, p)
			}
		}
	}
	if len(files) == 0 {
		return doctorItem{}, false
	}

	// 本机的 access token：配置里出现它，说明当初是（用 --allow-short-lived-token 或旧版本）
	// 把登录令牌写进了配置。
	var shortToken string
	if cred, err := newGatewayClient(gatewayURL, a.home).LoadCred(); err == nil && cred.APIKey == "" {
		shortToken = cred.AccessToken
	}

	var shortLived, longLived, unreadable []string
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", p, err))
			continue
		}
		body := string(raw)
		switch {
		case (shortToken != "" && strings.Contains(body, shortToken)) || accessTokenShape.MatchString(body):
			shortLived = append(shortLived, p)
		case strings.Contains(body, "ximo_sk_"):
			longLived = append(longLived, p)
		}
	}
	switch {
	case len(shortLived) > 0:
		return doctorItem{Name: name, Status: "FAIL", Detail: fmt.Sprintf(
			"%s 里写的是登录得到的 access token（网关侧寿命约 15 分钟）：过期后 Agent 会一直 401，本插件不会自动续期。"+
				"请用 --api-key <长期密钥> 重跑 `ximo-plugin apply`（网关侧签发入口 POST /admin/keys）",
			strings.Join(shortLived, ", "))}, true
	case len(longLived) == len(files):
		return doctorItem{Name: name, Status: "PASS", Detail: fmt.Sprintf(
			"%d 个配置文件里写的是长期 API Key（ximo_sk_）", len(longLived))}, true
	default:
		detail := fmt.Sprintf("%d 个目标配置里没有网关密钥（多是只写了 base_url，或该 Agent 用环境变量形态）",
			len(files)-len(longLived))
		if len(unreadable) > 0 {
			detail += "；另有读取失败：" + strings.Join(unreadable, ", ")
		}
		return doctorItem{Name: name, Status: "WARN", Detail: detail}, true
	}
}
