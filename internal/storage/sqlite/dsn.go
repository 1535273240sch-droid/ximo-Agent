package sqlite

import (
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// buildDSN 构造 modernc.org/sqlite 的 DSN。
//
// 格式：file:<escaped-path>?_pragma=...&_pragma=...&_txlock=immediate
//
// 路径部分按 SQLite URI 规则转义（% ? #），否则含这些字符的路径会被
// url.ParseQuery 截断。POSIX 分隔符统一为 "/"（Windows 下 SQLite URI
// 同样接受正斜杠）。
func buildDSN(path string, pragmas []string, txlock bool) string {
	var q []string
	for _, p := range pragmas {
		q = append(q, "_pragma="+url.QueryEscape(p))
	}
	if txlock {
		q = append(q, "_txlock=immediate")
	}
	dsn := "file:" + escapePath(path)
	if len(q) > 0 {
		dsn += "?" + strings.Join(q, "&")
	}
	return dsn
}

func escapePath(path string) string {
	if path == ":memory:" || strings.HasPrefix(path, "file::") {
		return path
	}
	// % 必须最先转义，否则后续转义产生的 % 会被二次解释。
	r := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")
	return r.Replace(filepath.ToSlash(path))
}

// commonPragmas 返回所有连接共享的 PRAGMA 列表（第10.2章）。
func commonPragmas(spec profileSpec) []string {
	return []string{
		"journal_mode(WAL)",
		"synchronous(" + spec.synchronous + ")",
		"foreign_keys(ON)",
		"busy_timeout(" + strconv.Itoa(spec.busyTimeoutMS) + ")",
		"wal_autocheckpoint(" + strconv.Itoa(spec.walAutocheckpoint) + ")",
		"journal_size_limit(" + strconv.Itoa(journalSizeLimit) + ")",
	}
}
