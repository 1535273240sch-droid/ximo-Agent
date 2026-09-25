package computeruse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// ============================== 默认关闭（最高风险能力 fail-closed） ==============================

// TestDisabledByDefault 验证 ComputerUse 默认关闭，未开启时所有调用被策略拒绝。
func TestDisabledByDefault(t *testing.T) {
	w := NewWorker("cu-0", Config{}) // Enabled 默认 false
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	args, _ := json.Marshal(args{})
	resp, _ := w.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionScreenshot, Args: args,
	})
	if resp.Error == nil || resp.Error.Code != worker.CodePolicyDenied {
		t.Fatalf("未启用时应返回 policy_denied，实际 %+v", resp.Error)
	}
	// 未启用不应被视为故障（避免 Manager 反复重启）。
	if err := w.Health(context.Background()); err != nil {
		t.Errorf("未启用是策略状态而非故障: %v", err)
	}
}

// ============================== 窗口白/黑名单（v1 完全缺失的权限维度） ==============================

// TestWindowDenyList 验证拒绝列表生效。
func TestWindowDenyList(t *testing.T) {
	w := NewWorker("cu-0", Config{
		Enabled:       true,
		DeniedWindows: []string{"*银行*", "KeePass*"},
	})
	if err := w.checkWindow("中国银行 - 登录"); err == nil {
		t.Error("命中拒绝列表的窗口应被拒绝")
	}
	if err := w.checkWindow("KeePass 2.x"); err == nil {
		t.Error("通配命中拒绝列表应被拒绝")
	}
	if err := w.checkWindow("Visual Studio Code"); err != nil {
		t.Errorf("未命中拒绝列表应放行: %v", err)
	}
}

// TestWindowAllowList 验证白名单模式生效。
func TestWindowAllowList(t *testing.T) {
	w := NewWorker("cu-0", Config{
		Enabled:        true,
		AllowedWindows: []string{"Notepad*", "计算器"},
	})
	if err := w.checkWindow("无标题 - 记事本"); err != nil {
		t.Logf("中文标题与英文模式不匹配属预期: %v", err)
	}
	if err := w.checkWindow("Notepad - file.txt"); err != nil {
		t.Errorf("白名单内窗口应放行: %v", err)
	}
	if err := w.checkWindow("Chrome"); err == nil {
		t.Error("白名单外窗口应被拒绝")
	}
}

// TestGlobalInputRequiresExplicitOptIn 验证白名单模式下全局键鼠操作需显式放行。
func TestGlobalInputRequiresExplicitOptIn(t *testing.T) {
	w := NewWorker("cu-0", Config{
		Enabled:        true,
		AllowedWindows: []string{"Notepad"},
	})
	// 不带 window 的全局键鼠操作在白名单模式下应被拒绝。
	if err := w.checkWindow(""); err == nil {
		t.Error("白名单模式下全局键鼠操作应被拒绝（防误操作整个桌面）")
	}
	// 显式放行 "*" 后才可全局操作。
	w2 := NewWorker("cu-1", Config{
		Enabled:        true,
		AllowedWindows: []string{"*"},
	})
	if err := w2.checkWindow(""); err != nil {
		t.Errorf("显式 \"*\" 时应允许全局操作: %v", err)
	}
}

// TestNoAllowListPermitsAll 验证未配置白名单时不限制窗口（由 Enabled 开关控制）。
func TestNoAllowListPermitsAll(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true})
	if err := w.checkWindow("任意窗口"); err != nil {
		t.Errorf("未配置白名单时不应限制窗口: %v", err)
	}
}

// ============================== 动作映射（对齐 v1 的 helper 协议） ==============================

