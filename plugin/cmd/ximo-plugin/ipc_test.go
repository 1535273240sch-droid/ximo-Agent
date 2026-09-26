package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/1535273240sch-droid/ximo-plugin/internal/agents"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// —— 假 ximo-agent IPC 服务端（进程内，复用 agents 的帧编解码） ——
//
// 它存在的唯一理由：把每次 system.config.set 的**原始载荷**记下来，好断言
// 「插件到底发了哪些键」。整体回传 providers / mcp_servers 会触发主仓库
// ApplySettings 的「整体替换」语义（internal/bootstrap/secrets.go），把用户
// 自己配好的候选池与 MCP 清单顶掉——这类问题只有看原始载荷才暴露得出来。

const testSecretRef = "secretref:v1:testref00000000"

type fakeAgentIPC struct {
	t        *testing.T
	ln       net.Listener
	endpoint string

	mu    sync.Mutex
	cfg   map[string]any   // 配置文件层面的当前状态
	sets  []map[string]any // 每次 config.set 的原始载荷
	puts  []string         // 每次 secret.put 的明文（只在测试进程内）
	gets  int
	avail bool

	wg      sync.WaitGroup
	done    chan struct{}
	stopped sync.Once
}

func newFakeAgentIPC(t *testing.T, cfg map[string]any) *fakeAgentIPC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动假 IPC 服务端失败: %v", err)
	}
	f := &fakeAgentIPC{
		t:        t,
		ln:       ln,
		endpoint: "tcp://" + ln.Addr().String(),
		cfg:      cloneJSONMap(cfg),
		avail:    true,
		done:     make(chan struct{}),
	}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeAgentIPC) stop() {
	f.stopped.Do(func() {
		close(f.done)
		_ = f.ln.Close()
	})
	f.wg.Wait()
}

func (f *fakeAgentIPC) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer conn.Close()
			for {
				req, err := agents.ReadFrame(conn, agents.DefaultMaxPayload)
				if err != nil {
					return
				}
				if resp := f.dispatch(req); resp != nil {
					if err := agents.WriteFrame(conn, resp); err != nil {
						return
					}
				}
			}
		}()
	}
}

func (f *fakeAgentIPC) dispatch(req *agents.Frame) *agents.Frame {
	switch req.Header.Type {
	case agents.TypeConfigGet:
		f.mu.Lock()
		f.gets++
		cur := cloneJSONMap(f.cfg)
		f.mu.Unlock()
		return okFrame(req, cur)
	case agents.TypeSecretPut:
		var p struct {
			Value string `json:"value"`
		}
		if err := decodeReqEnvelope(req, &p); err != nil {
			return errFrame(req, err)
		}
		f.mu.Lock()
		if !f.avail {
			f.mu.Unlock()
			return errFrame(req, fmt.Errorf("ipcapi: 平台安全存储不可用（测试里刻意关掉）"))
		}
		f.puts = append(f.puts, p.Value)
		f.cfg["secret_ref"] = testSecretRef
		f.mu.Unlock()
		return okFrame(req, map[string]any{"ref": testSecretRef})
	case agents.TypeConfigSet:
		var payload map[string]any
		if err := decodeReqEnvelope(req, &payload); err != nil {
			return errFrame(req, err)
		}
		if s, _ := payload["base_url"].(string); s == "" {
			return errFrame(req, fmt.Errorf("provider base_url must not be empty"))
		}
		f.mu.Lock()
		f.sets = append(f.sets, payload)
		f.cfg = mergeAgentSettings(f.cfg, payload)
		f.mu.Unlock()
		return okFrame(req, map[string]any{"ok": true})
	case agents.TypeModelList:
		return okFrame(req, map[string]any{"models": []any{}, "base_url": ""})
	default:
		// 与主仓库 server.go 一致：未注册帧回裸文本 system.error。
		return &agents.Frame{
			Header: agents.FrameHeader{
				Version:   agents.CurrentVersion,
				RequestID: req.Header.RequestID,
				Sequence:  req.Header.Sequence + 1,
				Type:      agents.TypeError,
			},
			Payload: []byte("unknown message type: " + req.Header.Type),
		}
	}
}

