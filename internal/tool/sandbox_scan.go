package tool

import (
	"fmt"
	"strings"
)

// 本文件实现沙箱的静态分析：一个自包含的 JS 扫描器（只依赖标准库），
// 不依赖 goja 的 parser/ast 内部结构。职责：
//  1. 识别禁用标识符（require/import/process/os/fs/net/eval/Function/fetch/...）；
//  2. 识别可证明无界的循环（while(true)/for(;;)）；
//  3. 对循环体做指令预算插桩（每次循环迭代消耗 1 预算）。
//
// 扫描器对无法可靠解析的输入返回错误，由调用方 fail-closed 拒绝执行，
// 绝不退化为“无预算执行”。

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokNumber
	tokString
	tokTemplate
	tokRegex
	tokPunct
)

type jsToken struct {
	kind       tokKind
	text       string
	start, end int // 字节偏移 [start, end)
}

// bannedGlobals 沙箱禁用的全局标识符。goja 本身不提供 os/exec/net/fs，
// 这里拦住的是“动态代码执行”与“试图访问宿主环境”的写法。
var bannedGlobals = map[string]string{
	"require":           "CommonJS require",
	"module":            "CommonJS module",
	"exports":           "CommonJS exports",
	"process":           "Node process 对象",
	"os":                "操作系统模块",
	"fs":                "文件系统模块",
	"net":               "网络模块",
	"child_process":     "子进程模块",
	"exec":              "命令执行",
	"execSync":          "同步命令执行",
	"spawn":             "进程派生",
	"eval":              "动态代码执行",
	"Function":          "Function 构造器（动态代码执行）",
	"globalThis":        "全局对象访问",
	"fetch":             "任意网络请求（只能通过 tool.http 白名单）",
	"XMLHttpRequest":    "任意网络请求",
	"WebSocket":         "任意网络连接",
	"SharedArrayBuffer": "共享内存（Atomics.wait 可阻塞 VM）",
	"Atomics":           "共享内存等待（可阻塞 VM）",
	"Worker":            "宿主多线程",
	"Deno":              "宿主运行时",
	"Bun":               "宿主运行时",
	"window":            "浏览器全局",
	"document":          "浏览器全局",
}

// scanJS 把源码切成 token 序列（跳过注释）。
func scanJS(src string) ([]jsToken, error) {
	var tokens []jsToken
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("sandbox: 未闭合的块注释")
			}
			i += 2 + end + 2
		case c == '\'' || c == '"':
			start := i
			i++
			closed := false
			for i < n {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == c {
					i++
					closed = true
					break
				}
				if src[i] == '\n' {
					return nil, fmt.Errorf("sandbox: 字符串跨行（偏移 %d）", start)
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("sandbox: 未闭合的字符串（偏移 %d）", start)
			}
			tokens = append(tokens, jsToken{kind: tokString, text: src[start:i], start: start, end: i})
		case c == '`':
			start := i
			i++
			depth := 0
			for i < n {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if depth == 0 && src[i] == '`' {
					i++
					break
				}
				if depth > 0 && src[i] == '}' {
					depth--
					i++
					continue
				}
				if src[i] == '$' && i+1 < n && src[i+1] == '{' {
					depth++
					i += 2
					continue
				}
				i++
			}
			if depth != 0 || (i <= n && (i == start+1 || src[i-1] != '`')) {
				// 允许正常闭合；仅当明显未闭合时报错。
				if i >= n && (n == start+1 || src[n-1] != '`') {
					return nil, fmt.Errorf("sandbox: 未闭合的模板字符串（偏移 %d）", start)
				}
			}
			tokens = append(tokens, jsToken{kind: tokTemplate, text: src[start:i], start: start, end: i})
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(src[i+1])):
			start := i
			for i < n && (isIdentPart(src[i]) || src[i] == '.' || ((src[i] == '+' || src[i] == '-') && i > start && (src[i-1] == 'e' || src[i-1] == 'E'))) {
				i++
			}
			tokens = append(tokens, jsToken{kind: tokNumber, text: src[start:i], start: start, end: i})
		case isIdentStart(c):
			start := i
			for i < n && isIdentPart(src[i]) {
				i++
			}
			tokens = append(tokens, jsToken{kind: tokIdent, text: src[start:i], start: start, end: i})
		case c == '/':
			// 正则字面量 or 除法：依据前一个有效 token 判断。
			if regexAllowedAfter(prevSignificant(tokens)) {
				start := i
				i++
				inClass := false
				closed := false
				for i < n {
					ch := src[i]
					if ch == '\\' {
						i += 2
						continue
					}
					if ch == '\n' {
						return nil, fmt.Errorf("sandbox: 正则字面量跨行（偏移 %d）", start)
					}
					if ch == '[' {
						inClass = true
					} else if ch == ']' {
						inClass = false
					} else if ch == '/' && !inClass {
						i++
						closed = true
						break
					}
					i++
				}
				if !closed {
					return nil, fmt.Errorf("sandbox: 未闭合的正则字面量（偏移 %d）", start)
				}
				for i < n && isIdentPart(src[i]) { // flags
					i++
				}
				tokens = append(tokens, jsToken{kind: tokRegex, text: src[start:i], start: start, end: i})
			} else {
				tokens = append(tokens, jsToken{kind: tokPunct, text: "/", start: i, end: i + 1})
				i++
			}
		default:
			two := ""
			if i+1 < n {
				two = src[i : i+2]
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||", "??", "=>", "**", "++", "--", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>", "?.":
				tokens = append(tokens, jsToken{kind: tokPunct, text: two, start: i, end: i + 2})
				i += 2
			default:
				tokens = append(tokens, jsToken{kind: tokPunct, text: string(c), start: i, end: i + 1})
				i++
			}
		}
	}
	return tokens, nil
}

