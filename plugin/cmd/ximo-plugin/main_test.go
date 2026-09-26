package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1535273240sch-droid/ximo-plugin/internal/gateway"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// —— 测试脚手架 ——

func useSpecs(t *testing.T, specs []spec.Spec) {
	t.Helper()
	old := loadSpecsFn
	loadSpecsFn = func(string) ([]spec.Spec, error) { return specs, nil }
	t.Cleanup(func() { loadSpecsFn = old })
}

func setTTY(t *testing.T, v bool) {
	t.Helper()
	old := isTTY
	isTTY = func(any) bool { return v }
	t.Cleanup(func() { isTTY = old })
}

func run(args []string, stdin string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// synthSpec 造一个"环境变量即可检测 + 写一个 JSON 文件"的适配器。
func synthSpec(id, target string, create bool) spec.Spec {
	return spec.Spec{
		ID:          id,
		Name:        "Synth " + id,
		Description: "单测用规格",
		RestartNote: "单测用：无",
		Detect:      spec.Detect{Env: []string{"XIMO_TEST_" + strings.ToUpper(id)}},
		Env:         spec.EnvSpec{BaseURL: "SYNTH_BASE_URL", APIKey: "SYNTH_API_KEY", Style: "export"},
		Files: []spec.Target{{
			Path: target, Format: "json", Create: create,
			Fields: []spec.Field{
				{Path: "provider.base_url", From: "base_url"},
				{Path: "provider.api_key", From: "api_key"},
				{Path: "models.0.id", From: "model"},
			},
		}},
		Protocols: []string{"openai-chat"},
	}
}

func mustEnv(t *testing.T, kv ...string) {
	t.Helper()
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

// —— 退出码语义 ——

func TestExitCodes(t *testing.T) {
	useSpecs(t, nil)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"无参数", nil, exitUsage},
		{"未知命令", []string{"frobnicate"}, exitUsage},
		{"apply 缺 gateway", []string{"apply"}, exitUsage},
		{"apply 缺 spec/all", []string{"apply", "--gateway", "http://g:1"}, exitUsage},
		{"apply 同时给 spec 与 all", []string{"apply", "--gateway", "http://g:1", "--spec", "x", "--all"}, exitUsage},
		{"未知 flag", []string{"detect", "--bogus"}, exitUsage},
		{"--home 缺值", []string{"--home"}, exitUsage},
		{"--version", []string{"--version"}, exitOK},
		{"-h", []string{"-h"}, exitOK},
		{"adapters show 缺 id", []string{"adapters", "show"}, exitUsage},
		{"usage 缺 gateway", []string{"usage"}, exitUsage},
		{"login 缺 gateway", []string{"login"}, exitUsage},
		{"print-env 缺 gateway", []string{"print-env"}, exitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, _, _ := run(c.args, ""); code != c.want {
				t.Fatalf("%v：退出码 %d，想要 %d", c.args, code, c.want)
			}
		})
	}
	// 零规格时 doctor 报 FAIL（没有可用适配器是真实问题），退出码 1
	if code, _, _ := run([]string{"doctor"}, ""); code != exitFail {
		t.Errorf("零规格时 doctor 应退出 1，得到 %d", code)
	}
}

// —— detect：没有任何 Agent / 没有任何规格也不能 panic ——