func (f *fakeAgentIPC) counts() (gets, sets, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, len(f.sets), len(f.puts)
}

// setPayload 返回第 i 次 config.set 的原始载荷。
func (f *fakeAgentIPC) setPayload(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.sets) || i < 0 {
		f.t.Fatalf("只有 %d 次 config.set，取不到第 %d 次", len(f.sets), i)
	}
	return f.sets[i]
}

func (f *fakeAgentIPC) current() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneJSONMap(f.cfg)
}

func okFrame(req *agents.Frame, v any) *agents.Frame {
	raw, _ := json.Marshal(v)
	payload, _ := json.Marshal(agents.Envelope{OK: true, Data: raw})
	return &agents.Frame{
		Header: agents.FrameHeader{
			Version:   agents.CurrentVersion,
			RequestID: req.Header.RequestID,
			Sequence:  req.Header.Sequence + 1,
			Type:      req.Header.Type,
		},
		Payload: payload,
	}
}

func errFrame(req *agents.Frame, err error) *agents.Frame {
	payload, _ := json.Marshal(agents.Envelope{OK: false, Error: err.Error()})
	return &agents.Frame{
		Header: agents.FrameHeader{
			Version:   agents.CurrentVersion,
			RequestID: req.Header.RequestID,
			Sequence:  req.Header.Sequence + 1,
			Type:      req.Header.Type,
		},
		Payload: payload,
	}
}

func decodeReqEnvelope(req *agents.Frame, dst any) error {
	var env agents.Envelope
	if err := json.Unmarshal(req.Payload, &env); err != nil {
		return fmt.Errorf("decode request envelope: %w", err)
	}
	if !env.OK {
		return fmt.Errorf("client sent a failed envelope")
	}
	return env.Decode(dst)
}

// mergeAgentSettings 复刻主仓库 ApplySettings 的合并语义
// （internal/bootstrap/secrets.go）：
//   - provider_id / provider_name / base_url / model / workspace_root 无条件覆盖；
//   - secret_ref / auto_mode 非空才覆盖，context_window / max_output_tokens 非零才覆盖；
//   - providers == nil（载荷里没有这个键）⇒ 沿用；带了就整体替换，且**非主服务商
//     的 timeout 会被主服务商的 timeout 顶掉**（extras 是用
//     `config.ProviderConfig{Timeout: a.cfg.Provider.Timeout}` 重建的）；
//   - mcp_servers / sub_agent 同理：不带即沿用。
func mergeAgentSettings(cur, in map[string]any) map[string]any {
	next := cloneJSONMap(cur)
	for _, k := range []string{"provider_id", "provider_name", "base_url", "model", "workspace_root"} {
		if v, ok := in[k]; ok {
			next[k] = v
		} else {
			next[k] = ""
		}
	}
	if s, _ := in["secret_ref"].(string); s != "" {
		next["secret_ref"] = s
	}
	if s, _ := in["auto_mode"].(string); s != "" {
		next["auto_mode"] = s
	}
	for _, k := range []string{"context_window", "max_output_tokens"} {
		if n, ok := in[k].(float64); ok && n > 0 {
			next[k] = n
		}
	}
	if pool, ok := in["providers"].([]any); ok && pool != nil {
		primaryTimeout, _ := cur["timeout_sec"].(float64)
		if len(pool) > 0 {
			if e, ok := pool[0].(map[string]any); ok {
				if t, ok := e["timeout_sec"].(float64); ok {
					primaryTimeout = t
				}
			}
		}
		replaced := make([]any, 0, len(pool))
		for i, e := range pool {
			entry, _ := e.(map[string]any)
			cp := cloneJSONMap(entry)
			if i > 0 {
				cp["timeout_sec"] = primaryTimeout
			}
			replaced = append(replaced, cp)
		}
		next["providers"] = replaced
	}
	if mcp, ok := in["mcp_servers"].([]any); ok && mcp != nil {
		next["mcp_servers"] = mcp
	}
	if sub, ok := in["sub_agent"].(map[string]any); ok {
		merged := map[string]any{}
		if curSub, ok := next["sub_agent"].(map[string]any); ok {
			merged = cloneJSONMap(curSub)
		}
		for k, v := range sub {
			if v != nil {
				merged[k] = v
			}
		}
		next["sub_agent"] = merged
	}
	return next
}

func cloneJSONMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch vv := v.(type) {
		case map[string]any:
			out[k] = cloneJSONMap(vv)
		case []any:
			cp := make([]any, 0, len(vv))
			for _, e := range vv {
				if m, ok := e.(map[string]any); ok {
					cp = append(cp, cloneJSONMap(m))
					continue
				}
				cp = append(cp, e)
			}
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
}

// —— 夹具 ——

// userAgentConfig 是一份「用户自己配好的 ximo-agent 配置」：主服务商、候选池
// （备用服务商带自己的 timeout 42）、MCP 清单、子代理分配，一个都不少。
func userAgentConfig(configPath string) map[string]any {
	return map[string]any{
		"provider_id":    "user-provider",
		"provider_name":  "用户自己的服务商",
		"base_url":       "http://user-own.example/v1",
		"model":          "user-model",
		"secret_ref":     "secretref:v1:userref",
		"workspace_root": "/home/user/work",
		"auto_mode":      "manual",
		"timeout_sec":    float64(300),
		"config_path":    configPath,
		"providers": []any{
			map[string]any{
				"id": "user-provider", "name": "用户自己的服务商",
				"base_url": "http://user-own.example/v1", "model": "user-model",
				"secret_ref": "secretref:v1:userref",
			},
			map[string]any{
				"id": "backup-provider", "name": "备用服务商",
				"base_url": "http://backup.example/v1", "model": "backup-model",
				"secret_ref": "secretref:v1:backup", "timeout_sec": float64(42),
			},
		},
		"mcp_servers": []any{
			map[string]any{"id": "user-mcp", "transport": "stdio", "command": "npx", "enabled": true},
		},
		"sub_agent": map[string]any{"pool": []any{"user-provider", "backup-provider"}},
	}
}

// ximoAgentSpecForTest 造一份 id=ximo-agent 的规格：走 IPC 分支只需要 id 对得上。
// files[0].path 用调用方给的 agent 配置文件路径——真实规格里这个路径就是
// ximo-agent 自己的 config.json（%APPDATA%/ximo-agent/config.json），因此
// 「规格声明的落点」与「agent 回报的 config_path」本来就该是同一个文件。
func ximoAgentSpecForTest(t *testing.T, target string) spec.Spec {
	t.Helper()
	return spec.Spec{
		ID:          ximoAgentSpecID,
		Name:        "XIMO Agent（测试）",
		Description: "测试用",
		RestartNote: "测试用",
		Detect:      spec.Detect{Env: []string{"XIMO_TEST_XIMO_AGENT"}},
		Files: []spec.Target{{
			Path: target, Format: "json", Create: true,
			Fields: []spec.Field{
				{Path: "provider.base_url", From: "base_url"},
				{Path: "provider.api_key", From: "api_key"},
			},
		}},
	}
}

// deadEndpoint 返回一个「保证连不上」的 tcp 端点：先占住端口拿到地址，再关掉监听。
func deadEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "tcp://" + addr
}

