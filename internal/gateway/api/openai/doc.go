// Package openai 实现 XIMO 中转站的 OpenAI 兼容入口：POST /v1/chat/completions
// （流式与非流式）。
//
// 职责边界（契约 §11.2）：本包只做「对外 OpenAI 协议 ↔ 内部 provider 调用」的转换与
// 请求生命周期编排。鉴权实现、限流实现、目录存储、额度算法、上游客户端构建分别由
// W-Server 中间件、catalog、quota、upstream 承担，经 Deps 注入本包，本包不重复实现。
//
// 请求生命周期（契约 §11.4，顺序不可变）：
//
//	request_id → 认证 → 限流 → 模型启用校验 → 额度预占 → 候选路由（过滤协议/健康）
//	→ 逐个候选尝试 → 成功则结算 + 写 usage → 返回
//
// 其中限流由中间件完成（本包不实现 per-user 限流，避免两套阈值打架）；认证只消费
// Deps.Auth 的结果，不解析 Authorization 头。
//
// 额度硬要求：预占一旦成功，除了「成功结算」之外的**任何**返回路径（模型无可用上游、
// 候选全失败、上游不可重试错误、客户端取消、panic 展开）都必须归还预占，否则用户额度
// 会被永久占住。该不变量由 charge 类型集中保证（charge.go），不靠各分支自觉。
//
// 流式约定（契约 §11.5）：
//   - 首字节前失败：不写 SSE 头，回普通 JSON 错误 + 非 2xx（客户端能判定）；
//   - 首字节后失败：发一条错误事件后终止（不发 [DONE]：让「失败」与「正常收尾」在
//     协议层不可混淆），并落 usage 行标明终态；
//   - 正常收尾：finish_reason 分片 → 可选 usage 分片 → data: [DONE]。
//     finish_reason 优先取上游显式报告的值（length / tool_calls 均如实透出）；上游整条流
//     都没报时才回退到推断（出现过工具调用增量 → tool_calls，否则 stop，见 stream.go）。
//
// 诚实标注的未实现/口径（不做假实现）：
//   - 无真实价目表（§11.6）：V1 所有模型按占位单价结算，见 Deps.PriceMicroPerKTok；
//     出账金额只用于验证额度链路，不是账单。
//   - 上下文窗口门控不做（§11.1.8：ModelSpec 没有 context window 字段）。
//   - 图像/音频输入不做：出现 image_url/input_audio 内容块直接 400，不静默丢内容。
//   - 无法转发的参数一律显式 400（n、logprobs、response_format、audio、modalities、
//     stop、legacy functions 等），只有纯遥测/调优类字段（user、top_p、penalties、
//     seed 等）被忽略——见 request.go 的说明。
//   - /v1/responses 不做（§11.1.9）。
package openai
