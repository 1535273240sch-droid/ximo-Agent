package spec

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fsDirNames 列出内嵌目录下的文件名（不含子目录）。
func fsDirNames(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// readFileFS 读内嵌文件。
func readFileFS(fsys fs.FS, path string) ([]byte, error) {
	return fs.ReadFile(fsys, path)
}

// builtinRequired 是契约 §5 要求必须内置的适配器。
var builtinRequired = []string{
	"claude-code",
	"codex-cli",
	"continue",
	"cursor",
	"aider",
	"openai-compatible-generic",
	"env-openai",
	"env-anthropic",
	"ximo-agent",
}

func writeSpec(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return path
}

func validBase() Spec {
	return Spec{
		ID:          "demo",
		Name:        "Demo",
		Description: "示例；路径/schema 需用户确认。",
		Detect:      Detect{Binaries: []string{"demo"}},
		Env:         EnvSpec{BaseURL: "OPENAI_BASE_URL", APIKey: "OPENAI_API_KEY", Style: StyleExport},
		RestartNote: "需重启 demo。",
		Protocols:   []string{ProtocolOpenAIChat},
	}
}

// TestBuiltinRequiredAdaptersLoadAndValidate 覆盖「内置全部规格可加载且 Validate 通过」。
func TestBuiltinRequiredAdaptersLoadAndValidate(t *testing.T) {
	specs, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin() 失败: %v", err)
	}
	if len(specs) < len(builtinRequired) {
		t.Fatalf("内置规格只有 %d 份，少于契约要求的 %d 份", len(specs), len(builtinRequired))
	}
	byID := map[string]Spec{}
	for _, s := range specs {
		if _, dup := byID[s.ID]; dup {
			t.Fatalf("内置规格 id 重复: %s", s.ID)
		}
		byID[s.ID] = s
		if err := Validate(s); err != nil {
			t.Errorf("%s: Validate 未通过: %v", s.ID, err)
		}
		if strings.TrimSpace(s.RestartNote) == "" {
			t.Errorf("%s: 缺 restart_note（契约要求每份规格都必须写）", s.ID)
		}
		if s.Detect.Empty() {
			t.Errorf("%s: 缺 detect 证据（契约要求每份规格都必须有可执行证据）", s.ID)
		}
		if len(s.Protocols) == 0 {
			t.Errorf("%s: 没有声明 protocols", s.ID)
		}
	}
	for _, id := range builtinRequired {
		s, ok := byID[id]
		if !ok {
			t.Errorf("缺少契约要求的适配器: %s", id)
			continue
		}
		if id == "ximo-agent" && !strings.Contains(s.Description, "internal/agents") {
			t.Errorf("ximo-agent: description 必须写明需要代码适配（internal/agents 负责 IPC）")
		}
		if _, ok := Get(specs, id); !ok {
			t.Errorf("Get(%q) 取不到", id)
		}
	}

	// 排序稳定，便于 CLI 输出。
	for i := 1; i < len(specs); i++ {
		if specs[i-1].ID >= specs[i].ID {
			t.Fatalf("内置规格未按 id 升序: %s 在 %s 之前", specs[i-1].ID, specs[i].ID)
		}
	}
}

// TestBuiltinEmbedIntegrity 校验 embed 完整性：内嵌目录里的每个 *.json 都能解析、
// 文件名与 id 一致、且没有"磁盘上有文件但没被 embed 进去"的情况。
func TestBuiltinEmbedIntegrity(t *testing.T) {
	embedded, err := fsDirNames(BuiltinFS(), builtinDir)
	if err != nil {
		t.Fatalf("读内嵌目录失败: %v", err)
	}
	var jsonNames []string
	for _, n := range embedded {
		if strings.EqualFold(filepath.Ext(n), ".json") {
			jsonNames = append(jsonNames, n)
		}
	}
	specs, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin() 失败: %v", err)
	}
	if len(jsonNames) != len(specs) {
		t.Fatalf("内嵌目录有 %d 个 json，但只加载出 %d 份规格: %v", len(jsonNames), len(specs), jsonNames)
	}
	if len(jsonNames) < len(builtinRequired) {
		t.Fatalf("内嵌 json 数量 %d 少于契约要求的 %d", len(jsonNames), len(builtinRequired))
	}

	// 反向控制：磁盘源目录（测试工作目录即包目录）必须与内嵌数量一致，
	// 否则说明有文件没进 embed。
	disk, err := os.ReadDir(builtinDir)
	if err != nil {
		t.Fatalf("读源目录失败: %v", err)
	}
	diskJSON := 0
	for _, e := range disk {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			diskJSON++
		}
	}
	if diskJSON != len(jsonNames) {
		t.Fatalf("磁盘 %d 个 json，embed 里 %d 个 —— 有文件没被编进二进制", diskJSON, len(jsonNames))
	}

	for _, s := range specs {
		raw, err := BuiltinFS().Open(builtinDir + "/" + s.ID + ".json")
		if err != nil {
			t.Errorf("%s: embed 里按 id 找不到同名文件: %v", s.ID, err)
			continue
		}
		_ = raw.Close()
	}
}