// TestTranslateActionsMatchV1 验证所有动作映射到 v1 的底层 helper 命令。
func TestTranslateActionsMatchV1(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true, MaxDimension: 1280})

	cases := []struct {
		action     string
		args       args
		wantCmd    string
		wantAction string // helper 的 action 参数（若有）
	}{
		{ActionScreenshot, args{}, "look", ""},
		{ActionObserve, args{Window: "w"}, "look", ""},
		{ActionFindWindow, args{Window: "w"}, "listRoots", ""},
		{ActionClickElement, args{Ref: "@r1"}, "act", "press"},
		{ActionSetText, args{Ref: "@r1", Text: "hi"}, "act", "setText"},
		{ActionReadText, args{Ref: "@r1"}, "uiaReadText", ""},
		{ActionMouseClick, args{X: 10, Y: 20}, "act", "click"},
		{ActionMouseMove, args{X: 10, Y: 20}, "act", "moveMouse"},
		{ActionMouseDrag, args{Path: []point{{X: 1, Y: 1}, {X: 2, Y: 2}}}, "act", "drag"},
		{ActionMouseScroll, args{X: 1, Y: 1, ScrollY: 5}, "act", "scroll"},
		{ActionKeyPress, args{Keys: []string{"ctrl", "c"}}, "act", "keypress"},
		{ActionKeyType, args{Text: "abc"}, "act", "typeText"},
		{ActionWait, args{Text: "Ready", Until: "present"}, "uiaWaitFor", ""},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			cmd, cmdArgs, err := w.translate(tc.action, tc.args)
			if err != nil {
				t.Fatalf("translate 失败: %v", err)
			}
			if cmd != tc.wantCmd {
				t.Errorf("命令: got=%q want=%q", cmd, tc.wantCmd)
			}
			if tc.wantAction != "" {
				if got, _ := cmdArgs["action"].(string); got != tc.wantAction {
					t.Errorf("action 参数: got=%q want=%q", got, tc.wantAction)
				}
			}
		})
	}
}

// TestTranslateRequiresRef 验证需要 ref 的动作在缺失时报错。
func TestTranslateRequiresRef(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true})
	for _, action := range []string{ActionClickElement, ActionSetText, ActionReadText} {
		if _, _, err := w.translate(action, args{}); err == nil {
			t.Errorf("%s 缺少 ref 应报错", action)
		}
	}
}

// TestTranslateMouseDragRequiresPath 验证拖拽需要至少两个路径点。
func TestTranslateMouseDragRequiresPath(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true})
	if _, _, err := w.translate(ActionMouseDrag, args{Path: []point{{X: 1, Y: 1}}}); err == nil {
		t.Error("拖拽路径点不足应报错")
	}
}

// TestTranslateKeyPressRequiresKeys 验证按键需要 keys。
func TestTranslateKeyPressRequiresKeys(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true})
	if _, _, err := w.translate(ActionKeyPress, args{}); err == nil {
		t.Error("key_press 缺少 keys 应报错")
	}
}

// TestTranslateUnknownAction 验证未知动作报错。
func TestTranslateUnknownAction(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true})
	if _, _, err := w.translate("teleport", args{}); err == nil {
		t.Error("未知动作应报错")
	}
}

