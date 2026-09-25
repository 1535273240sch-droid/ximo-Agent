package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// ---------------------------------------------------------------------------
// Worker → Tool 桥接
//
// 背景：browser / terminal / office / dynamic-js / mcp / computer-use 六个
// 高危域的能力全部实现在 internal/worker 下，由 worker.Manager 统一托管
// （租约、崩溃恢复、进程树清理、熔断、排队）。但 buildToolRuntime 过去只注册了
// file/git/knowledge/web 四个进程内工具域，Worker 池从未被暴露给 Agent ——
// 结果是「引擎里有一整套工具执行链，模型却一个也调不到」。
//
// 本文件补上最后一层接线：为每个 (kind, action) 组合生成一个 tool.Tool，
// 使 Agent 看到的工具列表与 Worker 池的实际能力一致。
//
// 安全边界不变：Worker 自身的策略判定（allowed_roots / enabled /
// allow_shell / allowed_windows 等）仍然是最终裁决者，本层不做任何放行。
// ---------------------------------------------------------------------------

// workerTool 把一个 Worker 动作适配成 tool.Tool。
type workerTool struct {
	def    tool.ToolDefinition
	kind   string
	action string
	mgr    *worker.Manager
	norm   *worker.DefaultResultNormalizer
}

var _ tool.Tool = (*workerTool)(nil)

// Definition 实现 tool.Tool。
func (t *workerTool) Definition() tool.ToolDefinition { return t.def }

// Execute 实现 tool.Tool：把 ToolRequest 翻译成 WorkerRequest，交给 Manager
// 派发，再把 WorkerResponse 归一化成 ToolResponse。
//
// 错误语义（关键，决定一次工具失败会不会打断整个 run）：
//   - Worker 的业务失败（策略拒绝、命令非零退出、上游报错）以 resp.Error 返回，
//     Manager 的 error 为 nil；
//   - 基础设施故障（池未注册、熔断打开、队列满、Worker 崩溃）Manager 会同时
//     返回一个 error 和一个带 CallID 的错误响应。
//
// 两类都必须转成 Success=false 的正常结果交回模型自纠，绝不上抛 error ——
// 端口层约定「非 nil error 表示运行时自身坏了」，而这里都只是工具失败。
func (t *workerTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	args := req.Arguments
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return failedToolResult(req, t.def.Name, tool.ErrSchemaInvalid,
			fmt.Sprintf("工具参数序列化失败：%v", err))
	}

	timeout := t.def.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if !req.Deadline.IsZero() {
		if left := time.Until(req.Deadline); left > 0 && left < timeout {
			timeout = left
		}
	}

	wreq := worker.WorkerRequest{
		CallID:    req.ToolCallID,
		RunID:     req.RunID,
		SessionID: req.SessionID,
		Kind:      t.kind,
		Action:    t.action,
		Args:      raw,
		Timeout:   timeout,
	}

	resp, execErr := t.mgr.Execute(ctx, wreq)
	if execErr != nil && resp.CallID == "" {
		// 派发前就失败了（池不存在/已关闭），没有可归一化的响应体。
		return failedToolResult(req, t.def.Name, tool.ErrWorkerCrashed, execErr.Error())
	}

	norm, nerr := t.norm.Normalize(resp)
	if nerr != nil {
		return failedToolResult(req, t.def.Name, tool.ErrNormalizationFailed, nerr.Error())
	}

	out := tool.ToolResponse{
		ToolCallID:  req.ToolCallID,
		ToolName:    t.def.Name,
		Content:     norm.Content,
		Success:     norm.Success,
		Error:       norm.Error,
		DisplayType: norm.DisplayType,
		Metadata:    norm.Data,
		Domain:      t.def.Domain,
	}
	if !norm.Success {
		out.ErrorCode = workerErrorToToolCode(resp)
		out.Metadata = mergeWorkerMeta(norm.Data, resp)
		if out.Error == "" && execErr != nil {
			out.Error = execErr.Error()
		}
	}
	return out
}

// failedToolResult 构造一个不经过 Worker 的失败结果。
func failedToolResult(req tool.ToolRequest, name string, code tool.ErrorCode, msg string) tool.ToolResponse {
	return tool.ToolResponse{
		ToolCallID: req.ToolCallID,
		ToolName:   name,
		Success:    false,
		Error:      msg,
		ErrorCode:  code,
	}
}