// TestBuiltinNoSecretsInPlaintext 是红线检查：内置规格里不得出现密钥字面量。
func TestBuiltinNoSecretsInPlaintext(t *testing.T) {
	names, err := fsDirNames(BuiltinFS(), builtinDir)
	if err != nil {
		t.Fatalf("读内嵌目录失败: %v", err)
	}
	for _, n := range names {
		if !strings.EqualFold(filepath.Ext(n), ".json") {
			continue
		}
		data, err := readFileFS(BuiltinFS(), builtinDir+"/"+n)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", n, err)
		}
		lower := strings.ToLower(string(data))
		for _, bad := range []string{"sk-", "sk_", "bearer ", "api_key_value", "password"} {
			if strings.Contains(lower, bad) {
				t.Errorf("%s: 内置规格里出现疑似密钥字面量 %q", n, bad)
			}
		}
	}
	// 所有 env.* 都必须只是变量名（不是值）——Validate 已强制，这里再确认一次。
	specs, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin() 失败: %v", err)
	}
	for _, s := range specs {
		for label, v := range map[string]string{"base_url": s.Env.BaseURL, "api_key": s.Env.APIKey, "model": s.Env.Model} {
			if v == "" {
				continue
			}
			if !envNameRe.MatchString(v) {
				t.Errorf("%s: env.%s=%q 不是环境变量名", s.ID, label, v)
			}
		}
	}
}

// TestLoadUserOverridesBuiltin 覆盖「用户目录覆盖内置」。
func TestLoadUserOverridesBuiltin(t *testing.T) {
	home := t.TempDir()
	dir := UserDir(home)
	if want := filepath.Join(home, ".ximo-plugin", "specs"); dir != want {
		t.Fatalf("UserDir = %q, want %q", dir, want)
	}

	writeSpec(t, dir, "claude-code.json", `{
	  "id": "claude-code",
	  "name": "我的 Claude Code",
	  "description": "用户覆盖版；路径/schema 需用户确认。",
	  "detect": {"binaries": ["claude"]},
	  "env": {"base_url": "ANTHROPIC_BASE_URL", "api_key": "ANTHROPIC_AUTH_TOKEN", "style": "export"},
	  "restart_note": "重启。",
	  "protocols": ["anthropic-messages"]
	}`)
	writeSpec(t, dir, "my-cli.json", `{
	  "id": "my-cli",
	  "name": "My CLI",
	  "description": "全新自定义；路径/schema 需用户确认。",
	  "detect": {"paths": ["~/.my-cli"]},
	  "env": {"base_url": "OPENAI_BASE_URL", "api_key": "OPENAI_API_KEY", "style": "export"},
	  "restart_note": "重启 my-cli。",
	  "protocols": ["openai-chat"]
	}`)
	// 干扰文件：非 json、子目录里的 json、隐藏文件，都必须被忽略。
	writeSpec(t, dir, "README.md", "# 不是规格")
	writeSpec(t, dir, ".hidden.json", `{"id":"hidden"}`)
	writeSpec(t, filepath.Join(dir, "sub"), "nested.json", `{"id":"nested"}`)

	specs, err := Load(home)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(specs) != len(builtinRequired)+1 {
		t.Fatalf("合并后有 %d 份规格，期望 %d 份: %v", len(specs), len(builtinRequired)+1, IDs(specs))
	}
	cc, ok := Get(specs, "claude-code")
	if !ok {
		t.Fatal("找不到 claude-code")
	}
	if cc.Name != "我的 Claude Code" {
		t.Errorf("用户同名 id 没有覆盖内置: name=%q", cc.Name)
	}
	if _, ok := Get(specs, "my-cli"); !ok {
		t.Error("用户新增的规格没有出现")
	}
	for _, bad := range []string{"hidden", "nested"} {
		if _, ok := Get(specs, bad); ok {
			t.Errorf("不该被加载的规格出现了: %s", bad)
		}
	}
	for i := 1; i < len(specs); i++ {
		if specs[i-1].ID >= specs[i].ID {
			t.Fatalf("合并结果未排序: %s 在 %s 之前", specs[i-1].ID, specs[i].ID)
		}
	}

	// 用户目录不存在时只返回内置。
	bare := t.TempDir()
	only, err := Load(bare)
	if err != nil {
		t.Fatalf("Load(空 home) 失败: %v", err)
	}
	if len(only) != len(builtinRequired) {
		t.Fatalf("空 home 返回 %d 份，期望 %d 份", len(only), len(builtinRequired))
	}
}

