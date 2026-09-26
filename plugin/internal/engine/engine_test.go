package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// captureOut 接管 engine 的输出，返回取回内容的函数。
func captureOut(t *testing.T) func() string {
	t.Helper()
	old := Out
	var buf bytes.Buffer
	Out = &buf
	t.Cleanup(func() { Out = old })
	return buf.String
}

// numIs 兼容不同解码器给出的数字类型（float64 / json.Number / int）。
func numIs(v any, want string) bool { return fmt.Sprintf("%v", v) == want }

func helperBinary() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

func TestDetectEvidence(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "agent", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XIMO_TEST_ENV_SET", "1")

	specs := []spec.Spec{
		{ID: "hit", Name: "hit", Detect: spec.Detect{
			Binaries: []string{helperBinary()},
			Paths:    []string{"~/agent/config.json", "~/agent/missing.json"},
			Env:      []string{"XIMO_TEST_ENV_SET"},
		}},
		{ID: "miss", Name: "miss", Detect: spec.Detect{
			Binaries: []string{"ximo-plugin-not-a-real-binary"},
			Paths:    []string{"~/nope/nope.json"},
			Env:      []string{"XIMO_TEST_ENV_UNSET"},
		}},
		{ID: "noev", Name: "noev"},
	}

	dets := Detect(specs, home)
	if len(dets) != 3 {
		t.Fatalf("想要 3 条检测结果，得到 %d", len(dets))
	}
	if !dets[0].Found {
		t.Fatalf("hit 应命中，证据：%+v", dets[0].Evidence)
	}
	// 证据里必须有展开后的真实路径（含 ~ 展开）
	if got := dets[0].Evidence[1].Resolved; got != cfg {
		t.Errorf("~ 未按 home 展开：想要 %s，得到 %s", cfg, got)
	}
	if dets[0].Evidence[1].OK != true || dets[0].Evidence[2].OK != false {
		t.Errorf("路径证据的命中状态不对：%+v", dets[0].Evidence)
	}
	if !strings.Contains(dets[0].Evidence[0].Resolved, helperBinary()) {
		t.Errorf("二进制证据应给出解析后的路径，得到 %q", dets[0].Evidence[0].Resolved)
	}
	if dets[1].Found {
		t.Errorf("miss 不应命中：%+v", dets[1].Evidence)
	}
	if !strings.Contains(dets[1].Evidence[0].Detail, "PATH") {
		t.Errorf("未命中应给出原因，得到 %q", dets[1].Evidence[0].Detail)
	}
	if dets[2].Found || len(dets[2].Evidence) != 0 {
		t.Errorf("没有 detect 声明的规格不应命中：%+v", dets[2])
	}
	if got := Found(dets); len(got) != 1 || got[0].SpecID != "hit" {
		t.Errorf("Found 过滤结果不对：%+v", got)
	}
}

func TestBuildPlanPathsValuesAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	seed := `{"keep":{"x":1},"models":[{"id":"old","extra":true}],"provider":{"old":true}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	s := spec.Spec{ID: "synth", Name: "Synth", Files: []spec.Target{{
		Path: path, Format: "json",
		Fields: []spec.Field{
			{Path: "provider.base_url", From: "base_url"},
			{Path: "provider.api_key", From: "api_key"},
			{Path: "models.0.id", From: "model"},
			{Path: "tag", From: "literal", Value: "v"},
		},
	}}}

	steps, values, err := BuildPlan(s, Inputs{GatewayURL: "http://gw:8600/", APIKey: "sk-test-secret", Model: "m1", Home: dir})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if len(steps) != 1 || steps[0].Kind != KindFile || steps[0].Target != path || !steps[0].Exists {
		t.Fatalf("计划步骤不对：%+v", steps)
	}
	if values["base_url"] != "http://gw:8600/v1" || values["gateway_url"] != "http://gw:8600" {
		t.Errorf("values 里的地址不对：%+v", values)
	}
	if values["api_key"] == "sk-test-secret" {
		t.Fatalf("values 绝不能含明文密钥：%+v", values)
	}

	doc := steps[0].Doc()
	prov, _ := doc["provider"].(map[string]any)
	if prov["base_url"] != "http://gw:8600/v1" || prov["api_key"] != "sk-test-secret" {
		t.Errorf("字段未写入：%+v", prov)
	}
	if prov["old"] != true {
		t.Errorf("未知字段 provider.old 应保留：%+v", prov)
	}
	if keep, _ := doc["keep"].(map[string]any); !numIs(keep["x"], "1") {
		t.Errorf("未知顶层字段 keep 应保留：%+v", doc)
	}
	models, _ := doc["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("数组下标路径未生效：%+v", doc["models"])
	}
	first, _ := models[0].(map[string]any)
	if first["id"] != "m1" || first["extra"] != true {
		t.Errorf("数组元素内的未知字段应保留：%+v", first)
	}
	if doc["tag"] != "v" {
		t.Errorf("literal 字段未写入：%+v", doc["tag"])
	}
}

func TestBuildPlanRequiresKeyAndCreate(t *testing.T) {
	dir := t.TempDir()
	withKey := spec.Spec{ID: "k", Files: []spec.Target{{
		Path: filepath.Join(dir, "a.json"), Format: "json", Create: true,
		Fields: []spec.Field{{Path: "a.b", From: "api_key"}},
	}}}
	if _, _, err := BuildPlan(withKey, Inputs{GatewayURL: "http://gw", Home: dir}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("缺密钥应报 ErrMissingAPIKey，得到 %v", err)
	}
	if _, _, err := BuildPlan(withKey, Inputs{GatewayURL: "", APIKey: "k", Home: dir}); err == nil {
		t.Fatal("缺 --gateway 应报错")
	}
	noCreate := spec.Spec{ID: "c", Files: []spec.Target{{
		Path: filepath.Join(dir, "missing", "b.json"), Format: "json",
		Fields: []spec.Field{{Path: "a.b", From: "literal", Value: "x"}},
	}}}
	_, _, err := BuildPlan(noCreate, Inputs{GatewayURL: "http://gw", Home: dir})
	if err == nil || !strings.Contains(err.Error(), "create") {
		t.Fatalf("未声明 create 且文件不存在应报错，得到 %v", err)
	}
}

// —— dry-run 必须把目标文件里**已有的**密钥也打码（缺陷 1 回归） ——

func TestApplyDryRunMasksPreExistingSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	seed := "{\n  \"provider\": {\n    \"base_url\": \"http://old:1/v1\",\n    \"api_key\": \"sk-old-existing-secret\"\n  }\n}\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	s := spec.Spec{ID: "s", Files: []spec.Target{{
		Path: path, Format: "json",
		Fields: []spec.Field{
			{Path: "provider.base_url", From: "base_url"},
			{Path: "provider.api_key", From: "api_key"},
		},
	}}}
	steps, _, err := BuildPlan(s, Inputs{GatewayURL: "http://gw:8600", APIKey: "sk-brandnew-secret", Home: dir})
	if err != nil {
		t.Fatal(err)
	}

	old := MaskSecretsInDryRun
	MaskSecretsInDryRun = true
	defer func() { MaskSecretsInDryRun = old }()

	out := captureOut(t)
	if err := Apply(steps, true); err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	got := out()
	// 旧密钥不在本次计划里，只有"按字段名屏蔽取值"能挡住它。
	for _, leak := range []string{"sk-old-existing-secret", "sk-brandnew-secret"} {
		if strings.Contains(got, leak) {
			t.Errorf("dry-run 输出泄漏密钥 %q：\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "sk-o") || !strings.Contains(got, "sk-b") {
		t.Errorf("差异里应保留打码后的密钥：\n%s", got)
	}
	if strings.Contains(got, "\"api_key\"") == false {
		t.Errorf("字段名本身应保留（只是值被打码）：\n%s", got)
	}

	// --show-secrets 才显示真值（MaskSecretsInDryRun=false）
	MaskSecretsInDryRun = false
	out2 := captureOut(t)
	if err := Apply(steps, true); err != nil {
		t.Fatalf("dry-run（--show-secrets）失败: %v", err)
	}
	if !strings.Contains(out2(), "sk-old-existing-secret") {
		t.Errorf("--show-secrets 下应显示真值：\n%s", out2())
	}
}

// 打码只该打凭据：非密钥字段（模式名、计数、地址）被一起打掉只会让人看不懂差异。
func TestMaskDiffSecretsKeepsNonSecrets(t *testing.T) {
	diff := strings.Join([]string{
		"--- cfg.json (current)",
		"+++ cfg.json (after)",
		"@@ -1,4 +1,4 @@",
		`-  "api_key": "sk-old-secret-value",`,
		`+  "api_key": "sk-new-secret-value",`,
		`   "auth_type": "bearer",`,
		`   "max_tokens": 4096,`,
		`   "base_url": "http://gw:8600/v1"`,
	}, "\n")
	got := maskDiffSecrets(diff, "sk-new-secret-value")
	for _, leak := range []string{"sk-old-secret-value", "sk-new-secret-value"} {
		if strings.Contains(got, leak) {
			t.Errorf("应打码的密钥没打码 %q：\n%s", leak, got)
		}
	}
	for _, keep := range []string{"bearer", "4096", "http://gw:8600/v1", "auth_type", "max_tokens"} {
		if !strings.Contains(got, keep) {
			t.Errorf("非密钥取值 %q 不该被改掉：\n%s", keep, got)
		}
	}
}

// —— create=true 要补建父目录，失败回滚时连新目录一起撤掉（缺陷 2 回归） ——

func TestApplyCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "deep", "nested", "config.json")
	steps := []PlanStep{{SpecID: "s", Kind: KindFile, Target: target, Format: "json",
		doc: map[string]any{"api_key": "sk-x"}}}
	if err := Apply(steps, false); err != nil {
		t.Fatalf("目标目录不存在时 Apply 应补建父目录，实际失败: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("目标文件未写入: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Dir(target))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("新建目录权限应为 0700，得到 %04o", perm)
		}
	}
}

func TestApplyRemovesNewDirsOnRollback(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh", "sub")
	good := filepath.Join(fresh, "keep.json")
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(blocker, "cfg.json") // 父路径是个文件 → 必然写不进去
	err := Apply([]PlanStep{
		{SpecID: "s", Kind: KindFile, Target: good, Format: "json", doc: map[string]any{"a": 1}},
		{SpecID: "s", Kind: KindFile, Target: bad, Format: "json", doc: map[string]any{"a": 1}},
	}, false)
	if err == nil {
		t.Fatal("第二个目标写不进去时应报错")
	}
	if !strings.Contains(err.Error(), "回滚") {
		t.Errorf("错误里应说明回滚情况，得到 %v", err)
	}
	if _, serr := os.Stat(good); !os.IsNotExist(serr) {
		t.Errorf("失败后本轮新建的文件应删除：%v", serr)
	}
	if _, serr := os.Stat(fresh); !os.IsNotExist(serr) {
		t.Errorf("失败后本轮新建的目录应删除：%v", serr)
	}
	if _, serr := os.Stat(blocker); serr != nil {
		t.Errorf("回滚不得动写前就存在的文件：%v", serr)
	}
}

func TestApplyDryRunDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{"provider":{"a":1}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	steps, _, err := BuildPlan(spec.Spec{ID: "s", Files: []spec.Target{{
		Path: path, Format: "json",
		Fields: []spec.Field{{Path: "provider.b", From: "literal", Value: "2"}},
	}}}, Inputs{GatewayURL: "http://gw", Home: dir})
	if err != nil {
		t.Fatal(err)
	}

	out := captureOut(t)
	if err := Apply(steps, true); err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("dry-run 不得改文件：\n原=%s\n现=%s", original, got)
	}
	if baks, _ := filepath.Glob(path + ".bak-*"); len(baks) != 0 {
		t.Errorf("dry-run 不得产生备份：%v", baks)
	}
	if !strings.Contains(out(), "dry-run") {
		t.Errorf("dry-run 应打印差异，实际输出：%s", out())
	}
}

func TestApplyWritesBackupAndRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := "{\n  \"keep\": 1\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	steps, _, err := BuildPlan(spec.Spec{ID: "s", Files: []spec.Target{{
		Path: path, Format: "json",
		Fields: []spec.Field{{Path: "keep", From: "literal", Value: "2"}},
	}}}, Inputs{GatewayURL: "http://gw", Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(steps, false); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "2") {
		t.Errorf("字段未写入：%s", got)
	}
	baks, _ := filepath.Glob(path + ".bak-*")
	if len(baks) == 0 {
		t.Fatal("应生成 <path>.bak-<unix> 备份")
	}
	bak, _ := os.ReadFile(baks[0])
	if string(bak) != original {
		t.Errorf("备份内容应是写前内容：%s", bak)
	}

	// 第二个目标故意写不进去（父路径是个文件）→ 前一个文件必须回滚。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(blocker, "cannot.json")
	steps2 := []PlanStep{
		{SpecID: "s", Kind: KindFile, Target: path, Format: "json", doc: map[string]any{"keep": 3}},
		{SpecID: "s", Kind: KindFile, Target: bad, Format: "json", doc: map[string]any{"a": 1}},
	}
	err = Apply(steps2, false)
	if err == nil {
		t.Fatal("写不进去时应报错")
	}
	if !strings.Contains(err.Error(), "回滚") {
		t.Errorf("错误里应说明回滚情况，得到 %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(got) {
		t.Errorf("失败后应回滚到本轮写前内容：\n想要 %s\n得到 %s", got, after)
	}
}

func TestApplyRejectsStepWithoutDoc(t *testing.T) {
	err := Apply([]PlanStep{{SpecID: "s", Kind: KindFile, Target: "x.json"}}, false)
	if err == nil || !strings.Contains(err.Error(), "待写文档") {
		t.Fatalf("缺文档应报错，得到 %v", err)
	}
	if err := Apply([]PlanStep{{SpecID: "s", Kind: KindEnv, Vars: []string{"A"}}}, false); err != nil {
		t.Fatalf("env 步骤不写盘，不应报错：%v", err)
	}
}

func TestEnvLinesStyles(t *testing.T) {
	s := spec.Spec{Env: spec.EnvSpec{BaseURL: "OPENAI_BASE_URL", APIKey: "OPENAI_API_KEY", Model: "OPENAI_MODEL"}}
	in := Inputs{GatewayURL: "http://gw:8600", APIKey: "sk-x", Model: "m"}
	cases := map[string]string{
		"export": "export OPENAI_BASE_URL='http://gw:8600/v1'",
		"set":    "set OPENAI_BASE_URL=http://gw:8600/v1",
	}
	for style, want := range cases {
		lines, err := EnvLines(s, in, style)
		if err != nil {
			t.Fatalf("%s: %v", style, err)
		}
		if lines[0] != want {
			t.Errorf("%s 第一行 = %q，想要 %q", style, lines[0], want)
		}
	}
	lines, err := EnvLines(s, in, "powershell")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(lines[0], "$env:OPENAI_BASE_URL=") {
		t.Errorf("powershell 渲染不对：%q", lines[0])
	}
	if _, err := EnvLines(s, in, "cmd"); err == nil {
		t.Error("未知 style 应报错")
	}
	if got := MaskKey("sk-test-secret"); got == "sk-test-secret" || !strings.Contains(got, "*") {
		t.Errorf("MaskKey 必须打码：%q", got)
	}
}
