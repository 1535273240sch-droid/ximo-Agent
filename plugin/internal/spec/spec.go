// Package spec 定义「如何把中转站接进某个 Agent」的数据模型，并提供内置规格库
// （//go:embed）+ 用户自定义规格目录的加载与校验。
//
// 类型定义与 recon/插件契约-冻结.md §2 逐字段对应，属于冻结契约：id/name/
// description/docs/detect/env/files/restart_note/protocols 的 JSON tag 与取值
// 白名单不得改动，其他包（internal/engine、internal/formats、cmd/ximo-plugin）
// 按这些字段实现。
//
// 设计目标：**加一个新 Agent = 加一份 JSON，无需改代码**。凡是需要真正写代码的
// （例如 ximo-agent 用 IPC 帧而非改文件），规格里的 files 只是回退路径，
// 权威实现放在 internal/agents。
package spec

// Spec 描述「如何把中转站接进某个 Agent」。加一个新 Agent = 加一份 JSON，无需改代码。
type Spec struct {
	ID          string   `json:"id"`                     // 例 "claude-code"
	Name        string   `json:"name"`                   // 展示名
	Description string   `json:"description"`            //
	Docs        string   `json:"docs,omitempty"`         // 参考链接
	Detect      Detect   `json:"detect"`                 //
	Env         EnvSpec  `json:"env,omitempty"`          // 环境变量形态（很多 Agent 只认环境变量）
	Files       []Target `json:"files,omitempty"`        // 配置文件形态
	RestartNote string   `json:"restart_note,omitempty"` // 例「改完需重启 Agent」
	Protocols   []string `json:"protocols,omitempty"`    // 支持的入站协议：openai-chat / anthropic-messages
}

// Detect 是「本机有没有这个 Agent」的证据。
//
// 三个列表的语义都是 **any-of**（任一命中即算检测到），因为绝大多数 Agent
// 在不同平台上只有其中一种痕迹。engine 的 Detect 必须按 any-of 实现。
type Detect struct {
	Binaries []string `json:"binaries,omitempty"` // PATH 上的可执行名
	Paths    []string `json:"paths,omitempty"`    // 必须存在的路径（支持 ~ 与 %VAR% 展开）
	Env      []string `json:"env,omitempty"`      // 必须已设置的环境变量
}

// Empty 报告这份证据是否为空（空证据的规格无法被 detect 命中）。
func (d Detect) Empty() bool {
	return len(d.Binaries) == 0 && len(d.Paths) == 0 && len(d.Env) == 0
}

// Target 是一个要改的文件（或只读探测的家庭配置）。
type Target struct {
	Path   string  `json:"path"`             // 支持 ~ 与 %APPDATA% 等展开
	Format string  `json:"format"`           // json | toml | yaml | dotenv
	Create bool    `json:"create,omitempty"` // 不存在时是否创建
	Fields []Field `json:"fields"`           //
}

// Field 把网关信息映射到文件里的某个位置。Path 用点号路径，数字段表示数组下标（例 "models.0.api_base"）。
type Field struct {
	Path  string `json:"path"`            //
	From  string `json:"from"`            // base_url | api_key | model | display_name | gateway_url | literal
	Value string `json:"value,omitempty"` // From==literal 时的常量
}

// EnvSpec 描述该 Agent 认哪些环境变量。
//
// BaseURL / APIKey / Model 里放的是**变量名**（例 "ANTHROPIC_BASE_URL"），
// 不是值——Validate 会拒绝把 URL 或 key 字面量写进来。Extra 只应被「提示」，
// 不应被自动赋值（它是备选/相关变量，例：claude-code 的 ANTHROPIC_API_KEY）。
type EnvSpec struct {
	BaseURL string   `json:"base_url,omitempty"` // 例 "ANTHROPIC_BASE_URL"
	APIKey  string   `json:"api_key,omitempty"`  // 例 "ANTHROPIC_AUTH_TOKEN"
	Model   string   `json:"model,omitempty"`    //
	Extra   []string `json:"extra,omitempty"`    // 需要一并提示的变量
	Style   string   `json:"style,omitempty"`    // export(POSIX) | set(Windows cmd) | powershell
}

// Empty 报告这份环境变量形态是否什么都没写。
func (e EnvSpec) Empty() bool {
	return e.BaseURL == "" && e.APIKey == "" && e.Model == "" && len(e.Extra) == 0
}

// Field.From 的取值白名单。
const (
	FromBaseURL     = "base_url"
	FromAPIKey      = "api_key"
	FromModel       = "model"
	FromDisplayName = "display_name"
	FromGatewayURL  = "gateway_url"
	FromLiteral     = "literal"
)

// FromValues 是 Field.From 的全部合法取值。
var FromValues = []string{FromBaseURL, FromAPIKey, FromModel, FromDisplayName, FromGatewayURL, FromLiteral}

// Target.Format 的取值白名单（与 internal/formats 的实现一一对应）。
const (
	FormatJSON   = "json"
	FormatTOML   = "toml"
	FormatYAML   = "yaml"
	FormatDotenv = "dotenv"
)

// Formats 是 Target.Format 的全部合法取值。
var Formats = []string{FormatJSON, FormatTOML, FormatYAML, FormatDotenv}

// EnvSpec.Style 的取值白名单。空串表示「由 CLI 的 --style 决定，默认 export」。
const (
	StyleExport     = "export"
	StyleSet        = "set"
	StylePowerShell = "powershell"
)

// Styles 是 EnvSpec.Style 的全部合法取值。
var Styles = []string{StyleExport, StyleSet, StylePowerShell}

// 入站协议取值。
const (
	ProtocolOpenAIChat        = "openai-chat"
	ProtocolAnthropicMessages = "anthropic-messages"
)

// ProtocolValues 是 Spec.Protocols 的全部合法取值。
var ProtocolValues = []string{ProtocolOpenAIChat, ProtocolAnthropicMessages}

// Get 在规格切片里按 ID 取一份（大小写敏感的精确匹配）。
func Get(specs []Spec, id string) (Spec, bool) {
	for _, s := range specs {
		if s.ID == id {
			return s, true
		}
	}
	return Spec{}, false
}

// IDs 返回规格 ID 列表（保持传入顺序）。
func IDs(specs []Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.ID)
	}
	return out
}
