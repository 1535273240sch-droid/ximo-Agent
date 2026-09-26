// Package anthropic 实现 XIMO 中转站的 Anthropic Messages 入口（POST /v1/messages）。
//
// V1 的上游只有 openai-chat（契约 §11.1.4），所以本包做的是**协议翻译**：
// Anthropic Messages 入站 → OpenAI chat 上游（internal/provider + upstream.Pool）→
// Anthropic Messages 出站（非流式 JSON / 流式 SSE，事件序列见契约 §11.5）。
//
// 翻译映射（逐条实现点见 translate.go）：
//
//	入站：system(string | text block 数组) → 首条 system 消息
//	      messages[].content 的 text / tool_use / tool_result block → OpenAI 消息与 tool_calls
//	      tools[].{name,description,input_schema} → OpenAI function 形状
//	      max_tokens（必填）→ max_tokens；temperature → temperature；stop_sequences → 见下
//	出站：choices[].message.content → content:[{type:"text",text:...}]
//	      tool_calls → content:[{type:"tool_use",id,name,input}]
//	      finish_reason stop|length|tool_calls → stop_reason end_turn|max_tokens|tool_use
//	      usage.prompt_tokens/completion_tokens → input_tokens/output_tokens
//
// 已知缺口（不假装支持，逐条都有明确行为或注释）：
//
//  1. **图片**：provider.Message.Content 是 string，冻结的 provider 客户端没有任何
//     content-parts 入口（多模态只存在于 provider.AnalyzeImages 这条固定提示词的旁路），
//     因此 image block 无法转发。本包选择**显式 400 拒绝**（code=unsupported_content_block），
//     而不是静默丢图 —— 丢了图会让模型基于残缺上下文给出错误答案，比报错更难排查。
//  2. **stop_sequences**：provider.CompletionRequest 没有 Stop 字段，无法把 stop 传给上游。
//     本包在**输出侧按 stop 序列截断**并回报 stop_reason=stop_sequence（客户端可见语义一致），
//     差别是上游仍会生成到自然结束；流式下截断时上游 usage 可能尚未上报，此时按已知 usage 结算。
//  3. **tool_choice**：provider.BuildRequestBody 在有 tools 时把 tool_choice 硬编码为 "auto"，
//     故 {"type":"any"} / {"type":"tool"} 无法转发；本包降级为 auto 并记 Warn 日志（不静默）。
//  4. **top_p / metadata**：冻结的请求类型没有对应字段，忽略并记 Warn 日志；
//     `thinking`（扩展思考参数）同样未解析、直接忽略（无对应上游参数，也不产生告警）。
//  5. 流式 stop_reason：优先用上游显式报告的 finish_reason 映射（length→max_tokens、
//     tool_calls→tool_use、stop→end_turn，见 stream.go 的 streamStopReason）；只有上游
//     整条流都不报 finish_reason 时才回退到推断（outTokens >= max_tokens → max_tokens，
//     有工具调用 → tool_use，否则 end_turn）。
//
// 额度与计费链路（契约 §11.4/§11.5）与 api/openai 必须一致：认证 → 模型启用校验 → 预占 →
// 候选路由（协议过滤 openai-chat）→ 逐个候选尝试 → 结算/释放 + 落 usage。本轮 api/openai
// 尚未落地（目录内不存在），本包先把这套执行器写在自己包里（handler.go / stream.go），
// 待其暴露共享执行器后两边应收敛为一份，避免同一生命周期出现两份实现。
package anthropic