func TestDetectWithoutAgents(t *testing.T) {
	home := t.TempDir()
	useSpecs(t, []spec.Spec{synthSpec("nohit", filepath.Join(home, "x.json"), true)})

	code, out, errb := run([]string{"--home", home, "detect"}, "")
	if code != exitOK {
		t.Fatalf("退出码 %d，想要 0；stderr=%s", code, errb)
	}
	if !strings.Contains(out, "未检测到任何 Agent") {
		t.Errorf("应给出清晰提示，实际输出：%s", out)
	}
	code, out, _ = run([]string{"--home", home, "--json", "detect"}, "")
	if code != exitOK {
		t.Fatalf("--json 退出码 %d", code)
	}
	var payload struct {
		Detected []any `json:"detected"`
		All      []any `json:"all"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json 输出不是合法 JSON：%v\n%s", err, out)
	}
	if len(payload.Detected) != 0 || len(payload.All) != 1 {
		t.Errorf("JSON 内容不对：%s", out)
	}

	useSpecs(t, []spec.Spec{})
	if code, _, _ := run([]string{"--home", home, "detect"}, ""); code != exitOK {
		t.Fatalf("零规格时退出码 %d，想要 0", code)
	}
}

// —— dry-run 不落盘、密钥打码 ——

func TestApplyDryRunDoesNotWriteAndMasksKey(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "cfg.json")
	original := "{\n  \"keep\": {\"x\": 1}\n}\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	mustEnv(t, "XIMO_TEST_SYNTH", "1")
	useSpecs(t, []spec.Spec{synthSpec("synth", target, true)})

	code, out, errb := run([]string{"--home", home, "apply", "--spec", "synth",
		"--gateway", "http://gw:8600", "--api-key", "sk-test-secret", "--model", "m1", "--dry-run"}, "")
	if code != exitOK {
		t.Fatalf("dry-run 退出码 %d，想要 0；stderr=%s", code, errb)
	}
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Errorf("dry-run 不得改文件：\n%s", got)
	}
	if baks, _ := filepath.Glob(target + ".bak-*"); len(baks) != 0 {
		t.Errorf("dry-run 不得产生备份：%v", baks)
	}
	if !strings.Contains(out, "dry-run") || !strings.Contains(out, "provider") {
		t.Errorf("dry-run 应打印差异：%s", out)
	}
	combined := out + errb
	if strings.Contains(combined, "sk-test-secret") {
		t.Errorf("dry-run 输出不得含明文密钥：%s", combined)
	}
	if !strings.Contains(combined, "sk-t") {
		t.Errorf("差异里应保留打码后的密钥：%s", combined)
	}

	// --json 时 stdout 必须只有 JSON（差异走 stderr），否则脚本无法解析
	code, out, errb = run([]string{"--home", home, "--json", "apply", "--spec", "synth",
		"--gateway", "http://gw:8600", "--api-key", "sk-test-secret", "--model", "m1", "--dry-run"}, "")
	if code != exitOK {
		t.Fatalf("--json dry-run 退出码 %d；stderr=%s", code, errb)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json 下 stdout 必须是纯 JSON（差异不能混进来）：%v\n%s", err, out)
	}
	if !strings.Contains(errb, "dry-run") {
		t.Errorf("--json 下差异应走 stderr：%s", errb)
	}
}

// —— 真写：备份 + 未知字段保留 ——

func TestApplyYesWritesAndBacksUp(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "cfg.json")
	if err := os.WriteFile(target, []byte("{\n  \"keep\": 1\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustEnv(t, "XIMO_TEST_SYNTH", "1")
	useSpecs(t, []spec.Spec{synthSpec("synth", target, true)})
	setTTY(t, false)

	applyWith := func(model string) (int, string) {
		code, _, errb := run([]string{"--home", home, "apply", "--spec", "synth", "--gateway", "http://gw:8600",
			"--api-key", "sk-test-secret", "--model", model, "--yes"}, "")
		return code, errb
	}
	if code, errb := applyWith("m1"); code != exitOK {
		t.Fatalf("退出码 %d，想要 0；stderr=%s", code, errb)
	}
	body1, _ := os.ReadFile(target)
	for _, want := range []string{"sk-test-secret", "http://gw:8600/v1", "m1", "keep"} {
		if !strings.Contains(string(body1), want) {
			t.Errorf("写入内容缺少 %q：%s", want, body1)
		}
	}
	if baks, _ := filepath.Glob(target + ".bak-*"); len(baks) == 0 {
		t.Fatal("第一次写入应生成备份 <path>.bak-<unix>")
	}

	// 第二次换模型 → 内容确实变化 → 备份必须等于本次写前的内容（= 第一次的结果）
	if code, errb := applyWith("m2"); code != exitOK {
		t.Fatalf("第二次写入退出码 %d；stderr=%s", code, errb)
	}
	body2, _ := os.ReadFile(target)
	if !strings.Contains(string(body2), "m2") {
		t.Errorf("第二次写入未生效：%s", body2)
	}
	baks2, _ := filepath.Glob(target + ".bak-*")
	bak, err := os.ReadFile(baks2[len(baks2)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bak, body1) {
		t.Errorf("备份内容应是本次写前的内容：\n想要 %s\n得到 %s", body1, bak)
	}

	// 第三次参数完全相同 → 内容无变化 → 不写盘、也不新增备份槽（P-Formats 的语义）
	if code, errb := applyWith("m2"); code != exitOK {
		t.Fatalf("第三次退出码 %d；stderr=%s", code, errb)
	}
	body3, _ := os.ReadFile(target)
	if !bytes.Equal(body3, body2) {
		t.Errorf("无变化时文件不应被改写：%s", body3)
	}
	if baks3, _ := filepath.Glob(target + ".bak-*"); len(baks3) != len(baks2) {
		t.Errorf("无变化的重复 apply 不应新增备份：%v -> %v", baks2, baks3)
	}
}

// —— 交互确认：非 tty 不等待；交互时 yes/no 都要正确 ——

func TestApplyConfirmBehaviour(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "cfg.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustEnv(t, "XIMO_TEST_SYNTH", "1")
	useSpecs(t, []spec.Spec{synthSpec("synth", target, true)})
	base := []string{"--home", home, "apply", "--spec", "synth", "--gateway", "http://gw:8600",
		"--api-key", "sk-test-secret", "--model", "m1"}

	t.Run("非交互不等待并要求 --yes", func(t *testing.T) {
		setTTY(t, false)
		code, _, errb := run(base, "")
		if code != exitFail {
			t.Fatalf("退出码 %d，想要 1", code)
		}
		if !strings.Contains(errb, "--yes") {
			t.Errorf("应提示加 --yes，stderr=%s", errb)
		}
		if got, _ := os.ReadFile(target); string(got) != "{}\n" {
			t.Errorf("拒绝时不得改文件：%s", got)
		}
	})

	t.Run("交互输入 no 视为拒绝", func(t *testing.T) {
		setTTY(t, true)
		code, out, _ := run(base, "no\n")
		if code != exitFail {
			t.Fatalf("退出码 %d，想要 1", code)
		}
		if !strings.Contains(out, "已取消") {
			t.Errorf("应说明已取消：%s", out)
		}
		if got, _ := os.ReadFile(target); string(got) != "{}\n" {
			t.Errorf("拒绝时不得改文件：%s", got)
		}
	})

	t.Run("交互输入 yes 才写", func(t *testing.T) {
		setTTY(t, true)
		code, _, errb := run(base, "yes\n")
		if code != exitOK {
			t.Fatalf("退出码 %d，想要 0；stderr=%s", code, errb)
		}
		if got, _ := os.ReadFile(target); !strings.Contains(string(got), "m1") {
			t.Errorf("确认后应写入：%s", got)
		}
	})
}

// —— apply --all：单个失败不中断其余，最后汇总 PASS/FAIL ——

func TestApplyAllPartialFailure(t *testing.T) {
	home := t.TempDir()
	good := filepath.Join(home, "good.json")
	bad := filepath.Join(home, "missing", "bad.json") // 不存在且 create=false
	if err := os.WriteFile(good, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustEnv(t, "XIMO_TEST_GOOD", "1", "XIMO_TEST_BAD", "1")
	useSpecs(t, []spec.Spec{synthSpec("bad", bad, false), synthSpec("good", good, true)})
	setTTY(t, false)

	code, out, errb := run([]string{"--home", home, "apply", "--all", "--gateway", "http://gw:8600",
		"--api-key", "sk-test-secret", "--model", "m1", "--yes"}, "")
	if code != exitFail {
		t.Fatalf("有失败时退出码应为 1，得到 %d", code)
	}
	combined := out + errb
	if !strings.Contains(combined, "汇总：PASS 1 / FAIL 1") {
		t.Errorf("应汇总 PASS 1 / FAIL 1：%s", combined)
	}
	if !strings.Contains(combined, "bad") {
		t.Errorf("失败项应出现在输出里：%s", combined)
	}
	if got, _ := os.ReadFile(good); !strings.Contains(string(got), "m1") {
		t.Errorf("失败项不应中断其余写入：%s", got)
	}
	if _, err := os.Stat(bad); err == nil {
		t.Error("失败项不应被创建")
	}

	// 全部成功时退出码 0
	if code, _, errb := run([]string{"--home", home, "apply", "--spec", "good", "--gateway", "http://gw:8600",
		"--api-key", "sk-test-secret", "--model", "m1", "--yes"}, ""); code != exitOK {
		t.Fatalf("单适配器成功应退出 0，得到 %d；stderr=%s", code, errb)
	}
}

// —— print-env ——

func TestPrintEnv(t *testing.T) {
	home := t.TempDir()
	useSpecs(t, []spec.Spec{{
		ID: "env-openai", Name: "Env", Description: "d", RestartNote: "r",
		Detect: spec.Detect{Env: []string{"OPENAI_API_KEY"}},
		Env:    spec.EnvSpec{BaseURL: "OPENAI_BASE_URL", APIKey: "OPENAI_API_KEY", Style: "export"},
	}})
	code, out, errb := run([]string{"--home", home, "print-env", "--gateway", "http://gw:8600",
		"--api-key", "sk-test-secret", "--model", "m1"}, "")
	if code != exitOK {
		t.Fatalf("退出码 %d；stderr=%s", code, errb)
	}
	if !strings.Contains(out, "export OPENAI_BASE_URL='http://gw:8600/v1'") || !strings.Contains(out, "OPENAI_API_KEY='sk-test-secret'") {
		t.Errorf("print-env 输出不对：%s", out)
	}
	if code, _, _ := run([]string{"--home", home, "print-env", "--gateway", "http://gw:8600", "--style", "cmd"}, ""); code != exitUsage {
		t.Errorf("未知 --style 应退出 2，得到 %d", code)
	}
	code, out, _ = run([]string{"--home", home, "--json", "print-env", "--gateway", "http://gw:8600", "--api-key", "k"}, "")
	if code != exitOK || !strings.Contains(out, "\"lines\"") {
		t.Errorf("--json 输出不对（code=%d）：%s", code, out)
	}
}

// useRealGatewayClient 用真 *gateway.Client（P-Client）替换注入点，并缩短设备轮询间隔。
func useRealGatewayClient(t *testing.T) {
	t.Helper()
	old := newGatewayClient
	newGatewayClient = func(baseURL, home string) gatewayAPI {
		c := gateway.NewClient(baseURL, gateway.CredPathIn(home))
		c.PollInterval = 50 * time.Millisecond
		return c
	}
	t.Cleanup(func() { newGatewayClient = old })
}

// —— 网关相关命令：用真 *gateway.Client 打 httptest 假网关 ——

func fakeGateway(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"bad key","type":"auth_error","code":"invalid_api_key"}}`)
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","version":"test-1.0","time_ms":1}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"m1","display_name":"Model One","provider":"up1","protocols":["openai-chat"],"capabilities":{"stream":true},"enabled":true}]}`)
	})
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"data":[{"request_id":"req-1","model_id":"m1","provider_id":"up1","status":"settled","input_tokens":3,"output_tokens":4,"latency_ms":12,"cost_micro":1500000,"created_at":1758880000000}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	useRealGatewayClient(t)
	return srv
}