// TestScreenshotUsesConfiguredMaxDimension 验证截图尺寸上限来自配置。
func TestScreenshotUsesConfiguredMaxDimension(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true, MaxDimension: 800})
	_, cmdArgs, err := w.translate(ActionScreenshot, args{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := cmdArgs["maxDimension"].(int); got != 800 {
		t.Errorf("应使用配置的 maxDimension=800，实际 %v", cmdArgs["maxDimension"])
	}
	// 请求级覆盖优先。
	_, cmdArgs2, _ := w.translate(ActionScreenshot, args{MaxDimension: 320})
	if got, _ := cmdArgs2["maxDimension"].(int); got != 320 {
		t.Errorf("请求级 maxDimension 应优先，实际 %v", cmdArgs2["maxDimension"])
	}
}

// TestWaitDefaultTimeout 验证 wait 的默认超时（对齐 v1）。
func TestWaitDefaultTimeout(t *testing.T) {
	w := NewWorker("cu-0", Config{Enabled: true})
	_, cmdArgs, err := w.translate(ActionWait, args{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := cmdArgs["timeoutMs"].(int64); got != 10000 {
		t.Errorf("wait 默认超时应为 10000ms，实际 %v", cmdArgs["timeoutMs"])
	}
	if got, _ := cmdArgs["until"].(string); got != "present" {
		t.Errorf("wait 默认 until 应为 present，实际 %v", cmdArgs["until"])
	}
}

// ============================== helper 探测 ==============================

// TestHelperNotFoundGivesActionableError 验证找不到 helper 时错误可操作。
func TestHelperNotFoundGivesActionableError(t *testing.T) {
	w := NewWorker("cu-0", Config{
		Enabled:     true,
		SearchPaths: []string{t.TempDir()},
		HelperPath:  "",
	})
	_, err := w.resolveHelper()
	if err == nil {
		t.Skip("运行环境恰好存在 pi-helper，跳过")
	}
	if !strings.Contains(err.Error(), "helper_path") {
		t.Errorf("错误信息应提示配置项，实际: %v", err)
	}
}

// TestHelperResolvedFromSearchPath 验证能从候选目录找到 helper。
func TestHelperResolvedFromSearchPath(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, helperName())
	if err := os.WriteFile(helper, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := NewWorker("cu-0", Config{Enabled: true, SearchPaths: []string{dir}})
	got, err := w.resolveHelper()
	if err != nil {
		t.Fatalf("应能从 search_paths 解析: %v", err)
	}
	if got != helper {
		t.Errorf("got=%s want=%s", got, helper)
	}
}

// TestHelperNamesByPlatform 验证平台相关的 helper 文件名与目录。
func TestHelperNamesByPlatform(t *testing.T) {
	name := helperName()
	dir := platformDir()
	if runtime.GOOS == "windows" {
		if name != "windows-bridge.exe" {
			t.Errorf("Windows helper 名应为 windows-bridge.exe，实际 %s", name)
		}
		if dir != "win-x64" {
			t.Errorf("Windows 目录应为 win-x64，实际 %s", dir)
		}
	}
	if name == "" || dir == "" {
		t.Error("helper 名与目录不应为空")
	}
}

// ============================== 协议常量 ==============================

// TestProtocolVersionMatchesV1 验证协议版本与 v1 一致（4）。
func TestProtocolVersionMatchesV1(t *testing.T) {
	if ProtocolVersion != 4 {
		t.Errorf("协议版本应与 v1 的 PROTOCOL_VERSION=4 一致，实际 %d", ProtocolVersion)
	}
}

// TestHelperRequestWireFormat 验证请求消息字段与 v1 的 IPC 约定一致。
func TestHelperRequestWireFormat(t *testing.T) {
	req := helperRequest{
		ProtocolVersion: ProtocolVersion,
		ID:              "req-1",
		Cmd:             "look",
		Args:            map[string]any{"readText": "auto"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	_ = json.Unmarshal(raw, &probe)
	for _, field := range []string{"protocolVersion", "id", "cmd", "args"} {
		if _, ok := probe[field]; !ok {
			t.Errorf("请求消息应含字段 %q（与 v1 约定一致）", field)
		}
	}
}

// TestHelperResponseParsing 验证响应解析（含错误字段）。
func TestHelperResponseParsing(t *testing.T) {
	okLine := `{"protocolVersion":4,"id":"r1","ok":true,"result":{"title":"x"}}`
	var resp helperResponse
	if err := json.Unmarshal([]byte(okLine), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "r1" {
		t.Errorf("成功响应解析错误: %+v", resp)
	}

	errLine := `{"protocolVersion":4,"id":"r2","ok":false,"error":{"message":"failed","code":"E_X"}}`
	var resp2 helperResponse
	if err := json.Unmarshal([]byte(errLine), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.Error == nil || resp2.Error.Code != "E_X" {
		t.Errorf("错误响应解析错误: %+v", resp2)
	}
}

// TestMatchPattern 验证窗口标题匹配逻辑（含通配与前缀）。
func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern string
		title   string
		want    bool
	}{
		{"*", "anything", true},
		{"Notepad", "Notepad - file", true},
		{"Notepad*", "Notepad - file", true},
		{"*file*", "A file B", true},
		{"Chrome", "Firefox", false},
		{"*secret*", "my SECRET doc", true}, // 大小写不敏感
	}
	for _, tc := range cases {
		if got := matchPattern(tc.pattern, tc.title); got != tc.want {
			t.Errorf("matchPattern(%q,%q)=%v want=%v", tc.pattern, tc.title, got, tc.want)
		}
	}
}

// TestKindAndID 验证身份方法。
func TestKindAndID(t *testing.T) {
	w := NewWorker("cu-9", Config{})
	if w.ID() != "cu-9" || w.Kind() != "computer-use" {
		t.Errorf("身份错误: %s/%s", w.ID(), w.Kind())
	}
}

// TestStopIsIdempotent 验证停机幂等。
func TestStopIsIdempotent(t *testing.T) {
	w := NewWorker("cu-0", Config{})
	_ = w.Start(context.Background())
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Errorf("重复 Stop 应幂等: %v", err)
	}
}