// workerErrorToToolCode 把 worker 侧错误码映射为工具层错误码，
// 使 Engine 的重试/确认决策建立在统一语义上。
func workerErrorToToolCode(resp worker.WorkerResponse) tool.ErrorCode {
	if resp.Error == nil {
		return tool.ErrToolFailed
	}
	switch resp.Error.Code {
	case worker.CodeInvalidArgument:
		return tool.ErrSchemaInvalid
	case worker.CodePolicyDenied:
		return tool.ErrPermissionDenied
	case worker.CodeTimeout:
		return tool.ErrWorkerTimeout
	case worker.CodeCanceled:
		return tool.ErrCanceled
	case worker.CodeCrash, worker.CodeUnavailable:
		return tool.ErrWorkerCrashed
	default:
		return tool.ErrToolFailed
	}
}

// mergeWorkerMeta 在归一化得到的结构化数据上补充 Worker 诊断字段，
// 便于 UI 与审计定位「是哪个 worker 出的错、什么错误码」。
func mergeWorkerMeta(data map[string]any, resp worker.WorkerResponse) map[string]any {
	meta := make(map[string]any, len(data)+3)
	for k, v := range data {
		meta[k] = v
	}
	if resp.Error != nil {
		meta["worker_error_code"] = resp.Error.Code
		meta["worker_error_retryable"] = resp.Error.Retryable
	}
	if resp.WorkerID != "" {
		meta["worker_id"] = resp.WorkerID
	}
	return meta
}

// ---------------------------------------------------------------------------
// 工具清单声明
// ---------------------------------------------------------------------------

// workerToolSpec 声明一个 Worker 动作到 Tool 的映射。
type workerToolSpec struct {
	Name        string
	Description string
	Kind        string
	Action      string
	Params      tool.JSONSchema
	Risk        tool.RiskLevel
	Idempotency tool.IdempotencyClass
	Domain      tool.ExecutionDomain
	SideEffect  bool
	Timeout     time.Duration
}

func (s workerToolSpec) definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        s.Name,
		Description: s.Description,
		Parameters:  s.Params,
		Risk:        s.Risk,
		Idempotency: s.Idempotency,
		Domain:      s.Domain,
		SideEffect:  s.SideEffect,
		Timeout:     s.Timeout,
	}
}

// --- schema 构造辅助 ---

func pStr(desc string) tool.JSONSchema  { return tool.JSONSchema{Type: "string", Description: desc} }
func pNum(desc string) tool.JSONSchema  { return tool.JSONSchema{Type: "number", Description: desc} }
func pInt(desc string) tool.JSONSchema  { return tool.JSONSchema{Type: "integer", Description: desc} }
func pBool(desc string) tool.JSONSchema { return tool.JSONSchema{Type: "boolean", Description: desc} }
func pObj(desc string) tool.JSONSchema  { return tool.JSONSchema{Type: "object", Description: desc} }
func pStrArr(desc string) tool.JSONSchema {
	return tool.JSONSchema{Type: "array", Description: desc, Items: &tool.JSONSchema{Type: "string"}}
}

