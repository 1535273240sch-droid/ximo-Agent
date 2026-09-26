package quota

import "math"

const (
	// DefaultPriceMicroPerKTok 没有配置价格时使用的默认单价：1 微单位 / 1K token（§6）。
	// 价值极小，但它保证「未定价的模型也会占用一次预占」，从而让账户被冻结、额度耗尽的
	// 场景仍然走同一条拒绝路径，而不是因为估算为 0 悄悄放行。
	DefaultPriceMicroPerKTok int64 = 1

	// charsPerToken 字符数→token 数的粗估系数，见 EstimateInputTokens。
	charsPerToken = 4
)

// EstimateInputTokens 用「字符数 / 4」粗估输入 token 数（向上取整，宁可多占）。
//
// 这是**粗估**，不是计数：中英混排、代码、JSON 的真实 token 数会明显偏离 4 字符/token
// （中文常接近 1 字符/token，英文散文约 4，代码与 JSON 更高）。它只用于请求发出**之前**
// 预占一个上界，避免请求跑到上游才发现额度不够；**绝不用作计费依据**——结算一律用上游
// 返回的真实 usage（见 Settle）。
func EstimateInputTokens(chars int) int64 {
	if chars <= 0 {
		return 0
	}
	tokens := chars / charsPerToken
	if chars%charsPerToken != 0 {
		tokens++
	}
	return int64(tokens)
}

// EstimateMicro 按 §6 公式估算预占金额（微单位）：
//
//	(inputTokensEstimate + maxOutputTokens) * priceMicroPerKTok / 1000
//
// priceMicroPerKTok <= 0 时取 DefaultPriceMicroPerKTok；整数除法向下取整（与契约公式一致，
// 不足 1 微单位的零头不进位）。溢出时饱和到 MaxInt64：预占必然因额度不足而失败，
// 而不是回绕成一个看似合理的小数字。
func EstimateMicro(inputTokensEstimate, maxOutputTokens, priceMicroPerKTok int64) int64 {
	if inputTokensEstimate < 0 {
		inputTokensEstimate = 0
	}
	if maxOutputTokens < 0 {
		maxOutputTokens = 0
	}
	if priceMicroPerKTok <= 0 {
		priceMicroPerKTok = DefaultPriceMicroPerKTok
	}
	if maxOutputTokens > math.MaxInt64-inputTokensEstimate {
		return math.MaxInt64
	}
	tokens := inputTokensEstimate + maxOutputTokens
	if tokens == 0 {
		return 0
	}
	if tokens > math.MaxInt64/priceMicroPerKTok {
		return math.MaxInt64
	}
	return tokens * priceMicroPerKTok / 1000
}

// CostMicro 用上游真实 usage 计算成本（微单位）：
//
//	(inputTok + outputTok) * priceMicroPerKTok / 1000
//
// 与 EstimateMicro 共用同一段算术，保证「估算」与「结算」不会因为取整方式不同而漂移
// ——预占金额按同一公式算出的成本一定落得进预占里。
func CostMicro(inputTok, outputTok, priceMicroPerKTok int64) int64 {
	return EstimateMicro(inputTok, outputTok, priceMicroPerKTok)
}