// writeAgentConfig 在磁盘上放一份真配置文件：IPC 路径改配置前应当先备份它。
func writeAgentConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"provider":{"base_url":"http://user-own.example/v1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// —— 用例 1：IPC 写配置绝不能整体回传 providers / mcp_servers ——

func TestApplyXimoAgentViaIPCKeepsUserPool(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "agent", "config.json")
	writeAgentConfig(t, cfgPath)
	fake := newFakeAgentIPC(t, userAgentConfig(cfgPath))
	mustEnv(t, "XIMO_TEST_XIMO_AGENT", "1")
	useSpecs(t, []spec.Spec{ximoAgentSpecForTest(t, cfgPath)})
	setTTY(t, false)

	code, out, errb := run([]string{"--home", home, "apply", "--spec", ximoAgentSpecID,
		"--gateway", "http://gw:8600", "--api-key", "sk-test-secret", "--model", "m1",
		"--ipc-endpoint", fake.endpoint, "--yes"}, "")
	combined := out + errb
	if code != exitOK {
		t.Fatalf("apply 退出码 %d，想要 0\n%s", code, combined)
	}
	gets, sets, puts := fake.counts()
	if gets == 0 {
		t.Fatalf("没有走 IPC（system.config.get 一次都没发）：%s", combined)
	}
	if sets != 1 {
		t.Fatalf("config.set 应恰好发一次，实际 %d 次：%s", sets, combined)
	}
	if puts != 1 {
		t.Fatalf("secret.put 应恰好发一次（明文只经 IPC 进安全存储），实际 %d 次", puts)
	}

	payload := fake.setPayload(0)
	for _, k := range []string{"providers", "mcp_servers"} {
		if v, ok := payload[k]; ok {
			t.Errorf("config.set 载荷里出现了 %q（值=%v）：非 nil 会触发主仓库的「整体替换」，覆盖用户已配好的候选池/MCP 清单", k, v)
		}
	}
	if sub, ok := payload["sub_agent"].(map[string]any); ok {
		for k, v := range sub {
			if v != nil {
				t.Errorf("sub_agent.%s 不该被携带（值=%v），nil 才是「沿用现有配置」", k, v)
			}
		}
	}
	// 无条件覆盖的字段必须回填现值，否则会被清空。
	for k, want := range map[string]string{
		"provider_id": "ximo-gateway", "provider_name": "m1",
		"base_url": "http://gw:8600/v1", "model": "m1", "workspace_root": "/home/user/work",
	} {
		if got, _ := payload[k].(string); got != want {
			t.Errorf("config.set 载荷的 %s = %q，想要 %q（漏回填会把用户配置清空）", k, got, want)
		}
	}
	if got, _ := payload["secret_ref"].(string); got != testSecretRef {
		t.Errorf("config.set 载荷的 secret_ref = %q，想要 %q（必须是引用，不能是明文）", got, testSecretRef)
	}
	if raw, _ := json.Marshal(payload); strings.Contains(string(raw), "sk-test-secret") {
		t.Errorf("config.set 载荷里出现了密钥明文：%s", raw)
	}
	if strings.Contains(combined, "sk-test-secret") {
		t.Errorf("命令输出里出现了密钥明文：%s", combined)
	}

	// agent 侧的状态：用户自己的池与 MCP 清单原样保留，只有主 provider 被改。
	after := fake.current()
	if got, _ := after["provider_id"].(string); got != "ximo-gateway" {
		t.Errorf("主 provider 未改：provider_id=%q", got)
	}
	pool, _ := after["providers"].([]any)
	if len(pool) != 2 {
		t.Fatalf("候选池被改动了（应保持 2 项）：%v", after["providers"])
	}
	second, _ := pool[1].(map[string]any)
	if id, _ := second["id"].(string); id != "backup-provider" {
		t.Errorf("备用服务商丢了：%v", pool)
	}
	// 备用服务商自己的 timeout 必须还在：带了 providers 就会走整体替换，
	// 它的 timeout 会被主服务商的 300 顶掉（主仓库 providerFromEntry 的默认值）。
	if got, _ := second["timeout_sec"].(float64); got != 42 {
		t.Errorf("备用服务商的 timeout_sec 被顶掉了：%v（整体替换语义的后果）", got)
	}
	if mcp, _ := after["mcp_servers"].([]any); len(mcp) != 1 {
		t.Errorf("MCP 清单被改动了：%v", after["mcp_servers"])
	}
	if sub, _ := after["sub_agent"].(map[string]any); sub == nil || len(sub["pool"].([]any)) != 2 {
		t.Errorf("子代理分配被改动了：%v", after["sub_agent"])
	}
	if got, _ := after["workspace_root"].(string); got != "/home/user/work" {
		t.Errorf("workspace_root 被清空了：%q", got)
	}

	// 备份：改 agent 的配置文件前必须留一份。
	baks, _ := filepath.Glob(cfgPath + ".bak-*")
	if len(baks) == 0 {
		t.Errorf("IPC 路径也必须先备份 %s（未生成 .bak-*）", cfgPath)
	}
}