// workerToolSpecs 返回全部 Worker 工具声明。
//
// 声明与各 Worker 的 Execute 分支一一对应；新增 Worker 动作时必须同步这里，
// 否则模型看不到该能力（工具注册以本清单为准，不反射扫描）。
func workerToolSpecs() []workerToolSpec {
	specs := []workerToolSpec{
		// ---------------- terminal ----------------
		{
			Name:        "terminal_exec",
			Description: "在受控沙箱内执行一条系统命令（argv 模式不经 shell，从根本上消除注入）。cwd 必须落在允许的根目录内；默认已禁用 shell 模式、命令链接符与高风险命令（cmd/powershell/rm 等）。",
			Kind:        "terminal",
			Action:      "exec",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"command":    pStr("要执行的命令。argv 模式下按空白安全切分（支持引号分组），不会交给 shell 解释。"),
				"argv":       pStrArr("参数数组。优先于 command；给出时 command 被忽略。"),
				"cwd":        pStr("工作目录，必须落在允许写入的根目录内。"),
				"mode":       {Type: "string", Description: "执行模式。", Enum: []any{"argv", "shell"}},
				"stdin":      pStr("标准输入内容。"),
				"timeout_ms": pInt("超时毫秒数，只能收紧策略上限。"),
			}, "command"),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerTerminal,
			SideEffect:  true,
			Timeout:     90 * time.Second,
		},

		// ---------------- browser ----------------
		{
			Name:        "browser_navigate",
			Description: "在受控浏览器中打开一个 URL，返回最终地址、标题与状态码。",
			Kind:        "browser",
			Action:      "navigate",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"url":        pStr("要打开的完整 URL（含协议）。"),
				"timeout_ms": pInt("导航超时毫秒数。"),
			}, "url"),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerBrowser,
			Timeout:     60 * time.Second,
		},
		{
			Name:        "browser_screenshot",
			Description: "对当前页面或指定元素截图。截图以 CAS 产物引用返回，不内联二进制。",
			Kind:        "browser",
			Action:      "screenshot",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"selector":  pStr("CSS 选择器。省略则截取整个视口。"),
				"full_page": pBool("是否截取整页（含滚动区域）。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerBrowser,
			Timeout:     60 * time.Second,
		},
		{
			Name:        "browser_click",
			Description: "点击页面上匹配 CSS 选择器的元素。",
			Kind:        "browser",
			Action:      "click",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"selector":   pStr("CSS 选择器。"),
				"timeout_ms": pInt("等待元素出现与点击的超时毫秒数。"),
			}, "selector"),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassDetectable,
			Domain:      tool.DomainWorkerBrowser,
			SideEffect:  true,
			Timeout:     60 * time.Second,
		},
		{
			Name:        "browser_type",
			Description: "向匹配 CSS 选择器的输入框填入文本（清空后输入）。不回显文本内容。",
			Kind:        "browser",
			Action:      "type",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"selector":   pStr("CSS 选择器。"),
				"text":       pStr("要填入的文本。"),
				"timeout_ms": pInt("超时毫秒数。"),
			}, "selector", "text"),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassDetectable,
			Domain:      tool.DomainWorkerBrowser,
			SideEffect:  true,
			Timeout:     60 * time.Second,
		},
		{
			Name:        "browser_get_content",
			Description: "读取当前页面或指定元素的文本内容。",
			Kind:        "browser",
			Action:      "get_content",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"selector":   pStr("CSS 选择器。省略则取整页文本。"),
				"max_length": pInt("最大返回字符数（默认 10000，上限 50000）。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerBrowser,
			Timeout:     60 * time.Second,
		},
		{
			Name:        "browser_execute_js",
			Description: "在页面上下文中执行一段 JavaScript 并返回其结果（结果截断到 30000 字符）。",
			Kind:        "browser",
			Action:      "execute_js",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"code": pStr("要执行的 JavaScript 源码。"),
			}, "code"),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerBrowser,
			SideEffect:  true,
			Timeout:     60 * time.Second,
		},
		{
			Name:        "browser_network_monitor",
			Description: "查看当前页面已发生的网络请求（可按 URL 关键字过滤）。",
			Kind:        "browser",
			Action:      "network_monitor",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"filter":      pStr("URL 关键字过滤。"),
				"max_results": pInt("最大返回条数（默认 30，上限 100）。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerBrowser,
			Timeout:     60 * time.Second,
		},

		// ---------------- dynamic-js ----------------
		{
			Name:        "dynamic_eval",
			Description: "在隔离的 JS 沙箱（goja）中执行一段代码做纯计算或数据处理。沙箱默认只开放 input/log/output：网络与文件能力需显式配置白名单后才可用。",
			Kind:        "dynamic-js",
			Action:      "eval",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"code":       pStr("要执行的 JavaScript 源码。可用 tool.input / tool.log / tool.output 与 console.*。"),
				"input":      pObj("注入到沙箱的输入数据。"),
				"timeout_ms": pInt("执行超时毫秒数（只能收紧上限）。"),
			}, "code"),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerDynamicJS,
			Timeout:     30 * time.Second,
		},

		// ---------------- mcp ----------------
		{
			Name:        "mcp_list_tools",
			Description: "列出所有已连接 MCP 服务器暴露的工具（聚合结果，含每个工具的所属服务器与入参 schema）。调用 MCP 工具前先用本工具确认工具名。",
			Kind:        "mcp",
			Action:      "tools/list",
			Params:      tool.ObjectSchema(map[string]tool.JSONSchema{}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerMCP,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "mcp_call_tool",
			Description: "调用一个 MCP 服务器上的工具。工具名可用 mcp_list_tools 查询；当多个服务器存在同名工具时，请显式指定 server。",
			Kind:        "mcp",
			Action:      "tools/call",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"server":    pStr("MCP 服务器 ID 或名称。省略则在所有服务器中按工具名查找。"),
				"tool":      pStr("要调用的 MCP 工具名（不含 mcp__ 前缀）。"),
				"arguments": pObj("传给该 MCP 工具的参数对象。"),
			}, "tool"),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerMCP,
			SideEffect:  true,
			Timeout:     120 * time.Second,
		},
		{
			Name:        "mcp_status",
			Description: "查看每个 MCP 服务器的连接状态（state / 重连次数 / 工具数 / 熔断状态 / 子进程 PID）。",
			Kind:        "mcp",
			Action:      "status",
			Params:      tool.ObjectSchema(map[string]tool.JSONSchema{}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerMCP,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "mcp_refresh_tools",
			Description: "重新向 MCP 服务器拉取工具列表（服务器侧工具集变更后使用）。",
			Kind:        "mcp",
			Action:      "refresh_tools",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"server": pStr("只刷新指定服务器。省略则刷新全部就绪的服务器。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerMCP,
			Timeout:     60 * time.Second,
		},

		// ---------------- office ----------------
		{
			Name:        "office_docs",
			Description: "通过 officecli 读写办公文档（docx/xlsx/pptx）。action 决定具体操作；除 help 外都必须给出 filePath，且路径必须落在允许的根目录内。",
			Kind:        "office",
			Action:      "docs",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"action": {
					Type:        "string",
					Description: "子操作。写类：create/set/add/remove/move/batch/merge/dump/save；读类：get/query/validate/view；另有 help。",
					Enum: []any{
						"create", "get", "query", "set", "add", "remove", "move",
						"batch", "merge", "validate", "dump", "view", "save", "help",
					},
				},
				"filePath":     pStr("目标文档路径。help 之外必填。"),
				"path":         pStr("文档内的定位路径（如表格/单元格）。"),
				"selector":     pStr("元素选择器。"),
				"properties":   pObj("要写入的属性键值对。"),
				"operations":   {Type: "array", Description: "批量操作列表（action=batch 时使用）。"},
				"templateData": pObj("模板变量（action=create 基于模板时使用）。"),
				"outputPath":   pStr("输出路径（另存为时使用）。"),
				"depth":        pInt("遍历深度（view/dump）。"),
				"mode":         pStr("操作模式。"),
				"timeout_ms":   pInt("超时毫秒数（只能收紧上限）。"),
			}, "action", "filePath"),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassDetectable,
			Domain:      tool.DomainWorkerOffice,
			SideEffect:  true,
			Timeout:     120 * time.Second,
		},

		// ---------------- computer-use ----------------
		{
			Name:        "computer_screenshot",
			Description: "截取当前桌面画面（含可交互元素标注），用于「看一眼屏幕」。",
			Kind:        "computer-use",
			Action:      "screenshot",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"includeImage": pBool("是否附带图像数据。"),
				"maxDimension": pInt("图像最长边像素上限。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			Timeout:     45 * time.Second,
		},
		{
			Name:        "computer_observe",
			Description: "观察桌面 UI 结构，返回可交互元素及其 ref 标识（后续点击/输入要引用 ref）。",
			Kind:        "computer-use",
			Action:      "observe",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"window":       pStr("目标窗口标题（受 allowed_windows/denied_windows 约束）。"),
				"includeImage": pBool("是否附带图像数据。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			Timeout:     45 * time.Second,
		},
		{
			Name:        "computer_find_window",
			Description: "列出当前打开的窗口（可按标题过滤），用于找到目标窗口。",
			Kind:        "computer-use",
			Action:      "find_window",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"window": pStr("窗口标题关键字。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "computer_click_element",
			Description: "点击由 observe 返回的 ref 所标识的 UI 元素。",
			Kind:        "computer-use",
			Action:      "click_element",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"ref":    pStr("元素引用标识（来自 computer_observe）。"),
				"window": pStr("目标窗口标题。"),
			}, "ref"),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			SideEffect:  true,
			Timeout:     45 * time.Second,
		},
		{
			Name:        "computer_set_text",
			Description: "向由 ref 标识的输入控件写入文本。",
			Kind:        "computer-use",
			Action:      "set_text",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"ref":    pStr("元素引用标识（来自 computer_observe）。"),
				"text":   pStr("要写入的文本。"),
				"window": pStr("目标窗口标题。"),
			}, "ref", "text"),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			SideEffect:  true,
			Timeout:     45 * time.Second,
		},
		{
			Name:        "computer_read_text",
			Description: "读取由 ref 标识的元素（或窗口）中的文本。",
			Kind:        "computer-use",
			Action:      "read_text",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"ref":    pStr("元素引用标识。"),
				"window": pStr("目标窗口标题。"),
			}, "ref"),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			Timeout:     45 * time.Second,
		},
		{
			Name:        "computer_mouse_click",
			Description: "在屏幕指定坐标点击鼠标。",
			Kind:        "computer-use",
			Action:      "mouse_click",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"x":          pInt("屏幕 X 坐标。"),
				"y":          pInt("屏幕 Y 坐标。"),
				"button":     pStr("按键：left / right / middle。默认 left。"),
				"clickCount": pInt("点击次数，默认 1。"),
			}),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			SideEffect:  true,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "computer_mouse_move",
			Description: "把鼠标移动到屏幕指定坐标。",
			Kind:        "computer-use",
			Action:      "mouse_move",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"x": pInt("屏幕 X 坐标。"),
				"y": pInt("屏幕 Y 坐标。"),
			}),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "computer_mouse_drag",
			Description: "按住鼠标沿给定路径拖拽（至少两个点）。",
			Kind:        "computer-use",
			Action:      "mouse_drag",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"path": {
					Type:        "array",
					Description: "拖拽路径点列表，每项形如 {\"x\":100,\"y\":200}，至少两项。",
					Items: &tool.JSONSchema{
						Type: "object",
						Properties: map[string]tool.JSONSchema{
							"x": pInt("X 坐标。"),
							"y": pInt("Y 坐标。"),
						},
					},
				},
			}, "path"),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			SideEffect:  true,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "computer_mouse_scroll",
			Description: "在指定坐标滚动鼠标滚轮。",
			Kind:        "computer-use",
			Action:      "mouse_scroll",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"x":       pInt("屏幕 X 坐标。"),
				"y":       pInt("屏幕 Y 坐标。"),
				"scrollX": pInt("横向滚动量。"),
				"scrollY": pInt("纵向滚动量（正数向下）。"),
			}),
			Risk:        tool.RiskMedium,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "computer_key_press",
			Description: "按下组合键（例如 [\"ctrl\",\"s\"] 或 [\"alt\",\"f4\"]）。",
			Kind:        "computer-use",
			Action:      "key_press",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"keys": pStrArr("按键序列，例如 [\"ctrl\",\"c\"]。"),
			}, "keys"),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			SideEffect:  true,
			Timeout:     30 * time.Second,
		},
		{
			Name:        "computer_key_type",
			Description: "键入一段文本（逐字符），用于向当前焦点控件输入。",
			Kind:        "computer-use",
			Action:      "key_type",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"text": pStr("要键入的文本。"),
			}, "text"),
			Risk:        tool.RiskHigh,
			Idempotency: tool.ClassNonIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			SideEffect:  true,
			Timeout:     45 * time.Second,
		},
		{
			Name:        "computer_wait",
			Description: "等待某个 UI 条件成立（默认等待元素出现）。",
			Kind:        "computer-use",
			Action:      "wait",
			Params: tool.ObjectSchema(map[string]tool.JSONSchema{
				"until":     pStr("等待条件，默认 present（出现）。"),
				"timeoutMs": pInt("等待超时毫秒数，默认 10000。"),
				"window":    pStr("目标窗口标题。"),
			}),
			Risk:        tool.RiskLow,
			Idempotency: tool.ClassIdempotent,
			Domain:      tool.DomainWorkerComputerUse,
			Timeout:     60 * time.Second,
		},
	}
	return specs
}