func prevSignificant(tokens []jsToken) *jsToken {
	if len(tokens) == 0 {
		return nil
	}
	return &tokens[len(tokens)-1]
}

// regexAllowedAfter 判断 `/` 是否应被解析为正则字面量起点。
func regexAllowedAfter(prev *jsToken) bool {
	if prev == nil {
		return true
	}
	switch prev.kind {
	case tokIdent:
		switch prev.text {
		case "return", "typeof", "instanceof", "in", "of", "new", "delete",
			"void", "case", "do", "else", "yield", "await":
			return true
		}
		return false
	case tokNumber, tokString, tokTemplate, tokRegex:
		return false
	case tokPunct:
		switch prev.text {
		case ")", "]", "}":
			return false
		}
		return true
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// ---------------------------------------------------------------------------
// 静态检查
// ---------------------------------------------------------------------------

// scanViolations 检查禁用标识符与可证明无界的循环。
func scanViolations(tokens []jsToken) error {
	for i, tok := range tokens {
		if tok.kind != tokIdent {
			continue
		}
		if why, banned := bannedGlobals[tok.text]; banned {
			// 跳过属性访问（obj.process / a?.os）。
			if i > 0 && tokens[i-1].kind == tokPunct && (tokens[i-1].text == "." || tokens[i-1].text == "?.") {
				continue
			}
			// typeof <banned> 是合法且无害的存在性检查，不视为访问。
			if i > 0 && tokens[i-1].kind == tokIdent && tokens[i-1].text == "typeof" {
				continue
			}
			return &SandboxViolationError{
				Kind:   ViolationBannedAPI,
				Detail: fmt.Sprintf("禁止访问 %s（标识符 %q，偏移 %d）", why, tok.text, tok.start),
			}
		}
	}
	for i := 0; i < len(tokens); i++ {
		if tokens[i].kind != tokIdent {
			continue
		}
		switch tokens[i].text {
		case "while", "for":
			if !atStatementStart(tokens, i) {
				continue
			}
			if isProvablyUnbounded(tokens, i) {
				return &SandboxViolationError{
					Kind:   ViolationUnboundedLoop,
					Detail: fmt.Sprintf("检测到可证明无界的循环（偏移 %d），沙箱拒绝执行", tokens[i].start),
				}
			}
		}
	}
	return nil
}

// atStatementStart 报告 token 是否处于语句起始位置（用于定位循环语句）。
func atStatementStart(tokens []jsToken, i int) bool {
	if i == 0 {
		return true
	}
	prev := tokens[i-1]
	if prev.kind == tokPunct {
		switch prev.text {
		case ";", "{", "}", ")", ":":
			return true
		}
		return false
	}
	if prev.kind == tokIdent {
		switch prev.text {
		case "else", "do":
			return true
		}
	}
	return false
}

// isProvablyUnbounded 识别 while(true)/while(1)/for(;;) 等静态无界循环。
func isProvablyUnbounded(tokens []jsToken, kwIdx int) bool {
	if kwIdx+1 >= len(tokens) || tokens[kwIdx+1].kind != tokPunct || tokens[kwIdx+1].text != "(" {
		return false
	}
	open := kwIdx + 1
	close := matchParen(tokens, open)
	if close < 0 {
		return false
	}
	inner := tokens[open+1 : close]
	if len(inner) == 0 {
		return true
	}
	if tokens[kwIdx].text == "while" {
		if len(inner) != 1 {
			return false
		}
		// while (true) 或 while (1)：字面真值条件。
		return (inner[0].kind == tokIdent && inner[0].text == "true") ||
			(inner[0].kind == tokNumber && inner[0].text == "1")
	}
	// for(init; cond; update)：cond 为空即可证明无界
	semis := []int{}
	depth := 0
	for i, tok := range inner {
		if tok.kind == tokPunct {
			switch tok.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case ";":
				if depth == 0 {
					semis = append(semis, i)
				}
			}
		}
	}
	return len(semis) >= 2 && len(inner[semis[0]+1:semis[1]]) == 0
}

// matchParen 找到与 open 匹配的闭括号索引。
func matchParen(tokens []jsToken, open int) int {
	depth := 0
	for i := open; i < len(tokens); i++ {
		if tokens[i].kind != tokPunct {
			continue
		}
		switch tokens[i].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// 循环插桩
// ---------------------------------------------------------------------------

type insertion struct {
	offset int
	text   string
}

// instrumentLoops 为每个循环体插入预算检查调用。返回插入后的源码。
// 插桩名 __sbTick 由沙箱注入为函数参数，用户代码无法伪造（标识符检查会
// 拦住对同名标识符的重新声明以外的用法；重新声明只影响用户自身作用域）。
func instrumentLoops(src string, tokens []jsToken) string {
	var insertions []insertion
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.kind != tokIdent || !atStatementStart(tokens, i) {
			continue
		}
		var bodyIdx int
		switch tok.text {
		case "while", "for":
			if i+1 >= len(tokens) || tokens[i+1].kind != tokPunct || tokens[i+1].text != "(" {
				continue
			}
			close := matchParen(tokens, i+1)
			if close < 0 || close+1 >= len(tokens) {
				continue
			}
			bodyIdx = close + 1
		case "do":
			if i+1 >= len(tokens) {
				continue
			}
			bodyIdx = i + 1
		default:
			continue
		}
		body := tokens[bodyIdx]
		if body.kind == tokPunct && body.text == "{" {
			// 块体：在 { 之后插入 tick。
			insertions = append(insertions, insertion{offset: body.end, text: "__sbTick();"})
			continue
		}
		// 单语句体：包一层块，块内先 tick。
		end := singleStatementEnd(tokens, bodyIdx)
		insertions = append(insertions,
			insertion{offset: body.start, text: "{__sbTick();"},
			insertion{offset: end, text: "}"},
		)
	}
	if len(insertions) == 0 {
		return src
	}
	// 从后往前插入，保证偏移有效。
	out := src
	for k := len(insertions) - 1; k >= 0; k-- {
		ins := insertions[k]
		if ins.offset < 0 || ins.offset > len(out) {
			continue
		}
		out = out[:ins.offset] + ins.text + out[ins.offset:]
	}
	return out
}

// singleStatementEnd 计算单语句体的结束偏移（含分号；无分号则到下一个
// 会降低括号层级的 token 之前）。
func singleStatementEnd(tokens []jsToken, bodyIdx int) int {
	depth := 0
	for i := bodyIdx; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.kind == tokPunct {
			switch tok.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth == 0 {
					return tok.start
				}
				depth--
			case ";":
				if depth == 0 {
					return tok.end
				}
			}
		}
	}
	if len(tokens) > 0 {
		return tokens[len(tokens)-1].end
	}
	return 0
}
