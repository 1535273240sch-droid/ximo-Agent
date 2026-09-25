package tool

import (
	"strings"
	"testing"
)

func TestScanJSTokenizesTrickySources(t *testing.T) {
	cases := []string{
		`const s = "while (true) { }"; // while 在字符串里`,
		`/* while (true) { } */ let x = 1;`,
		`const re = /while \(true\)/g; let y = 2;`,
		"const tpl = `while (true) { ${1 + 2} }`;",
		`const d = a / b / c;`,
		`for (const x of [1,2]) { if (x) continue; }`,
		`label: for (;;) { break label; }`,
		`do { x++; } while (x < 10);`,
		`while (c) doThing();`,
		`const obj = { while: 1, for: 2 };`,
	}
	for _, src := range cases {
		if _, err := scanJS(src); err != nil {
			t.Fatalf("scanJS(%q) 失败: %v", src, err)
		}
	}
}

func TestScanJSRejectsUnterminated(t *testing.T) {
	bad := []string{
		`const s = "unterminated`,
		"const t = `unterminated",
		`/* never closed`,
	}
	for _, src := range bad {
		if _, err := scanJS(src); err == nil {
			t.Fatalf("scanJS(%q) 应当报错", src)
		}
	}
}

func TestInstrumentLoopsInjectsTick(t *testing.T) {
	src := `let n = 0;
for (let i = 0; i < 10; i++) {
	n += i;
}
while (n < 5) { n++; }
do { n--; } while (n > 0);
return n;`
	tokens, err := scanJS(src)
	if err != nil {
		t.Fatal(err)
	}
	out := instrumentLoops(src, tokens)
	// 每个循环体都应被插入 __sbTick()
	if got := strings.Count(out, "__sbTick()"); got != 4 {
		t.Fatalf("__sbTick() 出现 %d 次，期望 4 次\n%s", got, out)
	}
}

func TestInstrumentLoopsSingleStatementBody(t *testing.T) {
	src := `while (c) doThing();
return 1;`
	tokens, err := scanJS(src)
	if err != nil {
		t.Fatal(err)
	}
	out := instrumentLoops(src, tokens)
	if !strings.Contains(out, "{__sbTick();doThing();}") {
		t.Fatalf("单语句循环体未被正确包裹:\n%s", out)
	}
}

func TestInstrumentLoopsSkipsStringsAndComments(t *testing.T) {
	src := `const s = "for (;;) { break; }";
// while (true) { }
/* for (;;) { } */
const re = /while \(true\)/;
return s + re;`
	tokens, err := scanJS(src)
	if err != nil {
		t.Fatal(err)
	}
	out := instrumentLoops(src, tokens)
	if strings.Contains(out, "__sbTick") {
		t.Fatalf("字符串/注释/正则中的循环被误插桩:\n%s", out)
	}
}

func TestInstrumentLoopsNested(t *testing.T) {
	src := `for (let i = 0; i < 3; i++) {
	for (let j = 0; j < 3; j++) {
		if (i === j) continue;
	}
}`
	tokens, err := scanJS(src)
	if err != nil {
		t.Fatal(err)
	}
	out := instrumentLoops(src, tokens)
	if got := strings.Count(out, "__sbTick()"); got != 2 {
		t.Fatalf("嵌套循环插桩次数 = %d，期望 2\n%s", got, out)
	}
}

func TestScanViolationsSkipsPropertyAccess(t *testing.T) {
	// obj.process 是属性访问，不是全局引用
	if err := scanViolations(mustScan(t, `const x = obj.process; return x;`)); err != nil {
		t.Fatalf("属性访问被误判: %v", err)
	}
	// process 单独出现必须拒绝
	if err := scanViolations(mustScan(t, `return process;`)); err == nil {
		t.Fatalf("全局 process 应当被拒绝")
	}
}

func TestScanViolationsAllowsTypeof(t *testing.T) {
	if err := scanViolations(mustScan(t, `return typeof fetch === "undefined";`)); err != nil {
		t.Fatalf("typeof 存在性检查被误判: %v", err)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, text string
		want          bool
	}{
		{"", "", true},
		{"*", "anything", true},
		{"file_*", "file_read", true},
		{"file_*", "git_log", false},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "example.com", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"rm -rf *", "rm -rf /tmp/x", true},
		{"git_operations:push", "git_operations:push", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.text); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v，期望 %v", c.pattern, c.text, got, c.want)
		}
	}
}

func TestMatchPort(t *testing.T) {
	cases := []struct {
		spec string
		port int
		want bool
	}{
		{"", 80, true},
		{"*", 9999, true},
		{"443", 443, true},
		{"443", 444, false},
		{"80,443", 443, true},
		{"8000-9000", 8500, true},
		{"8000-9000", 9001, false},
	}
	for _, c := range cases {
		if got := matchPort(c.spec, c.port); got != c.want {
			t.Errorf("matchPort(%q, %d) = %v，期望 %v", c.spec, c.port, got, c.want)
		}
	}
}

func mustScan(t *testing.T, src string) []jsToken {
	t.Helper()
	tokens, err := scanJS(src)
	if err != nil {
		t.Fatalf("scanJS(%q) 失败: %v", src, err)
	}
	return tokens
}