// registerWorkerTools 把「已启用的 Worker 池」对应的工具注册进注册表。
//
// enabled 来自 cfg.Runtime.WorkerPools（槽位 > 0 才算启用）：只为真正被拉起的
// kind 生成工具。这样默认配置下模型看不到 office / computer-use 的工具，
// 避免给出「看起来能调、实际必定失败」的假能力。
func registerWorkerTools(registry tool.Registry, mgr *worker.Manager, enabled map[string]bool) error {
	if mgr == nil {
		return nil
	}
	norm := worker.NewDefaultResultNormalizer(nil)
	registered := 0
	for _, spec := range workerToolSpecs() {
		if !enabled[spec.Kind] {
			continue
		}
		t := &workerTool{
			def:    spec.definition(),
			kind:   spec.Kind,
			action: spec.Action,
			mgr:    mgr,
			norm:   norm,
		}
		if err := registry.Register(t); err != nil {
			return fmt.Errorf("register worker tool %s (kind=%s action=%s): %w",
				spec.Name, spec.Kind, spec.Action, err)
		}
		registered++
	}
	if registered > 0 {
		// 用 fmt 而非日志包，保证 bootstrap 阶段不引入新的依赖方向。
		fmt.Printf("[bootstrap] 已注册 %d 个 Worker 工具（kind: %v）\n", registered, enabledKinds(enabled))
	}
	return nil
}

// enabledKinds 把 enabled 集合转成有序切片，仅用于日志可读性。
func enabledKinds(enabled map[string]bool) []string {
	out := make([]string, 0, len(enabled))
	for _, k := range []string{"terminal", "browser", "office", "dynamic-js", "mcp", "computer-use"} {
		if enabled[k] {
			out = append(out, k)
		}
	}
	return out
}