func TestModelsAndUsage(t *testing.T) {
	home := t.TempDir()
	useSpecs(t, nil)
	srv := fakeGateway(t, "sk-test-secret")

	code, out, errb := run([]string{"--home", home, "models", "--gateway", srv.URL, "--api-key", "sk-test-secret"}, "")
	if code != exitOK {
		t.Fatalf("models 退出码 %d；stderr=%s", code, errb)
	}
	if !strings.Contains(out, "m1") || !strings.Contains(out, "Model One") {
		t.Errorf("models 输出不对：%s", out)
	}

	code, out, errb = run([]string{"--home", home, "usage", "--gateway", srv.URL, "--api-key", "sk-test-secret", "--limit", "5"}, "")
	if code != exitOK {
		t.Fatalf("usage 退出码 %d；stderr=%s", code, errb)
	}
	for _, want := range []string{"req-1", "m1", "settled", "1.500000"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage 输出缺少 %q：%s", want, out)
		}
	}

	// 凭据被拒绝 → 运行失败（1），不是用法错误（2）
	if code, _, _ := run([]string{"--home", home, "models", "--gateway", srv.URL, "--api-key", "wrong"}, ""); code != exitFail {
		t.Errorf("401 应退出 1，得到 %d", code)
	}
	// 无凭据且未 login → 1
	if code, _, _ := run([]string{"--home", home, "models", "--gateway", srv.URL}, ""); code != exitFail {
		t.Errorf("无凭据应退出 1，得到 %d", code)
	}
	// --limit 非法 → 2
	if code, _, _ := run([]string{"--home", home, "usage", "--gateway", srv.URL, "--limit", "0"}, ""); code != exitUsage {
		t.Errorf("--limit 0 应退出 2，得到 %d", code)
	}
}

