package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/1535273240sch-droid/ximo-plugin/internal/engine"
	"github.com/1535273240sch-droid/ximo-plugin/internal/gateway"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
	"github.com/1535273240sch-droid/ximo-plugin/internal/ui"
)

// defaultUIPort 是界面默认端口（契约 §1）。
const defaultUIPort = 8787

// uiShortLivedRefusal 是界面拒绝把登录令牌写进 Agent 配置的理由。UI 刻意不提供
// --allow-short-lived-token 的等价开关（契约 §2.4：要看真值/要绕过都请用 CLI）。
const uiShortLivedRefusal = `本机凭据里只有登录得到的临时访问令牌（access token，网关侧寿命约 15 分钟）：` +
	`写进 Agent 配置后过一刻钟就开始 401，界面不提供绕过开关，已拒绝写入。
  请改用长期密钥：在「应用」页粘贴 ximo_sk_ 开头的长期 API Key（网关侧签发入口 POST /admin/keys），
  或用 CLI：ximo-plugin apply --gateway <url> --spec <id> --api-key <长期密钥>`

// uiShortLivedPreview 是 plan（不落盘）在遇到短期令牌时的提示：可以看差异，
// 但真正写盘会被拒绝。
const uiShortLivedPreview = "本次预览用的是登录得到的临时访问令牌（约 15 分钟寿命）：预览不落盘，" +
	"但「应用」会被拒绝——请改用长期密钥（ximo_sk_ 开头）"