// TestLoadRejectsDirtyJSON 覆盖「脏 JSON 报错」的各个分支。
func TestLoadRejectsDirtyJSON(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		body    string
		wantSub string
	}{
		{"语法错误", "broken.json", `{"id": "broken",}`, "JSON 解析失败"},
		{"数组而不是对象", "arr.json", `[{"id":"arr"}]`, "JSON 解析失败"},
		{"多个 JSON 值", "two.json", `{"id":"two","name":"x","description":"y","detect":{"binaries":["x"]},"env":{"base_url":"OPENAI_BASE_URL"},"restart_note":"z"} {"id":"two2"}`, "不止一个 JSON 值"},
		{"未知字段", "typo.json", `{"id":"typo","name":"x","description":"y","detect":{"binariess":["x"]},"env":{"base_url":"OPENAI_BASE_URL"},"restart_note":"z"}`, "unknown field"},
		{"文件名与 id 不一致", "wrongname.json", `{"id":"other","name":"x","description":"y","detect":{"binaries":["x"]},"env":{"base_url":"OPENAI_BASE_URL"},"restart_note":"z"}`, "文件名必须与 id 一致"},
		{"缺 restart_note", "nores.json", `{"id":"nores","name":"x","description":"y","detect":{"binaries":["x"]},"env":{"base_url":"OPENAI_BASE_URL"}}`, "restart_note"},
		{"无 detect 证据", "nodetect.json", `{"id":"nodetect","name":"x","description":"y","env":{"base_url":"OPENAI_BASE_URL"},"restart_note":"z"}`, "detect 不能为空"},
		{"env 里写了密钥字面量", "leak.json", `{"id":"leak","name":"x","description":"y","detect":{"binaries":["x"]},"env":{"base_url":"https://gw.example.com","api_key":"sk-secret"},"restart_note":"z"}`, "必须是环境变量名"},
		{"非法 format", "badfmt.json", `{"id":"badfmt","name":"x","description":"y","detect":{"binaries":["x"]},"files":[{"path":"~/.x/c.json","format":"ini","fields":[{"path":"a","from":"base_url"}]}],"restart_note":"z"}`, "format"},
		{"非法 from", "badfrom.json", `{"id":"badfrom","name":"x","description":"y","detect":{"binaries":["x"]},"files":[{"path":"~/.x/c.json","format":"json","fields":[{"path":"a","from":"env"}]}],"restart_note":"z"}`, "from"},
		{"literal 缺 value", "nolit.json", `{"id":"nolit","name":"x","description":"y","detect":{"binaries":["x"]},"files":[{"path":"~/.x/c.json","format":"json","fields":[{"path":"a","from":"literal"}]}],"restart_note":"z"}`, "value 不能为空"},
		{"非 literal 带 value", "withval.json", `{"id":"withval","name":"x","description":"y","detect":{"binaries":["x"]},"files":[{"path":"~/.x/c.json","format":"json","fields":[{"path":"a","from":"base_url","value":"v"}]}],"restart_note":"z"}`, "只允许与 from=literal 搭配"},
		{"相对路径", "relpath.json", `{"id":"relpath","name":"x","description":"y","detect":{"paths":["some/rel"]},"env":{"base_url":"OPENAI_BASE_URL"},"restart_note":"z"}`, "必须是绝对路径"},
		{"目录穿越", "trav.json", `{"id":"trav","name":"x","description":"y","detect":{"paths":["~/.x/../../etc/passwd"]},"env":{"base_url":"OPENAI_BASE_URL"},"restart_note":"z"}`, ".. 目录穿越"},
		{"点号路径空段", "dotpath.json", `{"id":"dotpath","name":"x","description":"y","detect":{"binaries":["x"]},"files":[{"path":"~/.x/c.json","format":"json","fields":[{"path":"a..b","from":"base_url"}]}],"restart_note":"z"}`, "空段"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := writeSpec(t, UserDir(home), tc.file, tc.body)
			_, err := Load(home)
			if err == nil {
				t.Fatalf("脏 JSON 没有报错（文件 %s）", path)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("报错信息里没有 %q: %v", tc.wantSub, err)
			}
			if !strings.Contains(err.Error(), tc.file) {
				t.Errorf("报错信息里没有指出是哪个文件: %v", err)
			}
		})
	}
}