func TestLoginDeviceFlow(t *testing.T) {
	home := t.TempDir()
	useSpecs(t, nil)
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device":
			fmt.Fprint(w, `{"device_code":"gwd_test","user_code":"WXYZ1234","expires_in":600,"interval":1,"verification_uri":"http://gw.test/activate"}`)
		case "/v1/auth/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"message":"pending","type":"invalid_request_error","code":"authorization_pending"}}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"gwa_test","refresh_token":"gwr_test","expires_in":3600,"token_type":"Bearer"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	useRealGatewayClient(t)
	code, out, errb := run([]string{"--home", home, "login", "--gateway", srv.URL, "--timeout", "20s"}, "")
	if code != exitOK {
		t.Fatalf("login 退出码 %d；stdout=%s stderr=%s", code, out, errb)
	}
	combined := out + errb
	if !strings.Contains(combined, "WXYZ1234") {
		t.Errorf("应把用户码展示给用户：%s", combined)
	}
	if strings.Contains(combined, "gwd_test") {
		t.Errorf("设备码是短期凭据，绝不能打印：%s", combined)
	}
	if _, err := os.Stat(credPath(home)); err != nil {
		t.Errorf("凭据应落盘到 %s：%v", credPath(home), err)
	}
}

// —— doctor ——

func TestDoctor(t *testing.T) {
	home := t.TempDir()
	useSpecs(t, []spec.Spec{synthSpec("synth", filepath.Join(home, "x.json"), true)})
	if code, out, _ := run([]string{"--home", home, "doctor"}, ""); code != exitOK {
		t.Fatalf("无 FAIL 时应退出 0，得到 %d\n%s", code, out)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if code, _, _ := run([]string{"--home", home, "doctor", "--gateway", bad.URL}, ""); code != exitFail {
		t.Fatalf("网关不健康应退出 1")
	}
	if code, _, _ := run([]string{"--home", home, "doctor", "--gateway", "not-a-url"}, ""); code != exitFail {
		t.Fatalf("非法网关地址应退出 1（不是 panic）")
	}
	code, out, _ := run([]string{"--home", home, "--json", "doctor"}, "")
	if code != exitOK || !strings.Contains(out, "\"items\"") {
		t.Fatalf("doctor --json 输出不对（code=%d）：%s", code, out)
	}
}

// —— 缺陷 3：没有长期密钥时不得把 access token 写进 Agent 配置 ——

// writeCredFile 造一份本机凭据文件（access token 形态，没有长期 API Key）。
func writeCredFile(t *testing.T, home, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(credPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"gateway":"http://gw:8600","access_token":%q,"refresh_token":"gwr_x","access_expires_at":4102444800}`+"\n", token)
	if err := os.WriteFile(credPath(home), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRefusesShortLivedToken(t *testing.T) {
	const token = "gwa_shortlived1234"
	home := t.TempDir()
	target := filepath.Join(home, "cfg.json")
	mustEnv(t, "XIMO_TEST_SYNTH", "1")
	useSpecs(t, []spec.Spec{synthSpec("synth", target, true)})
	setTTY(t, false)
	writeCredFile(t, home, token)

	base := []string{"--home", home, "apply", "--spec", "synth", "--gateway", "http://gw:8600", "--model", "m1", "--yes"}

	code, out, errb := run(base, "")
	combined := out + errb
	if code != exitFail {
		t.Fatalf("本机只有 access token 时默认应拒绝，得到退出码 %d：%s", code, combined)
	}
	for _, want := range []string{"access token", "15 分钟", "--api-key", "/admin/keys", "--allow-short-lived-token"} {
		if !strings.Contains(combined, want) {
			t.Errorf("拒绝理由应包含 %q：%s", want, combined)
		}
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("拒绝时不得创建/修改目标文件")
	}

	// --api-key 直接给 gwa_ 令牌同样被拦（换个目标，确认是拦在写之前）
	explicit := filepath.Join(home, "cfg2.json")
	useTarget := func(path string) { useSpecs(t, []spec.Spec{synthSpec("synth", path, true)}) }
	useTarget(explicit)
	if code, _, errb := run(append(base, "--api-key", token), ""); code != exitFail {
		t.Errorf("--api-key 传短期令牌也应拒绝，得到 %d：%s", code, errb)
	}
	if _, err := os.Stat(explicit); err == nil {
		t.Error("拒绝时不得创建目标文件（--api-key 形态）")
	}

	// dry-run 也不放行：要预览的是一个注定会坏的写入
	dryTarget := filepath.Join(home, "cfg-dry.json")
	useTarget(dryTarget)
	if code, _, _ := run([]string{"--home", home, "apply", "--spec", "synth", "--gateway", "http://gw:8600", "--model", "m1", "--dry-run"}, ""); code != exitFail {
		t.Errorf("dry-run 也应拒绝（加 --allow-short-lived-token 才能预览）")
	}
	if _, err := os.Stat(dryTarget); err == nil {
		t.Error("dry-run 拒绝时更不得创建文件")
	}

	// 显式放行才写，且必须有醒目告警
	useTarget(target)
	code, out, errb = run(append(base, "--allow-short-lived-token"), "")
	combined = out + errb
	if code != exitOK {
		t.Fatalf("--allow-short-lived-token 应放行，得到 %d：%s", code, combined)
	}
	if !strings.Contains(combined, "WARN") || !strings.Contains(combined, "15 分钟") {
		t.Errorf("放行时必须有醒目告警：%s", combined)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), token) {
		t.Errorf("放行时确实写入令牌：%s", body)
	}

	// 长期密钥（--api-key ximo_sk_…）正常放行，不告警
	longTarget := filepath.Join(home, "cfg3.json")
	useTarget(longTarget)
	code, out, errb = run([]string{"--home", home, "apply", "--spec", "synth", "--gateway", "http://gw:8600",
		"--model", "m1", "--api-key", "ximo_sk_" + strings.Repeat("a", 32), "--yes"}, "")
	if code != exitOK {
		t.Fatalf("长期密钥应正常写入，得到 %d：%s", code, out+errb)
	}
	if strings.Contains(out+errb, "临时访问令牌") {
		t.Errorf("长期密钥不该出现短期令牌告警：%s", out+errb)
	}
}

// —— 缺陷 2：create=true 的目标目录不存在时要补建 ——

func TestApplyCreatesMissingParentDir(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "new-cli", "sub", "cfg.json")
	mustEnv(t, "XIMO_TEST_SYNTH", "1")
	useSpecs(t, []spec.Spec{synthSpec("synth", target, true)})
	setTTY(t, false)

	code, _, errb := run([]string{"--home", home, "apply", "--spec", "synth", "--gateway", "http://gw:8600",
		"--api-key", "sk-test-secret", "--model", "m1", "--yes"}, "")
	if code != exitOK {
		t.Fatalf("父目录不存在时应自动创建（0700），得到退出码 %d：%s", code, errb)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("目标文件未写入：%v", err)
	}
}

// —— 缺陷 4：env-only 的适配器不得谎报"已写入配置" ——

func envOnlySpec(id string) spec.Spec {
	return spec.Spec{
		ID:          id,
		Name:        "Env " + id,
		Description: "只认环境变量，不落盘",
		RestartNote: "环境变量对之后启动的进程生效",
		Detect:      spec.Detect{Env: []string{"XIMO_TEST_" + strings.ToUpper(id)}},
		Env:         spec.EnvSpec{BaseURL: "SYNTH_BASE_URL", APIKey: "SYNTH_API_KEY", Style: "export"},
	}
}

func TestApplyEnvOnlyReportsEnvOutputNotFileWrites(t *testing.T) {
	home := t.TempDir()
	mustEnv(t, "XIMO_TEST_ENVONLY", "1")
	useSpecs(t, []spec.Spec{envOnlySpec("envonly")})
	setTTY(t, false)

	code, out, errb := run([]string{"--home", home, "apply", "--all", "--gateway", "http://gw:8600",
		"--api-key", "sk-test-secret", "--model", "m1", "--yes"}, "")
	combined := out + errb
	if code != exitOK {
		t.Fatalf("env-only 应算成功，得到退出码 %d：%s", code, combined)
	}
	if !strings.Contains(combined, "已输出环境变量（未写入文件）") {
		t.Errorf("应如实标注「已输出环境变量（未写入文件）」：\n%s", combined)
	}
	if strings.Contains(combined, "已写入配置") {
		t.Errorf("env-only 不得说已写入配置：\n%s", combined)
	}
	for _, want := range []string{"export SYNTH_BASE_URL='http://gw:8600/v1'", "export SYNTH_API_KEY='sk-test-secret'", "只输出了环境变量"} {
		if !strings.Contains(combined, want) {
			t.Errorf("输出应包含 %q：\n%s", want, combined)
		}
	}
	// 文案说的"未写入"必须与磁盘事实一致
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("env-only 不该落盘任何文件，实际：%v", entries)
	}

	// --json 时也必须是 Mode=env，脚本才能区分"写盘"与"只输出变量"
	code, out, _ = run([]string{"--home", home, "--json", "apply", "--all", "--gateway", "http://gw:8600",
		"--api-key", "sk-test-secret", "--model", "m1", "--yes"}, "")
	if code != exitOK {
		t.Fatalf("--json env-only 退出码 %d", code)
	}
	var payload struct {
		Results []struct {
			Mode   string `json:"mode"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json 输出不是合法 JSON：%v\n%s", err, out)
	}
	if len(payload.Results) != 1 || payload.Results[0].Mode != "env" || payload.Results[0].Status != "PASS" {
		t.Errorf("env-only 的结果应标注 mode=env：%s", out)
	}
}