// —— 用例 2：dry-run 走 IPC 只读、且能报出真实差异 ——

func TestApplyXimoAgentDryRunIsReadOnly(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "agent", "config.json")
	writeAgentConfig(t, cfgPath)
	fake := newFakeAgentIPC(t, userAgentConfig(cfgPath))
	mustEnv(t, "XIMO_TEST_XIMO_AGENT", "1")
	useSpecs(t, []spec.Spec{ximoAgentSpecForTest(t, cfgPath)})
	setTTY(t, false)

	code, out, errb := run([]string{"--home", home, "apply", "--spec", ximoAgentSpecID,
		"--gateway", "http://gw:8600", "--api-key", "sk-test-secret", "--model", "m1",
		"--ipc-endpoint", fake.endpoint, "--dry-run"}, "")
	combined := out + errb
	if code != exitOK {
		t.Fatalf("dry-run 退出码 %d，想要 0\n%s", code, combined)
	}
	gets, sets, puts := fake.counts()
	if gets == 0 {
		t.Errorf("dry-run 应读一次运行时配置（system.config.get）来打印真实差异\n%s", combined)
	}
	if sets != 0 || puts != 0 {
		t.Fatalf("dry-run 不得写任何东西：config.set=%d secret.put=%d", sets, puts)
	}
	if !strings.Contains(combined, "secret_ref") {
		t.Errorf("dry-run 应说明密钥将经安全存储写入，实际输出：%s", combined)
	}
	if baks, _ := filepath.Glob(cfgPath + ".bak-*"); len(baks) != 0 {
		t.Errorf("dry-run 不得产生备份：%v", baks)
	}
	if after := fake.current(); after["provider_id"] != "user-provider" {
		t.Errorf("dry-run 后 agent 侧配置被改了：%v", after["provider_id"])
	}
	original, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(original), "user-own.example") {
		t.Errorf("dry-run 改动了 agent 的配置文件：%s", original)
	}
}

// —— 用例 3：IPC 连不上时回退改文件，并且必须告警 ——

func TestApplyXimoAgentFallsBackToFileWithWarning(t *testing.T) {
	home := t.TempDir()
	// 落点就是规格声明的 agent 配置文件；先放一份真文件，回退改写前应当备份它。
	target := filepath.Join(home, "agent", "config.json")
	writeAgentConfig(t, target)

	mustEnv(t, "XIMO_TEST_XIMO_AGENT", "1")
	useSpecs(t, []spec.Spec{ximoAgentSpecForTest(t, target)})
	setTTY(t, false)

	code, out, errb := run([]string{"--home", home, "apply", "--spec", ximoAgentSpecID,
		"--gateway", "http://gw:8600", "--api-key", "sk-test-secret", "--model", "m1",
		"--ipc-endpoint", deadEndpoint(t), "--yes"}, "")
	combined := out + errb
	if code != exitOK {
		t.Fatalf("回退路径应成功，退出码 %d\n%s", code, combined)
	}
	if !strings.Contains(combined, "[警告]") || !strings.Contains(combined, "回退") {
		t.Errorf("回退改文件必须有显眼告警：%s", combined)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("回退路径应写入 %s：%v", target, err)
	}
	if !strings.Contains(string(body), "http://gw:8600/v1") {
		t.Errorf("回退写入内容不对：%s", body)
	}
	if baks, _ := filepath.Glob(target + ".bak-*"); len(baks) == 0 {
		t.Errorf("回退路径也应备份")
	}
}

