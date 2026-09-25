package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// DynamicTool：运行时创建的自定义工具（第14章加固对象）
// ---------------------------------------------------------------------------

// DynamicTool 是 Agent 通过 create_tool 元工具创建的自定义工具。
// 与 v1（src/main/tools/DynamicTool.ts）的关键差异——v1 审计问题全部修复：
//   - v1 把全局 fetch 暴露给沙箱 → 任意网络访问；本实现只暴露
//     tool.http(allowlisted)；
//   - v1 只有 30s Promise.race 超时，且 race 只 reject 外层 promise、
//     不中断 JS 执行；本实现用 vm.Interrupt 硬中断 + capability ctx 截止；
//   - v1 无 memory limit / instruction limit；本实现叠加内存采样中断 +
//     循环迭代预算插桩 + 调用栈上限；
//   - v1 无静态检查；本实现拒绝 require/import/process/os/eval/Function 等
//     禁用 API 与可证明无界的循环。
type DynamicTool struct {
	def     ToolDefinition
	code    string
	sandbox SandboxManager
}

// 编译期检查：DynamicTool 实现 Tool。
var _ Tool = (*DynamicTool)(nil)

// NewDynamicTool 创建动态工具。sandbox 为 nil 时执行阶段拒绝（fail-closed）。
func NewDynamicTool(name, description string, parameters JSONSchema, code string, sandbox SandboxManager) *DynamicTool {
	return &DynamicTool{
		def: ToolDefinition{
			Name:        name,
			Description: description,
			Parameters:  parameters,
			Risk:        RiskHigh,
			Idempotency: ClassDetectable,
			Domain:      DomainWorkerDynamicJS,
			SideEffect:  true,
		},
		code:    code,
		sandbox: sandbox,
	}
}

// Definition 实现 Tool。
func (d *DynamicTool) Definition() ToolDefinition { return d.def }

// Code 返回工具代码（供 Worker 传输与审计）。
func (d *DynamicTool) Code() string { return d.code }

// Execute 实现 Tool：在沙箱中执行动态工具代码，args 作为 tool.input。
func (d *DynamicTool) Execute(ctx context.Context, req ToolRequest) ToolResponse {
	if d.sandbox == nil {
		return ToolResponse{
			ToolCallID: req.ToolCallID,
			ToolName:   d.def.Name,
			Success:    false,
			Error:      "未配置沙箱，拒绝执行动态工具（fail-closed）",
			ErrorCode:  ErrSandboxViolation,
		}
	}
	result, err := d.sandbox.Run(ctx, d.code, req.Arguments)
	resp := ToolResponse{
		ToolCallID:  req.ToolCallID,
		ToolName:    d.def.Name,
		Success:     result.Success,
		DisplayType: "text",
		Metadata: map[string]any{
			"dynamic":          true,
			"loop_iterations":  result.Stats.LoopIterations,
			"http_calls":       result.Stats.HTTPCalls,
			"fs_calls":         result.Stats.FSCalls,
			"timed_out":        result.Stats.TimedOut,
			"memory_exceeded":  result.Stats.MemoryExceeded,
			"budget_exhausted": result.Stats.BudgetExhausted,
		},
		Domain: DomainWorkerDynamicJS,
	}
	if len(result.Logs) > 0 {
		resp.Content = result.Content
		if result.Content != "" {
			resp.Content += "\n\n"
		}
		resp.Content += "[console.log]\n" + strings.Join(result.Logs, "\n")
	} else {
		resp.Content = result.Content
	}
	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
		var violation *SandboxViolationError
		if errors.As(err, &violation) {
			resp.ErrorCode = ErrSandboxViolation
			resp.Metadata["violation"] = string(violation.Kind)
		} else {
			resp.ErrorCode = ErrToolFailed
		}
	}
	return resp
}

// ---------------------------------------------------------------------------
// CreateToolTool：create_tool 元工具
// ---------------------------------------------------------------------------

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// CreateToolTool 让 Agent 在运行时创建自定义工具（对应 v1 的 CreateToolTool）。
// 与 v1 的差异：创建前对代码做沙箱静态检查，含禁用 API / 无界循环 /
// 保留标识符的代码在创建阶段即被拒绝，不会进入注册表。
type CreateToolTool struct {
	registry Registry
	sandbox  SandboxManager
}

// 编译期检查。
var _ Tool = (*CreateToolTool)(nil)

// NewCreateToolTool 创建元工具。
func NewCreateToolTool(registry Registry, sandbox SandboxManager) *CreateToolTool {
	return &CreateToolTool{registry: registry, sandbox: sandbox}
}