// TestValidateRules 一条规则一个反例。
func TestValidateRules(t *testing.T) {
	if err := Validate(validBase()); err != nil {
		t.Fatalf("基准规格应当通过: %v", err)
	}
	withFile := validBase()
	withFile.Files = []Target{{
		Path:   "~/.demo/config.json",
		Format: FormatJSON,
		Create: true,
		Fields: []Field{{Path: "models.0.api_base", From: FromBaseURL}, {Path: "name", From: FromLiteral, Value: "ximo"}},
	}}
	if err := Validate(withFile); err != nil {
		t.Fatalf("带文件目标的规格应当通过: %v", err)
	}

	mutate := func(f func(*Spec)) Spec {
		s := validBase()
		f(&s)
		return s
	}
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"缺 id", mutate(func(s *Spec) { s.ID = "" }), "id 不能为空"},
		{"id 大写", mutate(func(s *Spec) { s.ID = "Demo" }), "不合法"},
		{"缺 name", mutate(func(s *Spec) { s.Name = "  " }), "name 不能为空"},
		{"缺 description", mutate(func(s *Spec) { s.Description = "" }), "description 不能为空"},
		{"docs 不是链接", mutate(func(s *Spec) { s.Docs = "docs/readme.md" }), "http(s) 链接"},
		{"detect 为空", mutate(func(s *Spec) { s.Detect = Detect{} }), "detect 不能为空"},
		{"detect.env 非法", mutate(func(s *Spec) { s.Detect = Detect{Env: []string{"1BAD"}} }), "不是合法的环境变量名"},
		{"缺 restart_note", mutate(func(s *Spec) { s.RestartNote = "" }), "restart_note 不能为空"},
		{"protocol 不在白名单", mutate(func(s *Spec) { s.Protocols = []string{"grpc"} }), "不在白名单"},
		{"没有任何动作", mutate(func(s *Spec) { s.Env = EnvSpec{} }), "没有任何可执行动作"},
		{"env.base_url 写成 URL", mutate(func(s *Spec) { s.Env.BaseURL = "https://gw.example.com/v1" }), "必须是环境变量名"},
		{"env.style 非法", mutate(func(s *Spec) { s.Env.Style = "fish" }), "不在白名单"},
		{"env.extra 非法", mutate(func(s *Spec) { s.Env.Extra = []string{"a b"} }), "不是合法的环境变量名"},
		{"format 大写", mutate(func(s *Spec) {
			s.Files = []Target{{Path: "~/.demo/c.json", Format: "JSON", Fields: []Field{{Path: "a", From: FromBaseURL}}}}
		}), "不在白名单"},
		{"files 没有 fields", mutate(func(s *Spec) {
			s.Files = []Target{{Path: "~/.demo/c.json", Format: FormatJSON}}
		}), "fields 不能为空"},
		{"files 路径重复", mutate(func(s *Spec) {
			s.Files = []Target{
				{Path: "~/.demo/c.json", Format: FormatJSON, Fields: []Field{{Path: "a", From: FromBaseURL}}},
				{Path: "~/.demo/c.json", Format: FormatYAML, Fields: []Field{{Path: "b", From: FromBaseURL}}},
			}
		}), "重复"},
		{"字段路径重复", mutate(func(s *Spec) {
			s.Files = []Target{{Path: "~/.demo/c.json", Format: FormatJSON, Fields: []Field{
				{Path: "a.b", From: FromBaseURL}, {Path: "a.b", From: FromModel},
			}}}
		}), "重复"},
		{"~user 不支持", mutate(func(s *Spec) { s.Detect = Detect{Paths: []string{"~other/x"}} }), "必须是绝对路径"},
		{"路径含 NUL", mutate(func(s *Spec) { s.Detect = Detect{Paths: []string{"~/\x00x"}} }), "NUL"},
		{"点号路径段非法", mutate(func(s *Spec) {
			s.Files = []Target{{Path: "~/.demo/c.json", Format: FormatJSON, Fields: []Field{{Path: "a.b c", From: FromBaseURL}}}}
		}), "含非法字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.spec)
			if err == nil {
				t.Fatal("应当报错，但通过了校验")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("报错信息里没有 %q: %v", tc.want, err)
			}
		})
	}
}