// —— doctor 要能看出配置里写的是短期令牌 ——

func TestDoctorDetectsShortLivedTokenInConfig(t *testing.T) {
	const token = "gwa_writtenintoconfig"
	home := t.TempDir()
	target := filepath.Join(home, "cfg.json")
	mustEnv(t, "XIMO_TEST_SYNTH", "1")
	useSpecs(t, []spec.Spec{synthSpec("synth", target, true)})
	writeCredFile(t, home, token)
	if err := os.WriteFile(target, []byte("{\n  \"provider\": {\n    \"api_key\": \""+token+"\"\n  }\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errb := run([]string{"--home", home, "doctor"}, "")
	combined := out + errb
	if code != exitFail {
		t.Fatalf("配置里是短期令牌时 doctor 应报 FAIL 并退出 1，得到 %d：\n%s", code, combined)
	}
	for _, want := range []string{"access token", "15 分钟", target} {
		if !strings.Contains(combined, want) {
			t.Errorf("doctor 输出应包含 %q：\n%s", want, combined)
		}
	}

	// 换成长期 API Key 后应 PASS（不能一直报错，否则用户没法靠 doctor 收敛）
	if err := os.WriteFile(target, []byte("{\n  \"provider\": {\n    \"api_key\": \"ximo_sk_abcdef0123456789abcdef0123456789\"\n  }\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errb := run([]string{"--home", home, "doctor"}, ""); code != exitOK || !strings.Contains(out+errb, "长期 API Key") {
		t.Fatalf("长期 API Key 应 PASS，得到 %d：\n%s", code, out+errb)
	}
}

// —— adapters show ——

func TestAdaptersShow(t *testing.T) {
	home := t.TempDir()
	useSpecs(t, []spec.Spec{synthSpec("synth", filepath.Join(home, "x.json"), true)})
	if code, out, _ := run([]string{"--home", home, "adapters", "show", "synth"}, ""); code != exitOK || !strings.Contains(out, "Synth synth") {
		t.Errorf("adapters show 失败：%d %s", code, out)
	}
	if code, _, _ := run([]string{"--home", home, "adapters", "show", "nope"}, ""); code != exitFail {
		t.Errorf("未知 id 应退出 1，得到 %d", code)
	}
	if code, _, _ := run([]string{"--home", home, "adapters", "explode"}, ""); code != exitUsage {
		t.Errorf("未知子命令应退出 2，得到 %d", code)
	}
}
