package formats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// dotenvCodec reads and writes KEY=VALUE files. Rendering patches the original
// lines, so comments, blank lines, key order and each line's quoting style all
// survive: only lines whose value actually changes are rewritten.
type dotenvCodec struct{}

// dotenvLine is one parsed KEY=VALUE line. prefix, quote and suffix remember how
// the line was written so a changed value can be put back in the same style.
type dotenvLine struct {
	prefix string // "export " or ""
	key    string
	value  string
	quote  byte   // '"', '\'' or 0 for an unquoted value
	suffix string // trailing comment, including the whitespace before it
}

func (dotenvCodec) parse(raw []byte) (map[string]any, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return doc, nil
	}
	for i, line := range strings.Split(string(raw), "\n") {
		parsed, ok, err := parseDotenvLine(line, i+1)
		if err != nil {
			return nil, err
		}
		if ok {
			doc[parsed.key] = parsed.value
		}
	}
	return doc, nil
}

func (dotenvCodec) render(doc map[string]any, raw []byte) ([]byte, error) {
	var lines []string
	if len(raw) > 0 {
		lines = strings.Split(string(raw), "\n")
	}
	seen := map[string]bool{}
	for i, line := range lines {
		parsed, ok, err := parseDotenvLine(line, i+1)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		value, wanted := doc[parsed.key]
		if !wanted {
			continue
		}
		seen[parsed.key] = true
		if stringifyDotenv(value) == parsed.value {
			continue
		}
		lines[i] = parsed.prefix + parsed.key + "=" + renderDotenvValue(value, parsed.quote) + parsed.suffix
	}

	var added []string
	for _, key := range sortedKeys(doc) {
		if seen[key] {
			continue
		}
		if !validDotenvKey(key) {
			return nil, fmt.Errorf("dotenv: refusing to write invalid variable name %q", key)
		}
		added = append(added, key+"="+renderDotenvValue(doc[key], 0))
	}
	if len(added) > 0 {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, added...)
		} else {
			lines = added
		}
		lines = append(lines, "")
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func parseDotenvLine(line string, number int) (dotenvLine, bool, error) {
	body := strings.TrimSpace(line)
	if body == "" || strings.HasPrefix(body, "#") {
		return dotenvLine{}, false, nil
	}
	out := dotenvLine{}
	if rest, ok := trimExportPrefix(body); ok {
		out.prefix = "export "
		body = rest
	}
	eq := strings.IndexByte(body, '=')
	if eq < 0 {
		return out, false, fmt.Errorf("dotenv: line %d: expected KEY=VALUE, got %q", number, body)
	}
	out.key = strings.TrimSpace(body[:eq])
	if !validDotenvKey(out.key) {
		return out, false, fmt.Errorf("dotenv: line %d: invalid variable name %q", number, out.key)
	}
	value, quote, suffix, err := splitDotenvValue(strings.TrimSpace(body[eq+1:]), number)
	if err != nil {
		return out, false, err
	}
	out.value, out.quote, out.suffix = value, quote, suffix
	return out, true, nil
}

// trimExportPrefix strips a leading "export" when it is the shell keyword and
// not part of the variable name (as in "exportFOO=1").
func trimExportPrefix(body string) (string, bool) {
	const keyword = "export"
	if !strings.HasPrefix(body, keyword) || len(body) == len(keyword) {
		return body, false
	}
	if c := body[len(keyword)]; c != ' ' && c != '\t' {
		return body, false
	}
	rest := strings.TrimSpace(body[len(keyword):])
	if !strings.Contains(rest, "=") {
		return body, false
	}
	return rest, true
}

// splitDotenvValue unquotes a value and returns whatever followed it (an inline
// comment, kept verbatim so it survives a rewrite).
func splitDotenvValue(s string, number int) (string, byte, string, error) {
	if s == "" {
		return "", 0, "", nil
	}
	switch s[0] {
	case '"':
		var b strings.Builder
		i := 1
		for ; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case '"':
					b.WriteByte('"')
				case '\\':
					b.WriteByte('\\')
				default:
					b.WriteByte('\\')
					b.WriteByte(s[i])
				}
				continue
			}
			if s[i] == '"' {
				break
			}
			b.WriteByte(s[i])
		}
		if i >= len(s) {
			return "", 0, "", fmt.Errorf("dotenv: line %d: unterminated double-quoted value", number)
		}
		suffix := s[i+1:]
		if err := checkTrailing(suffix, number); err != nil {
			return "", 0, "", err
		}
		return b.String(), '"', suffix, nil
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", 0, "", fmt.Errorf("dotenv: line %d: unterminated single-quoted value", number)
		}
		suffix := s[end+2:]
		if err := checkTrailing(suffix, number); err != nil {
			return "", 0, "", err
		}
		return s[1 : 1+end], '\'', suffix, nil
	}
	value := strings.TrimRight(s, " \t")
	if i := inlineCommentStart(value); i >= 0 {
		value = strings.TrimRight(value[:i], " \t")
	}
	return value, 0, s[len(value):], nil
}

// checkTrailing reports an error when text follows a quoted value that is not a
// comment.
func checkTrailing(suffix string, number int) error {
	if rest := strings.TrimSpace(suffix); rest != "" && !strings.HasPrefix(rest, "#") {
		return fmt.Errorf("dotenv: line %d: unexpected text after a quoted value: %q", number, rest)
	}
	return nil
}

// inlineCommentStart returns the index of a "#" that starts a comment, which
// requires whitespace before it. Without one the "#" is part of the value.
func inlineCommentStart(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}

func validDotenvKey(key string) bool {
	if key == "" || !isKeyStart(key[0]) {
		return false
	}
	for i := 1; i < len(key); i++ {
		c := key[i]
		if !isKeyStart(c) && (c < '0' || c > '9') && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

func isKeyStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func renderDotenvValue(value any, quote byte) string {
	s := stringifyDotenv(value)
	switch quote {
	case '\'':
		if !strings.ContainsAny(s, "'\n") {
			return "'" + s + "'"
		}
	case '"':
		return doubleQuoteDotenv(s)
	}
	if s == "" || strings.ContainsAny(s, " \t\n\r\"'#\\") {
		return doubleQuoteDotenv(s)
	}
	return s
}

func doubleQuoteDotenv(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	)
	return "\"" + r.Replace(s) + "\""
}

func stringifyDotenv(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return string(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return fmt.Sprint(v)
	}
}
