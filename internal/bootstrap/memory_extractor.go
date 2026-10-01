package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// memoryExtractionMaxTokens 是单次记忆抽取允许的输出上限。
//
// 抽取协议本身很小（最多 8 条 fact + 一个 episode 摘要），1200 token 足够，
// 而它比默认输出上限小得多：这是后台收尾路径上的一次附加调用，不该按一次完整
// 回答的预算去请求。
const memoryExtractionMaxTokens = 1200

// memoryProviderExtractor 把当前生效的 provider 适配成 memory.Extractor。
//
// 它实现的是"用模型做一次严格 JSON 抽取"这一层：提示词由 memory 包按文档 4.4
// 第 3 条构造好传进来，这里只负责调模型、把回复解析成 SynapseExtraction。
//
// 失败策略与整个记忆特性一致——**绝不报错上抛到 run**：
//   - provider 未配置 / 调用失败 / 超时 / 服务商拒绝 / JSON 解析失败，一律返回
//     错误；memory 包收到错误后回退规则抽取（把一问一答直存为一条低重要度 fact），
//     不重试、不阻塞 run 收尾。
//   - 因此这里的错误是"给日志看的诊断信息"，不是调用方需要处理的分支。
//
// provider 用的是装配期注入的那个接口值（bootstrap 里是 swappableProvider），
// 所以用户在设置页换密钥 / 换模型之后，抽取也会跟着用新的客户端。
type memoryProviderExtractor struct {
	provider ports.Provider
	// model 是配置里的默认模型；为空时由 provider 适配器回退到它自己的默认值。
	model string
	// timeout 是单次抽取的超时（memCfg.ExtractTimeout）。
	timeout time.Duration
}

var _ memory.Extractor = memoryProviderExtractor{}

// Extract 实现 memory.Extractor。
func (x memoryProviderExtractor) Extract(ctx context.Context, prompt string) (memory.SynapseExtraction, error) {
	if x.provider == nil {
		return memory.SynapseExtraction{}, errors.New("bootstrap: 记忆抽取没有可用的 provider")
	}
	if strings.TrimSpace(prompt) == "" {
		return memory.SynapseExtraction{}, errors.New("bootstrap: 记忆抽取提示为空")
	}
	timeout := x.timeout
	if timeout <= 0 {
		timeout = memory.DefaultSynapseExtractTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Effort 留空 = 关闭思考模式：这是一次格式化的信息抽取，不是需要推理的任务。
	resp, err := x.provider.Complete(ctx, ports.ProviderRequest{
		Model: x.model,
		Messages: []ports.Message{{
			Role:    ports.RoleUser,
			Content: prompt,
		}},
		MaxTokens: memoryExtractionMaxTokens,
	})
	if err != nil {
		return memory.SynapseExtraction{}, fmt.Errorf("bootstrap: 记忆抽取调用模型失败: %w", err)
	}
	if resp.FinishReason == ports.FinishError {
		// resp.Error 可能带着服务商回显的请求片段，脱敏后再进日志。
		return memory.SynapseExtraction{}, fmt.Errorf("bootstrap: 记忆抽取被服务商拒绝: %s",
			redactProviderError(resp.Error))
	}
	if strings.TrimSpace(resp.Content) == "" {
		return memory.SynapseExtraction{}, errors.New("bootstrap: 记忆抽取返回空内容")
	}
	return memory.ParseExtractionJSON(resp.Content)
}

// redactProviderError 保证服务商错误文本进入日志前已经脱敏。
func redactProviderError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "（服务商未给出错误信息）"
	}
	return types.RedactString(s)
}