// Definition 实现 Tool。
func (c *CreateToolTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "create_tool",
		Description: "为当前会话创建一个自定义工具。创建后可在后续步骤中直接调用。" +
			"代码在沙箱中执行：接收 args 参数（即 tool.input），需 return 结果（字符串或 {content, success} 对象）。" +
			"可用的能力 API：tool.log(msg) 调试输出、tool.http({method,url,headers,body}) 白名单网络请求、" +
			"tool.fs({op,path,content}) 白名单文件读写、tool.output(value) 显式输出、tool.parseURL(url) 解析 URL。" +
			"沙箱硬限制：执行超时、内存上限、循环迭代预算；禁止 require/import/process/os/eval/Function 等 API，" +
			"禁止 while(true)/for(;;) 等无界循环。",
		Parameters: ObjectSchema(map[string]JSONSchema{
			"name":        {Type: "string", Description: "工具名称（snake_case，如 calc_tax、format_date）"},
			"description": {Type: "string", Description: "工具描述，Agent 据此决定何时使用此工具"},
			"parameters":  {Type: "object", Description: "工具参数的 JSON Schema：{\"type\":\"object\",\"properties\":{...},\"required\":[...]}"},
			"code":        {Type: "string", Description: "JavaScript 执行代码（函数体）。接收 args，需 return 结果"},
		}, "name", "description", "parameters", "code"),
		Risk:        RiskMedium,
		Idempotency: ClassDetectable,
		Domain:      DomainInProcess,
		SideEffect:  true,
	}
}

// Execute 实现 Tool。
func (c *CreateToolTool) Execute(ctx context.Context, req ToolRequest) ToolResponse {
	fail := func(msg string) ToolResponse {
		return ToolResponse{ToolCallID: req.ToolCallID, ToolName: "create_tool", Success: false, Error: msg, ErrorCode: ErrSchemaInvalid}
	}

	name, _ := req.Arguments["name"].(string)
	description, _ := req.Arguments["description"].(string)
	code, _ := req.Arguments["code"].(string)
	rawParams, hasParams := req.Arguments["parameters"].(map[string]any)

	if !toolNamePattern.MatchString(name) {
		return fail(fmt.Sprintf("工具名 %q 无效。需为 snake_case（小写字母开头，只含字母数字下划线）", name))
	}
	if description == "" {
		return fail("description 不能为空")
	}
	if !hasParams || rawParams == nil {
		return fail("parameters 必须是 JSON Schema 对象")
	}
	if code == "" {
		return fail("code 不能为空")
	}
	if c.registry.Has(name) {
		return fail(fmt.Sprintf("工具名 %q 已存在。请换一个名称", name))
	}

	// 静态沙箱检查：含禁用 API / 无界循环 / 保留标识符的代码在创建期即拒绝。
	tokens, err := scanJS(code)
	if err != nil {
		return fail(fmt.Sprintf("代码无法通过沙箱静态分析: %v", err))
	}
	if err := scanViolations(tokens); err != nil {
		return fail(fmt.Sprintf("代码违反沙箱策略: %v", err))
	}
	for _, tok := range tokens {
		if tok.kind == tokIdent {
			if why, reserved := sandboxReserved[tok.text]; reserved && !precededByDot(tokens, tok) {
				return fail(fmt.Sprintf("代码违反沙箱策略: 标识符 %q 是%s，禁止使用", tok.text, why))
			}
		}
	}

	params, err := schemaFromRaw(rawParams)
	if err != nil {
		return fail(fmt.Sprintf("parameters 不是合法的 JSON Schema: %v", err))
	}

	tool := NewDynamicTool(name, description, params, code, c.sandbox)
	if err := c.registry.Register(tool); err != nil {
		return fail(fmt.Sprintf("注册工具失败: %v", err))
	}

	return ToolResponse{
		ToolCallID:  req.ToolCallID,
		ToolName:    "create_tool",
		Content:     fmt.Sprintf("✅ 工具 `%s` 已创建成功（沙箱策略已静态校验）。\n\n描述：%s\n\n后续步骤可直接调用 `%s`。", name, description, name),
		Success:     true,
		DisplayType: "text",
		Metadata:    map[string]any{"new_tool_definition": tool.Definition()},
	}
}

// schemaFromRaw 把 LLM 传入的 JSON Schema（map 形式）转换为 JSONSchema。
// 只转换工具定义用到的子集，其余字段忽略。
func schemaFromRaw(raw map[string]any) (JSONSchema, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return JSONSchema{}, err
	}
	var schema JSONSchema
	if err := json.Unmarshal(data, &schema); err != nil {
		return JSONSchema{}, err
	}
	if schema.Type == "" {
		schema.Type = "object"
	}
	return schema, nil
}