// —— 用例 4：--home 必须把 %APPDATA% / $XIMO_HOME 一并隔离 ——

func TestApplyHomeIsolatesAppDataAndXimoHome(t *testing.T) {
	home := t.TempDir()
	decoyAppData := filepath.Join(t.TempDir(), "real-appdata")
	decoyXimoHome := filepath.Join(t.TempDir(), "real-ximo-home")
	mustEnv(t, "APPDATA", decoyAppData, "XIMO_HOME", decoyXimoHome, "XIMO_TEST_APPDATA_SPEC", "1")

	target := filepath.Join(home, "AppData", "Roaming", "ximo-agent", "config.json")
	useSpecs(t, []spec.Spec{{
		ID: "appdata-synth", Name: "AppData Synth", Description: "d", RestartNote: "r",
		Detect: spec.Detect{Env: []string{"XIMO_TEST_APPDATA_SPEC"},
			Paths: []string{"%APPDATA%/ximo-agent/config.json", "%XIMO_HOME%/config.json"}},
		Files: []spec.Target{{
			Path: "%APPDATA%/ximo-agent/config.json", Format: "json", Create: true,
			Fields: []spec.Field{{Path: "provider.base_url", From: "base_url"}},
		}},
	}})
	setTTY(t, false)

	// detect 的证据必须落在隔离后的 home 内。
	code, out, errb := run([]string{"--home", home, "--json", "detect"}, "")
	if code != exitOK {
		t.Fatalf("detect 退出码 %d；stderr=%s", code, errb)
	}
	var det struct {
		Detected []struct {
			Evidence []struct {
				Kind     string `json:"kind"`
				Need     string `json:"need"`
				Resolved string `json:"resolved"`
			} `json:"evidence"`
		} `json:"detected"`
	}
	if err := json.Unmarshal([]byte(out), &det); err != nil {
		t.Fatalf("detect --json 输出不是合法 JSON：%v\n%s", err, out)
	}
	if len(det.Detected) != 1 {
		t.Fatalf("应检测到 1 个适配器：%s", out)
	}
	for _, ev := range det.Detected[0].Evidence {
		if ev.Kind != "path" {
			continue
		}
		if !strings.HasPrefix(ev.Resolved, filepath.Clean(home)) {
			t.Errorf("--home 生效时证据 %q 解析到 %q，逃出了隔离 home %q", ev.Need, ev.Resolved, home)
		}
	}

	// apply 必须写进隔离 home，一个字节都不能落到真实的 AppData / XIMO_HOME。
	code, out, errb = run([]string{"--home", home, "apply", "--spec", "appdata-synth",
		"--gateway", "http://gw:8600", "--api-key", "sk-test-secret", "--yes"}, "")
	if code != exitOK {
		t.Fatalf("apply 退出码 %d\n%s%s", code, out, errb)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("应写入隔离 home 下的 %s：%v", target, err)
	}
	for _, decoy := range []string{decoyAppData, decoyXimoHome} {
		if _, err := os.Stat(decoy); err == nil {
			ents, _ := os.ReadDir(decoy)
			if len(ents) > 0 {
				t.Errorf("--home 生效时不得写入真实用户目录，却在 %s 下产生了 %d 个条目", decoy, len(ents))
			}
		}
	}
}