// TestAddAgentWithoutCodeChange 走一遍「加一个新 Agent 不用改代码」的完整链路：
// 复制一份规格 → 丢进 ~/.ximo-plugin/specs/<id>.json → Load 能拿到 → 它的
// detect 证据 / 文件目标经 ExpandPath 展开后，指向测试 home 里真实存在的位置。
func TestAddAgentWithoutCodeChange(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, ".my-cli", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"providers":[{}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	writeSpec(t, UserDir(home), "my-cli.json", `{
	  "id": "my-cli",
	  "name": "My CLI",
	  "description": "用户自己加的适配器；schema 需用户确认。",
	  "detect": {"binaries": ["my-cli-not-installed"], "paths": ["~/.my-cli/config.json"]},
	  "files": [{
	    "path": "~/.my-cli/config.json",
	    "format": "json",
	    "create": false,
	    "fields": [
	      {"path": "providers.0.base_url", "from": "base_url"},
	      {"path": "providers.0.api_key", "from": "api_key"}
	    ]
	  }],
	  "restart_note": "改完重开 my-cli。",
	  "protocols": ["openai-chat"]
	}`)

	specs, err := Load(home)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	s, ok := Get(specs, "my-cli")
	if !ok {
		t.Fatalf("新加的适配器没进规格集: %v", IDs(specs))
	}

	// detect 证据必须能落到真实文件（这里 binary 故意写成不存在的，只有 path 命中）。
	for _, raw := range s.Detect.Paths {
		got, err := ExpandPath(raw, home)
		if err != nil {
			t.Fatalf("ExpandPath(%q) 失败: %v", raw, err)
		}
		if _, err := os.Stat(got); err != nil {
			t.Errorf("detect 证据 %q 展开成 %q，但文件不存在: %v", raw, got, err)
		}
	}
	// 文件目标必须落在测试 home 内（防止规格把东西写到真实用户目录）。
	for _, tgt := range s.Files {
		got, err := ExpandPath(tgt.Path, home)
		if err != nil {
			t.Fatalf("ExpandPath(%q) 失败: %v", tgt.Path, err)
		}
		if !strings.HasPrefix(got, filepath.Clean(home)) {
			t.Errorf("目标路径 %q 逃出了 home %q", got, home)
		}
	}
}

func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	// 注意：Windows 上 "/fake/appdata" 这类盘符相对路径并不是绝对路径，
	// 所以这里用具真实盘符的绝对路径当环境变量的值。
	appdata := filepath.Join(t.TempDir(), "appdata")
	testDir := filepath.Join(t.TempDir(), "ximo-test")
	absFile := filepath.Join(t.TempDir(), "path.json")
	t.Setenv("APPDATA", appdata)
	t.Setenv("XIMO_TEST_DIR", testDir)

	ok := []struct {
		raw  string
		want string
	}{
		{"~/a/b.json", filepath.Join(home, "a", "b.json")},
		{"~", home},
		{"%APPDATA%/ximo-agent/config.json", filepath.Join(appdata, "ximo-agent", "config.json")},
		{"$XIMO_TEST_DIR/f.json", filepath.Join(testDir, "f.json")},
		{"${XIMO_TEST_DIR}/f.json", filepath.Join(testDir, "f.json")},
		{absFile, absFile},
	}
	for _, tc := range ok {
		got, err := ExpandPath(tc.raw, home)
		if err != nil {
			t.Errorf("ExpandPath(%q) 报错: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ExpandPath(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	// 反向控制：未定义变量必须报错，而不是展开成空串后变成 "/config.json"。
	bad := []string{
		"~/%XIMO_PLUGIN_TEST_UNSET_9c1f%/config.json",
		"~/$XIMO_PLUGIN_TEST_UNSET_9c1f/config.json",
		"~other/x",
		"relative/path.json",
		"",
	}
	for _, raw := range bad {
		if got, err := ExpandPath(raw, home); err == nil {
			t.Errorf("ExpandPath(%q) 应当报错，却返回 %q", raw, got)
		}
	}
}

// TestUserDirEmptyHomeFallsBack 确认 home 为空时退化为真实用户主目录。
func TestUserDirEmptyHomeFallsBack(t *testing.T) {
	got := UserDir("")
	if !strings.HasSuffix(filepath.ToSlash(got), "/.ximo-plugin/specs") {
		t.Fatalf("UserDir(\"\") = %q，不是 ~/.ximo-plugin/specs", got)
	}
	if strings.HasPrefix(got, "/.ximo-plugin") || strings.HasPrefix(got, `\.ximo-plugin`) {
		t.Fatalf("UserDir(\"\") 退化成了根目录: %q", got)
	}
}
