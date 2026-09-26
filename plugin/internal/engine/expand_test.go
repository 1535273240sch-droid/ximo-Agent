package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// 本文件钉住路径展开的两条语义（都与 spec.ExpandPathOpts 一致）：
//
//  1. 引用未定义的环境变量是**错误**，不是空串：detect 证据里应给出原因，
//     绝不能出现 "/ximo-agent/config.json" 这种展开残渣。
//  2. --home 显式生效时（IsolateHome），%APPDATA% / %XIMO_HOME% 也按隔离
//     home 解析，一个字节都不落到真实用户目录。

func TestDetectPathEvidenceReportsExpansionError(t *testing.T) {
	home := t.TempDir()

	s := spec.Spec{ID: "strict", Name: "strict", Detect: spec.Detect{Paths: []string{
		"~/agent/config.json",
		"%XIMO_TEST_UNSET_FOR_EXPAND_9c1f%/config.json",
		"../../relative/path.json",
	}}}
	dets := Detect([]spec.Spec{s}, home)
	if len(dets) != 1 {
		t.Fatalf("想要 1 条检测结果，得到 %d", len(dets))
	}
	ev := dets[0].Evidence
	if ev[0].Resolved != filepath.Join(home, "agent", "config.json") {
		t.Errorf("~ 未按 home 展开：%+v", ev[0])
	}
	if ev[0].OK {
		t.Errorf("文件不存在时不该命中：%+v", ev[0])
	}
	for _, e := range ev[1:] {
		if e.Resolved != "" {
			t.Errorf("展开失败时不得给出残渣路径（宽松展开的老毛病）：%+v", e)
		}
		if !strings.Contains(e.Detail, "路径无法展开") {
			t.Errorf("展开失败应给出原因，得到 %+v", e)
		}
	}
	if dets[0].Found {
		t.Errorf("全部未命中时不该 Found：%+v", dets[0])
	}
}

func TestDetectInIsolatedHomeKeepsVarsUnderHome(t *testing.T) {
	home := t.TempDir()
	decoyAppData := filepath.Join(t.TempDir(), "decoy-appdata")
	decoyXimoHome := filepath.Join(t.TempDir(), "decoy-ximo-home")
	t.Setenv("APPDATA", decoyAppData)
	t.Setenv("XIMO_HOME", decoyXimoHome)

	s := spec.Spec{ID: "iso", Name: "iso", Detect: spec.Detect{Paths: []string{
		"%APPDATA%/ximo-agent/config.json",
		"%XIMO_HOME%/config.json",
	}}}

	isolated := DetectIn([]spec.Spec{s}, spec.ExpandOptions{Home: home, IsolateHome: true})[0].Evidence
	for _, e := range isolated {
		if !strings.HasPrefix(e.Resolved, filepath.Clean(home)) {
			t.Errorf("隔离 home 下 %q 解析到 %q，逃出了 %q", e.Need, e.Resolved, home)
		}
		if strings.Contains(e.Resolved, "decoy") {
			t.Errorf("隔离 home 下仍指向真实环境变量给出的位置：%+v", e)
		}
	}
	// 隔离后 %APPDATA%/<agent>/config.json 与 %XIMO_HOME%/config.json 必须指向同一处
	// （XIMO_HOME 是 agent 自己的 BaseDir 开关，隔离后落在隔离 home 下的同一位置）。
	if isolated[0].Resolved != isolated[1].Resolved {
		t.Errorf("隔离后两个变量应指向同一配置路径：%q vs %q", isolated[0].Resolved, isolated[1].Resolved)
	}

	// 反向控制：没显式给 --home 时必须尊重真实环境里的 %XIMO_HOME% / %APPDATA%。
	plain := DetectIn([]spec.Spec{s}, spec.ExpandOptions{Home: home})[0].Evidence
	if plain[0].Resolved != filepath.Join(decoyAppData, "ximo-agent", "config.json") {
		t.Errorf("未隔离时 %%APPDATA%% 应按真实环境解析，得到 %q", plain[0].Resolved)
	}
	if plain[1].Resolved != filepath.Join(decoyXimoHome, "config.json") {
		t.Errorf("未隔离时 %%XIMO_HOME%% 应按真实环境解析，得到 %q", plain[1].Resolved)
	}
}

func TestBuildPlanPathFollowsIsolatedHome(t *testing.T) {
	home := t.TempDir()
	decoy := filepath.Join(t.TempDir(), "decoy-appdata")
	t.Setenv("APPDATA", decoy)

	target := "%APPDATA%/ximo-agent/config.json"
	s := spec.Spec{ID: "iso", Files: []spec.Target{{
		Path: target, Format: "json", Create: true,
		Fields: []spec.Field{{Path: "provider.base_url", From: "base_url"}},
	}}}

	in := Inputs{GatewayURL: "http://gw:8600", Home: home, IsolateHome: true}
	steps, _, err := BuildPlan(s, in)
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	want := filepath.Join(home, "AppData", "Roaming", "ximo-agent", "config.json")
	if steps[0].Target != want {
		t.Errorf("隔离 home 下目标路径 = %q，想要 %q", steps[0].Target, want)
	}

	in.IsolateHome = false
	steps, _, err = BuildPlan(s, in)
	if err != nil {
		t.Fatalf("BuildPlan（未隔离）失败: %v", err)
	}
	if steps[0].Target != filepath.Join(decoy, "ximo-agent", "config.json") {
		t.Errorf("未隔离时目标路径 = %q，应按真实 APPDATA 解析", steps[0].Target)
	}

	// 未定义变量必须是错误（而不是静默展开成 "/config.json" 后去创建它）。
	bad := spec.Spec{ID: "bad", Files: []spec.Target{{
		Path: "%XIMO_TEST_UNSET_FOR_EXPAND_9c1f%/config.json", Format: "json", Create: true,
		Fields: []spec.Field{{Path: "provider.base_url", From: "base_url"}},
	}}}
	if _, _, err := BuildPlan(bad, in); err == nil || !strings.Contains(err.Error(), "无法展开") {
		t.Fatalf("未定义变量应报错，得到 %v", err)
	}
}
