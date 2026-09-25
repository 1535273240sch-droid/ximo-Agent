// Package domains 提供纯 Go、轻量的工具域实现（file/git/knowledge/web）。
// 高风险工具域（browser/terminal/office/dynamic-js/mcp/computer-use）不在
// 本包范围——它们由任务05 的 Worker 承载，本包只提供进程内安全工具。
package domains

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

// Guard 是文件与网络访问的安全守卫，对应 v1 的 src/main/security-guard.ts：
// 写入白名单、敏感凭据文件拦截、SSRF 防护。
type Guard struct {
	// AllowedWriteRoots 允许写入的根目录；为空时默认允许（与 v1 一致，
	// 生产环境由 Supervisor 启动时注入项目路径）。
	AllowedWriteRoots []string
	// SensitivePatterns 敏感文件模式（凭据/私钥）。
	SensitivePatterns []*regexp.Regexp
}

// DefaultSensitivePatterns 从 v1 SENSITIVE_FILE_PATTERNS 平移。
var DefaultSensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\.ssh[\\/]`),
	regexp.MustCompile(`(?i)id_rsa`),
	regexp.MustCompile(`(?i)id_ecdsa`),
	regexp.MustCompile(`(?i)id_ed25519`),
	regexp.MustCompile(`(?i)\.gnupg[\\/]`),
	regexp.MustCompile(`(?i)\.env$`),
	regexp.MustCompile(`(?i)\.env\.`),
	regexp.MustCompile(`(?i)\.npmrc$`),
	regexp.MustCompile(`(?i)\.pypirc$`),
	regexp.MustCompile(`(?i)\.netrc$`),
	regexp.MustCompile(`(?i)_netrc$`),
	regexp.MustCompile(`(?i)credentials\.json$`),
	regexp.MustCompile(`(?i)cookies\.txt$`),
	regexp.MustCompile(`(?i)\.key$`),
	regexp.MustCompile(`(?i)\.pem$`),
	regexp.MustCompile(`(?i)\.pfx$`),
	regexp.MustCompile(`(?i)\.keystore$`),
	regexp.MustCompile(`(?i)\.kdbx$`),
	regexp.MustCompile(`(?i)kube[\\/]?config`),
	regexp.MustCompile(`(?i)\.docker[\\/]config\.json$`),
	regexp.MustCompile(`(?i)\.aws[\\/]credentials$`),
	regexp.MustCompile(`(?i)\.aws[\\/]config$`),
}

// NewGuard 创建安全守卫。
func NewGuard(allowedWriteRoots []string) *Guard {
	roots := make([]string, 0, len(allowedWriteRoots))
	for _, r := range allowedWriteRoots {
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(r))
		if err != nil {
			continue
		}
		roots = append(roots, abs)
	}
	return &Guard{AllowedWriteRoots: roots, SensitivePatterns: DefaultSensitivePatterns}
}

// CheckWriteAccess 检查路径是否允许写入。
func (g *Guard) CheckWriteAccess(path string) error {
	if len(g.AllowedWriteRoots) == 0 {
		return nil
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	for _, root := range g.AllowedWriteRoots {
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("路径 %q 不在允许写入的目录范围内。允许的目录：%s", abs, strings.Join(g.AllowedWriteRoots, ", "))
}

// CheckSensitiveFile 拦截敏感凭据文件。
func (g *Guard) CheckSensitiveFile(path string) error {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		abs = path
	}
	for _, pattern := range g.SensitivePatterns {
		if pattern.MatchString(abs) {
			return fmt.Errorf("出于安全考虑，不允许访问敏感凭据文件：%q。如需查看内容请用户手动提供", abs)
		}
	}
	return nil
}

// CheckSSRF 禁止访问内网/回环/云元数据端点（从 v1 checkSsrf 平移）。
func CheckSSRF(rawURL string) error {
	host, scheme, err := hostOf(rawURL)
	if err != nil {
		return fmt.Errorf("SSRF 防护：URL 解析失败 %q", rawURL)
	}
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("SSRF 防护：仅允许 http/https，不允许 %q", scheme)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch host {
	case "169.254.169.254", "fd00:ec2::254", "metadata.google.internal", "metadata.azure.com":
		return fmt.Errorf("SSRF 防护：禁止访问云元数据端点 %q", host)
	case "localhost", "ip6-localhost", "ip6-loopback", "broadcasthost":
		return fmt.Errorf("SSRF 防护：禁止访问内网地址 %q", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("SSRF 防护：禁止访问内网地址 %q", host)
		}
	}
	return nil
}

func hostOf(rawURL string) (host, scheme string, err error) {
	// 最小化 URL 解析：只取 scheme 与 host（含 userinfo 剥离、端口剥离）。
	idx := strings.Index(rawURL, "://")
	if idx < 0 {
		return "", "", fmt.Errorf("缺少协议")
	}
	scheme = strings.ToLower(rawURL[:idx])
	rest := rawURL[idx+3:]
	if slash := strings.IndexAny(rest, "/?#"); slash >= 0 {
		rest = rest[:slash]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if strings.HasPrefix(rest, "[") {
		// IPv6 字面量：[::1] 或 [::1]:8080
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return "", "", fmt.Errorf("IPv6 字面量未闭合")
		}
		return rest[1:end], scheme, nil
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		rest = rest[:colon]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", fmt.Errorf("缺少主机")
	}
	return rest, scheme, nil
}

// ---------------------------------------------------------------------------
// 响应构造辅助
// ---------------------------------------------------------------------------

// Text 构造文本响应。
func Text(toolCallID, toolName, content string) tool.ToolResponse {
	return tool.ToolResponse{
		ToolCallID:  toolCallID,
		ToolName:    toolName,
		Content:     content,
		Success:     true,
		DisplayType: "text",
	}
}

// Code 构造代码渲染响应。
func Code(toolCallID, toolName, content string) tool.ToolResponse {
	resp := Text(toolCallID, toolName, content)
	resp.DisplayType = "code"
	return resp
}

// Fail 构造失败响应。
func Fail(toolCallID, toolName, format string, args ...any) tool.ToolResponse {
	return tool.ToolResponse{
		ToolCallID: toolCallID,
		ToolName:   toolName,
		Success:    false,
		Error:      fmt.Sprintf(format, args...),
		ErrorCode:  tool.ErrToolFailed,
	}
}

// StringArg 读取字符串参数。
func StringArg(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// StringArgDefault 读取字符串参数（带默认值）。
func StringArgDefault(args map[string]any, key, def string) string {
	if s := StringArg(args, key); s != "" {
		return s
	}
	return def
}

// IntArg 读取整数参数（JSON 数字解码为 float64）。
func IntArg(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
	}
	return def
}

// BoolArg 读取布尔参数。
func BoolArg(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// StringSliceArg 读取字符串数组参数。
func StringSliceArg(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
