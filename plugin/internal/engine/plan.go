package engine

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/1535273240sch-droid/ximo-plugin/internal/formats"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// 计划步骤的种类。
const (
	KindFile = "file" // 写一个配置文件
	KindEnv  = "env"  // 只提示环境变量，不落盘
	KindIPC  = "ipc"  // 需要走 Agent 自身的进程间接口（由 cmd 侧接线）
)

// ErrMissingAPIKey 表示规格要求写入密钥，但调用方没有提供。
var ErrMissingAPIKey = errors.New("缺少 API Key：请用 --api-key 提供，或先执行 ximo-plugin login 保存凭据")

// Inputs 是生成计划所需的全部输入（来自 CLI flag 与网关探测结果）。
type Inputs struct {
	GatewayURL       string // 中转站地址，例 http://127.0.0.1:8600
	APIKey           string // 明文密钥，仅用于写入目标配置；不会出现在返回的 values 里
	Model            string
	ModelDisplayName string
	Home             string // 用于展开 ~
	// IsolateHome 表示 Home 是本次调用唯一认可的家目录（CLI --home 显式生效）：
	// %APPDATA% / $XIMO_HOME 等也按 Home 解析，见 spec.ExpandOptions。
	IsolateHome bool
}

// expandOptions 返回本计划使用的路径展开上下文。
func (in Inputs) expandOptions() spec.ExpandOptions {
	return spec.ExpandOptions{Home: in.Home, IsolateHome: in.IsolateHome}
}

// PlanStep 描述一项待执行的变更。前四个字段是契约冻结字段；其余为 P-Core 追加的展示/执行细节。
type PlanStep struct {
	SpecID  string
	Kind    string // file | env | ipc
	Target  string // kind=file 是文件绝对路径；kind=env 是变量名列表；kind=ipc 是 Agent 标识
	Summary string

	Format string   // kind=file
	Exists bool     // kind=file：目标当前是否已存在
	Vars   []string // kind=env

	doc map[string]any // kind=file：待写入的完整文档（保留未知字段），只在 engine 内流转
	// secret 是本次要写入的明文密钥（api_key 字段的值）。只在 dry-run 打印差异时用于
	// 把差异文本里的密钥打码，绝不外泄、绝不进日志。
	secret string
}

// Doc 返回待写入文档的副本，供同包内其他逻辑/测试使用；外部包不应依赖它。
func (s PlanStep) Doc() map[string]any { return s.doc }

// BuildPlan 生成「把 s 接进中转站」的变更清单，不接触磁盘以外的副作用（只读取现状用于生成差异）。
// 返回的 values 可用于展示，**不含明文密钥**。
func BuildPlan(s spec.Spec, in Inputs) ([]PlanStep, map[string]any, error) {
	if strings.TrimSpace(in.GatewayURL) == "" {
		return nil, nil, errors.New("缺少 --gateway：无法生成计划")
	}
	gatewayURL := strings.TrimRight(strings.TrimSpace(in.GatewayURL), "/")
	baseURL := normalizeBaseURL(gatewayURL)

	values := map[string]any{
		"spec_id":      s.ID,
		"gateway_url":  gatewayURL,
		"base_url":     baseURL,
		"model":        in.Model,
		"display_name": in.displayName(),
		"api_key":      MaskKey(in.APIKey),
	}

	var steps []PlanStep
	for _, t := range s.Files {
		path, err := spec.ExpandPathOpts(t.Path, in.expandOptions())
		if err != nil {
			return nil, nil, fmt.Errorf("spec %s: 目标路径 %q 无法展开: %w", s.ID, t.Path, err)
		}
		if strings.TrimSpace(t.Format) == "" {
			return nil, nil, fmt.Errorf("spec %s: %s 未声明 format", s.ID, path)
		}
		doc, err := formats.Read(path, t.Format)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 %s 失败: %w", path, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
		exists := true
		if _, err := os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("探测 %s 失败: %w", path, err)
			}
			exists = false
			if !t.Create {
				return nil, nil, fmt.Errorf("spec %s: %s 不存在，且该目标未声明 create=true", s.ID, path)
			}
		}
		paths := make([]string, 0, len(t.Fields))
		for _, f := range t.Fields {
			if strings.TrimSpace(f.Path) == "" {
				return nil, nil, fmt.Errorf("spec %s: %s 有不带 path 的字段规则", s.ID, path)
			}
			v, err := fieldValue(s.ID, f, in, baseURL, gatewayURL)
			if err != nil {
				return nil, nil, err
			}
			if err := formats.SetPath(doc, f.Path, v); err != nil {
				return nil, nil, fmt.Errorf("spec %s: 设置 %s 的 %s 失败: %w", s.ID, path, f.Path, err)
			}
			paths = append(paths, f.Path)
		}
		steps = append(steps, PlanStep{
			SpecID:  s.ID,
			Kind:    KindFile,
			Target:  path,
			Format:  t.Format,
			Exists:  exists,
			Summary: fmt.Sprintf("设置 %d 个字段：%s", len(paths), strings.Join(paths, ", ")),
			doc:     doc,
			secret:  in.APIKey,
		})
	}

	if vars := envVarNames(s); len(vars) > 0 {
		steps = append(steps, PlanStep{
			SpecID:  s.ID,
			Kind:    KindEnv,
			Target:  strings.Join(vars, " "),
			Vars:    vars,
			Summary: fmt.Sprintf("需设置 %d 个环境变量（本插件不落盘）", len(vars)),
		})
	}
	return steps, values, nil
}