// cmdUI 启动本地网页界面（契约 §1）。这里只做接线：HTTP/令牌/静态资源在 internal/ui，
// 业务全部复用既有包（engine/gateway/spec/agents）——本文件不重写任何业务逻辑。
func (a *app) cmdUI(args []string) int {
	fs := a.flags("ui")
	port := fs.Int("port", defaultUIPort, "本地界面端口（只绑 127.0.0.1）")
	noOpen := fs.Bool("no-open", false, "不自动打开浏览器，只打印地址")
	gatewayURL := fs.String("gateway", "", "中转站地址（省略时用已保存凭据里的网关）")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	if *port < 0 || *port > 65535 {
		return a.usageErr(fmt.Errorf("--port 必须在 0..65535（0 表示由系统分配一个空闲端口）"))
	}

	base := ""
	if strings.TrimSpace(*gatewayURL) != "" {
		b, err := a.resolveGateway(*gatewayURL)
		if err != nil {
			return a.usageErr(err)
		}
		base = b
	}

	be := &uiBackend{app: a, gateway: base}
	srv, err := ui.New(ui.Config{Port: *port, Backend: be})
	if err != nil {
		return a.fail(err)
	}
	if err := srv.Listen(); err != nil {
		return a.fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	a.notef("ximo-plugin UI %s", version)
	a.notef("界面：%s", srv.BaseURL())
	if base != "" {
		a.notef("网关：%s", base)
	} else {
		a.notef("网关：未指定（界面上登录后会自动用凭据里的网关）")
	}
	if a.isolateHome {
		a.notef("隔离模式：%s（--home 生效，%%APPDATA%% 等也按它解析）", a.home)
	} else {
		a.notef("用户主目录：%s", a.home)
	}
	// 令牌只在这里出现一次：打开浏览器或（--no-open / 打不开时）交给用户手动粘贴。
	// 打印带令牌的地址是唯一的人机通道，请勿把它写进日志、工单或聊天记录。
	switch {
	case *noOpen:
		a.notef("地址（含本次会话令牌，请勿贴进日志/聊天记录）：%s", srv.URL())
	default:
		if err := openBrowser(srv.URL()); err != nil {
			a.warnf("未能自动打开浏览器（%v）", err)
			a.notef("地址（含本次会话令牌，请勿贴进日志/聊天记录）：%s", srv.URL())
		} else {
			a.notef("已在浏览器中打开（地址含本次会话令牌，不会再次打印；关掉浏览器后可用 --no-open 重跑取地址）")
		}
	}
	a.notef("按 Ctrl+C 停止")
	if err := srv.Serve(ctx); err != nil {
		return a.fail(err)
	}
	a.notef("界面已停止")
	return exitOK
}

// openBrowser 尽力打开系统默认浏览器；打不开不算失败（调用方只给个告警）。
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// newGatewayConcrete 与 newGatewayClient 是同一套构造（同样的凭据路径），但返回具体
// 类型：deps.go 里冻结的 gatewayAPI 只有「轮询到成功」的 PollDeviceLogin，而界面要的
// 是「轮询一次，pending 就原样返回」，只有具体类型提供 PollDeviceLoginOnce。
// 声明为变量便于单测注入。
var newGatewayConcrete = func(baseURL, home string) *gateway.Client {
	return gateway.NewClient(baseURL, gateway.CredPathIn(home))
}

// uiBackend 把 internal/ui 的 Backend 接口接到既有的 app 上。
type uiBackend struct {
	app     *app
	gateway string // --gateway；空时用已保存凭据里的网关

	// mu 串行化 plan/apply：engine.Out 与 engine.MaskSecretsInDryRun 是包级状态
	// （差异生成要用它们），并发调用会互相串味。写入也是破坏性操作，串行更安全。
	mu sync.Mutex
	// deviceCode 是本次登录流程的设备码：等价于短期凭据，只留在服务端。
	deviceMu   sync.Mutex
	deviceCode string
}

// baseURL 决定本次调用访问哪个网关：--gateway 优先，其次已保存凭据里的网关。
func (b *uiBackend) baseURL() (string, error) {
	if b.gateway != "" {
		return b.gateway, nil
	}
	if cred, err := newGatewayClient("", b.app.home).LoadCred(); err == nil && strings.TrimSpace(cred.Gateway) != "" {
		return cred.Gateway, nil
	}
	return "", ui.Errorf("no_gateway",
		"未设置中转站地址：请用 --gateway 启动界面（例 --gateway http://127.0.0.1:8600），或先在界面上完成登录")
}

// adapters 是 state 与 detect 共用的检测结果（复用 engine.DetectIn，不重写检测逻辑）。
func (b *uiBackend) adapters() ([]ui.Adapter, error) {
	specs, err := loadSpecsFn(b.app.home)
	if err != nil {
		return nil, ui.Errorf("specs_failed", "加载适配器规格失败: %v", err)
	}
	dets := engine.DetectIn(specs, b.app.expand())
	out := make([]ui.Adapter, 0, len(dets))
	for _, d := range dets {
		out = append(out, ui.Adapter{ID: d.SpecID, Name: d.Name, Detected: d.Found, Evidence: d.Evidence})
	}
	return out, nil
}

func (b *uiBackend) State(context.Context) (ui.State, error) {
	adapters, err := b.adapters()
	if err != nil {
		return ui.State{}, err
	}
	base, _ := b.baseURL() // 没设网关不算错：界面上照实报告，用户去登录页填
	st := ui.State{
		Version:      version,
		Home:         b.app.home,
		IsolatedHome: b.app.isolateHome,
		Gateway:      base,
		Adapters:     adapters,
	}
	// 凭据只回掩码形态（gateway.Cred.Fingerprint 本身只给掩码）。
	if cred, cerr := newGatewayClient(base, b.app.home).LoadCred(); cerr == nil && !cred.Empty() {
		st.LoggedIn = true
		st.CredMasked = cred.Fingerprint()
	}
	return st, nil
}

func (b *uiBackend) Detect(context.Context) ([]ui.Adapter, error) { return b.adapters() }

func (b *uiBackend) LoginStart(ctx context.Context) (ui.LoginStart, error) {
	base, err := b.baseURL()
	if err != nil {
		return ui.LoginStart{}, err
	}
	da, err := newGatewayClient(base, b.app.home).StartDeviceLogin(ctx)
	if err != nil {
		return ui.LoginStart{}, ui.Errorf("login_failed", "%v", err)
	}
	b.deviceMu.Lock()
	b.deviceCode = da.DeviceCode
	b.deviceMu.Unlock()

	// DeviceAuth.DeviceCode 等价于短期凭据：只留在服务端做轮询，不回给前端。
	out := ui.LoginStart{UserCode: da.UserCode, VerificationURI: da.VerificationURI}
	if da.ExpiresIn > 0 {
		out.ExpiresIn = int64(da.ExpiresIn / time.Second)
	}
	if da.Interval > 0 {
		out.Interval = int64(da.Interval / time.Second)
	}
	return out, nil
}

func (b *uiBackend) LoginPoll(ctx context.Context) (ui.LoginPoll, error) {
	base, err := b.baseURL()
	if err != nil {
		return ui.LoginPoll{}, err
	}
	b.deviceMu.Lock()
	deviceCode := b.deviceCode
	b.deviceMu.Unlock()
	if deviceCode == "" {
		return ui.LoginPoll{}, ui.Errorf("login_not_started", "还没有开始登录：请先点击「开始登录」")
	}

	client := newGatewayConcrete(base, b.app.home)
	cred, err := client.PollDeviceLoginOnce(ctx, deviceCode)
	switch {
	case errors.Is(err, gateway.ErrAuthorizationPending), errors.Is(err, gateway.ErrSlowDown):
		// 用户还没在授权页确认：不是错误，前端继续轮询即可。
		return ui.LoginPoll{Status: "pending"}, nil
	case err != nil:
		return ui.LoginPoll{}, ui.Errorf("login_failed", "%v", err)
	}
	b.deviceMu.Lock()
	b.deviceCode = ""
	b.deviceMu.Unlock()

	if serr := client.SaveCred(cred); serr != nil {
		// 凭据本身有效（本次会话可用），但没落盘：如实告诉用户要重新登录，不假装成功。
		return ui.LoginPoll{Status: "ok", CredMasked: cred.Fingerprint(),
			Warning: "登录成功但凭据写入失败（" + serr.Error() + "）：请重新登录以持久化"}, nil
	}
	return ui.LoginPoll{Status: "ok", CredMasked: cred.Fingerprint()}, nil
}

func (b *uiBackend) Models(ctx context.Context) ([]gateway.Model, error) {
	base, err := b.baseURL()
	if err != nil {
		return nil, err
	}
	cred, err := b.app.credFor(ctx, base, "")
	if err != nil {
		return nil, ui.Errorf("no_credential", "%v", err)
	}
	models, err := newGatewayClient(base, b.app.home).ListModels(ctx, cred)
	if err != nil {
		return nil, ui.Errorf("gateway_failed", "%v", err)
	}
	return models, nil
}

func (b *uiBackend) Plan(ctx context.Context, req ui.PlanRequest) (ui.Plan, error) {
	base, s, err := b.target(req.SpecID)
	if err != nil {
		return ui.Plan{}, err
	}

	// plan 不落盘，所以没有密钥时按 CLI 的 dry-run 一样用占位符生成差异（不显示明文）。
	key, shortLived, kerr := b.resolveKey(ctx, base, req.APIKey)
	warning := ""
	switch {
	case kerr != nil:
		key, shortLived = dryRunKeyPlaceholder, false
		warning = "没有可用密钥（" + kerr.Error() + "）：差异用占位符生成，只作预览"
	case shortLived:
		warning = uiShortLivedPreview
	}

	model, err := b.pickModel(ctx, base, key, req.Model, s)
	if err != nil {
		return ui.Plan{}, err
	}
	steps, _, err := engine.BuildPlan(s, engine.Inputs{
		GatewayURL: base, Model: model, Home: b.app.home, IsolateHome: b.app.isolateHome, APIKey: key,
	})
	if err != nil {
		return ui.Plan{}, ui.Errorf("plan_failed", "%v", err)
	}

	b.mu.Lock()
	diff := b.dryRunDiff(steps)
	b.mu.Unlock()

	items := make([]ui.PlanItem, 0, len(steps))
	for _, st := range steps {
		items = append(items, ui.PlanItem{SpecID: st.SpecID, Kind: st.Kind, Target: st.Target, Summary: st.Summary})
	}
	return ui.Plan{Items: items, Diff: diff, Warning: warning}, nil
}

func (b *uiBackend) Apply(ctx context.Context, req ui.PlanRequest) ([]ui.ApplyItem, error) {
	// handler 层也拦了一道，这里再拦一道：写盘必须显式确认。
	if !req.Confirm {
		return nil, ui.Errorf("confirm_required", "未确认：写盘请求必须带 confirm:true")
	}
	base, s, err := b.target(req.SpecID)
	if err != nil {
		return nil, err
	}

	key, shortLived, err := b.resolveKey(ctx, base, req.APIKey)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ui.Errorf("missing_api_key", "没有可用密钥：请在界面粘贴长期密钥（ximo_sk_ 开头），或用 CLI 执行 login/apply")
	}
	if shortLived {
		// 与 CLI 的默认策略一致：短期 access token 不写进 Agent 配置。界面没有绕过开关。
		return nil, ui.Errorf("short_lived_token", "%s", uiShortLivedRefusal)
	}

	model, err := b.pickModel(ctx, base, key, req.Model, s)
	if err != nil {
		return nil, err
	}
	in := engine.Inputs{
		GatewayURL: base, Model: model, Home: b.app.home, IsolateHome: b.app.isolateHome, APIKey: key,
	}
	steps, _, err := engine.BuildPlan(s, in)
	if err != nil {
		return nil, ui.Errorf("plan_failed", "%v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// ximo-agent 走 IPC 帧（与 CLI 同一条路）：备份、dry-run 差异、安全存储不可用时
	// 的告警都由 agents.ApplyXimoAgent 负责，这里不绕过。
	if b.app.ipcEligible(s, false) {
		if ierr := b.app.applyXimoAgentIPC(in, ximoAgentFallbackPath(steps), key, false); ierr != nil {
			return []ui.ApplyItem{{SpecID: s.ID, Target: s.ID, Result: "failed", Message: ierr.Error()}}, nil
		}
		return []ui.ApplyItem{{SpecID: s.ID, Target: s.ID, Result: "ok",
			Message: "已通过 IPC 帧写入运行时配置（即时生效；配置文件已备份）" + restartNote(s)}}, nil
	}
	return b.applyFiles(s, steps), nil
}

// applyFiles 落盘（engine.Apply 负责备份、0700 补建父目录与失败回滚）。
func (b *uiBackend) applyFiles(s spec.Spec, steps []engine.PlanStep) []ui.ApplyItem {
	var files []engine.PlanStep
	envTarget := ""
	for _, st := range steps {
		switch st.Kind {
		case engine.KindFile:
			files = append(files, st)
		case engine.KindEnv:
			envTarget = st.Target
		}
	}
	if len(files) == 0 {
		// 只认环境变量的适配器：插件不落盘，也**不把密钥回给前端**（要看真值请用 CLI
		// 的 ximo-plugin print-env，那里是给 shell 用的）。
		msg := "该适配器只认环境变量，插件不落盘、也不回传密钥：请在终端执行 ximo-plugin print-env 生成设置语句"
		if envTarget != "" {
			msg = fmt.Sprintf("需要设置的环境变量：%s（插件不落盘，也不回传密钥）；生成设置语句：ximo-plugin print-env --gateway %s --spec %s",
				envTarget, b.gatewayForMessage(), s.ID)
		}
		return []ui.ApplyItem{{SpecID: s.ID, Target: "env", Result: "ok", Message: msg}}
	}

	if err := engine.Apply(steps, false); err != nil {
		// engine.Apply 失败时会整体回滚（internal/engine/apply.go），所以这里只报一条失败，
		// 不假装知道是哪个文件出的错。
		return []ui.ApplyItem{{SpecID: s.ID, Target: "file", Result: "failed", Message: err.Error()}}
	}
	items := make([]ui.ApplyItem, 0, len(files))
	for _, st := range files {
		items = append(items, ui.ApplyItem{SpecID: s.ID, Target: st.Target, Result: "ok",
			Message: "已写入配置（原文件备份为 *.bak-<unix>）" + restartNote(s)})
	}
	return items
}

// target 解析本次要操作的适配器，并确定网关地址。
func (b *uiBackend) target(specID string) (string, spec.Spec, error) {
	base, err := b.baseURL()
	if err != nil {
		return "", spec.Spec{}, err
	}
	id := strings.TrimSpace(specID)
	if id == "" {
		return "", spec.Spec{}, ui.Errorf("bad_request", "缺少 spec_id")
	}
	specs, err := loadSpecsFn(b.app.home)
	if err != nil {
		return "", spec.Spec{}, ui.Errorf("specs_failed", "加载适配器规格失败: %v", err)
	}
	s, ok := engine.FindSpec(specs, id)
	if !ok {
		return "", spec.Spec{}, ui.Errorf("spec_not_found", "没有 id=%q 的适配器（可用：%s）", id, strings.Join(spec.IDs(specs), ", "))
	}
	return base, s, nil
}

// resolveKey 决定本次要写进 Agent 配置的密钥：请求里给的优先，否则用本机凭据。
// 第二个返回值表示这是登录得到的短期 access token（约 15 分钟寿命）——调用方必须
// 按 CLI 的默认策略处理（写盘一律拒绝），UI 不提供 --allow-short-lived-token 一类开关。
func (b *uiBackend) resolveKey(ctx context.Context, base, fromRequest string) (string, bool, error) {
	key := strings.TrimSpace(fromRequest)
	if key != "" {
		return key, strings.HasPrefix(key, accessTokenPrefix), nil
	}
	cred, err := newGatewayClient(base, b.app.home).EnsureCred(ctx)
	if err != nil {
		return "", false, ui.Errorf("no_credential", "%v；也可直接粘贴长期密钥", err)
	}
	return cred.Bearer(), cred.APIKey == "" && cred.AccessToken != "", nil
}

// pickModel 决定写进配置的模型：入参优先，其次从网关挑一个；规格需要模型却挑不到
// 时报错（而不是让 engine 抛一句难懂的「字段需要 --model」）。
func (b *uiBackend) pickModel(ctx context.Context, base, key, want string, s spec.Spec) (string, error) {
	if m := strings.TrimSpace(want); m != "" {
		return m, nil
	}
	if !specsNeedModel([]spec.Spec{s}) {
		return "", nil
	}
	if picked, err := b.app.pickModel(ctx, base, key); err == nil {
		return picked, nil
	}
	return "", ui.Errorf("model_required", "规格 %s 需要模型：请在界面上先选一个模型（GET /api/models）", s.ID)
}

// dryRunDiff 用 engine 既有的 dry-run 路径生成差异（含两层密钥打码），不落盘。
// engine.Out 与 engine.MaskSecretsInDryRun 是包级状态，调用方必须持有 b.mu。
// 打码是强制的：UI 没有 --show-secrets 的等价开关（契约 §2.4）。
func (b *uiBackend) dryRunDiff(steps []engine.PlanStep) string {
	var buf bytes.Buffer
	prevOut, prevMask := engine.Out, engine.MaskSecretsInDryRun
	engine.Out, engine.MaskSecretsInDryRun = &buf, true
	defer func() { engine.Out, engine.MaskSecretsInDryRun = prevOut, prevMask }()

	if err := engine.Apply(steps, true); err != nil {
		return "（生成差异失败：" + err.Error() + "）"
	}
	return buf.String()
}

func (b *uiBackend) Doctor(ctx context.Context) ([]ui.DoctorItem, error) {
	var items []ui.DoctorItem
	add := func(name, status, detail string) {
		items = append(items, ui.DoctorItem{Name: name, Status: status, Detail: detail})
	}

	// 体检项的判定逻辑与 CLI 的 doctor 一致：同一批既有的包与函数（这里不重写判定）。
	var specs []spec.Spec
	var detected []engine.Detection
	loaded, err := loadSpecsFn(b.app.home)
	switch {
	case err != nil:
		add("适配器规格", "fail", err.Error())
	case len(loaded) == 0:
		add("适配器规格", "fail", "没有加载到任何规格（内置库为空？）")
	default:
		specs = loaded
		detected = engine.Found(engine.DetectIn(specs, b.app.expand()))
		add("适配器规格", "pass", fmt.Sprintf("%d 个（home=%s）", len(specs), b.app.home))
		if len(detected) == 0 {
			add("本机 Agent 检测", "warn", "未检测到任何 Agent；仍可用 CLI 的 --spec 显式指定")
		} else {
			ids := make([]string, 0, len(detected))
			for _, d := range detected {
				ids = append(ids, d.SpecID)
			}
			add("本机 Agent 检测", "pass", fmt.Sprintf("%d 个：%s", len(detected), strings.Join(ids, ", ")))
		}
	}

	base, _ := b.baseURL()
	perm, perr := newGatewayClient(base, b.app.home).CredPerm()
	switch {
	case perr != nil:
		add("凭据文件", "warn", perr.Error())
	case !perm.Exists:
		add("凭据文件", "warn", fmt.Sprintf("%s 不存在（未登录；apply 需要在界面粘贴长期密钥）", perm.Path))
	case perm.Warning != "":
		add("凭据文件", "warn", perm.Warning)
	case perm.Secure:
		add("凭据文件", "pass", fmt.Sprintf("%s（%04o）", perm.Path, perm.Mode))
	default:
		add("凭据文件", "pass", fmt.Sprintf("%s（权限由平台决定）", perm.Path))
	}

	// 「配置里写的是长期密钥还是短期令牌」由 CLI 的同一个实现判定（env_doctor.go）。
	if it, ok := b.app.inspectConfigCredentials(specs, base); ok {
		items = append(items, ui.DoctorItem{Name: it.Name, Status: strings.ToLower(it.Status), Detail: it.Detail})
	}

	for _, d := range detected {
		if d.SpecID != ximoAgentSpecID {
			continue
		}
		endpoint := defaultIPCEndpoint()
		if endpoint == "" {
			add("ximo-agent IPC", "warn", "本平台不支持命名管道（P-AgentIPC 仅 Windows），将走配置文件回退路径")
			break
		}
		if perr := pingIPC(endpoint); perr != nil {
			add("ximo-agent IPC", "warn", fmt.Sprintf("%s 不可达（%v）；将走配置文件回退路径", endpoint, perr))
		} else {
			add("ximo-agent IPC", "pass", fmt.Sprintf("%s 可达（配置可即时生效）", endpoint))
		}
	}

	if base == "" {
		add("网关连通性", "warn", "未设置网关（用 --gateway 启动界面或先登录），已跳过")
		return items, nil
	}
	client := newGatewayClient(base, b.app.home)
	if h, herr := client.Health(ctx); herr != nil {
		add("网关健康", "fail", fmt.Sprintf("%s：%v", base, herr))
	} else {
		add("网关健康", "pass", fmt.Sprintf("%s status=%s version=%s", base, h.Status, h.Version))
	}
	if cred, cerr := client.EnsureCred(ctx); cerr != nil {
		add("网关模型", "warn", fmt.Sprintf("无可用凭据（%v），已跳过", cerr))
	} else if models, merr := client.ListModels(ctx, cred); merr != nil {
		add("网关模型", "fail", merr.Error())
	} else {
		add("网关模型", "pass", fmt.Sprintf("%d 个可用模型", len(models)))
	}
	return items, nil
}

func (b *uiBackend) Specs(context.Context) ([]spec.Spec, error) {
	specs, err := loadSpecsFn(b.app.home)
	if err != nil {
		return nil, ui.Errorf("specs_failed", "加载适配器规格失败: %v", err)
	}
	return specs, nil
}

// gatewayForMessage 只用于拼给用户看的命令行提示：优先用显式 --gateway。
func (b *uiBackend) gatewayForMessage() string {
	if b.gateway != "" {
		return b.gateway
	}
	return "<中转站地址>"
}

func restartNote(s spec.Spec) string {
	if strings.TrimSpace(s.RestartNote) == "" {
		return ""
	}
	return "；重启提示：" + s.RestartNote
}

var _ ui.Backend = (*uiBackend)(nil)