func (in Inputs) displayName() string {
	if in.ModelDisplayName != "" {
		return in.ModelDisplayName
	}
	return in.Model
}

func envVarNames(s spec.Spec) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range append([]string{s.Env.BaseURL, s.Env.APIKey, s.Env.Model}, s.Env.Extra...) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func fieldValue(specID string, f spec.Field, in Inputs, baseURL, gatewayURL string) (any, error) {
	switch strings.TrimSpace(f.From) {
	case "base_url":
		if baseURL == "" {
			return nil, fmt.Errorf("spec %s: base_url 需要 --gateway", specID)
		}
		return baseURL, nil
	case "gateway_url":
		return gatewayURL, nil
	case "api_key":
		if in.APIKey == "" {
			return nil, fmt.Errorf("spec %s: %w", specID, ErrMissingAPIKey)
		}
		return in.APIKey, nil
	case "model":
		if in.Model == "" {
			return nil, fmt.Errorf("spec %s: 字段 %s 需要 --model", specID, f.Path)
		}
		return in.Model, nil
	case "display_name":
		name := in.displayName()
		if name == "" {
			return nil, fmt.Errorf("spec %s: 字段 %s 需要 --model", specID, f.Path)
		}
		return name, nil
	case "literal":
		return f.Value, nil
	case "":
		return nil, fmt.Errorf("spec %s: 字段 %s 未声明 from", specID, f.Path)
	default:
		return nil, fmt.Errorf("spec %s: 字段 %s 的 from=%q 不受支持（可选 base_url|api_key|model|display_name|gateway_url|literal）", specID, f.Path, f.From)
	}
}

// GatewayBaseURL 把网关根地址规范成 OpenAI 兼容客户端使用的 base_url：
// 裸主机（无路径）补 /v1；已经是 /v1 或带自定义路径时原样返回。
// 这是 values 里 base_url 的唯一来源，cmd 侧走 IPC 时也用它，保证两条路径写入一致。
func GatewayBaseURL(gatewayURL string) string {
	return normalizeBaseURL(strings.TrimRight(strings.TrimSpace(gatewayURL), "/"))
}

// normalizeBaseURL 把网关根地址规范成 OpenAI 兼容客户端使用的 base_url：
// 裸主机（无路径）补 /v1；已经是 /v1 或带自定义路径时原样返回。
func normalizeBaseURL(gatewayURL string) string {
	if gatewayURL == "" {
		return ""
	}
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return gatewayURL
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "" {
		u.Path = "/v1"
		return strings.TrimRight(u.String(), "/")
	}
	u.Path = p
	return strings.TrimRight(u.String(), "/")
}

// EnvLines 按 style 渲染环境变量设置语句（供 print-env 使用）。
// 这里会输出真实密钥值——print-env 的用途就是把凭据交给用户的 shell，因此调用方不得把它写进日志。
func EnvLines(s spec.Spec, in Inputs, style string) ([]string, error) {
	if strings.TrimSpace(style) == "" {
		style = s.Env.Style
	}
	if strings.TrimSpace(style) == "" {
		if runtime.GOOS == "windows" {
			style = "set"
		} else {
			style = "export"
		}
	}
	pairs := [][2]string{
		{s.Env.BaseURL, normalizeBaseURL(strings.TrimRight(strings.TrimSpace(in.GatewayURL), "/"))},
		{s.Env.APIKey, in.APIKey},
		{s.Env.Model, in.Model},
	}
	var lines []string
	for _, p := range pairs {
		if p[0] == "" {
			continue
		}
		if p[1] == "" {
			lines = append(lines, "# "+p[0]+"：未提供取值（--gateway/--api-key/--model）")
			continue
		}
		line, err := renderEnv(style, p[0], p[1])
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	for _, extra := range s.Env.Extra {
		extra = strings.TrimSpace(extra)
		if extra == "" {
			continue
		}
		if v := os.Getenv(extra); v != "" {
			line, err := renderEnv(style, extra, v)
			if err != nil {
				return nil, err
			}
			lines = append(lines, line)
			continue
		}
		lines = append(lines, "# "+extra+"：规格声明需要，但本机未设置（取值由用户确认）")
	}
	return lines, nil
}

func renderEnv(style, name, value string) (string, error) {
	switch style {
	case "export":
		return fmt.Sprintf("export %s=%s", name, shellQuote(value)), nil
	case "set":
		return fmt.Sprintf("set %s=%s", name, value), nil
	case "powershell":
		return fmt.Sprintf("$env:%s=%q", name, value), nil
	default:
		return "", fmt.Errorf("未知 --style=%q（可选 export|set|powershell）", style)
	}
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
