// spec.go —— 目录条目到 provider.Client 装配参数的映射与校验。
//
// 网关的 provider 是运行期数据（数据库行），而 internal/provider 的 ProviderConfig
// 是编译期结构，本文件是两者之间唯一的转换点：所有「不可用的 provider 要在构建期
// 就报错，而不是发出去变成一个看不懂的 HTTP 错误」的规则都集中在这里。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// chatCompletionsSuffix provider.Client 会自动追加的路径。
// Endpoint 约定填 base URL；这里做一次容错剔除，避免运营方粘贴完整 URL 时变成
// /v1/chat/completions/chat/completions。
const chatCompletionsSuffix = "/chat/completions"

// defaultFailureThreshold / defaultOpenTimeout 单 provider 熔断器缺省值，
// 与 bootstrap 装配单活跃 provider 时的取值保持一致。
const (
	defaultFailureThreshold = 5
	defaultOpenTimeout      = 30 * time.Second
)

// defaultMaxOutputTokens 请求未指定 max_tokens 时的兜底值。
const defaultMaxOutputTokens = 8192

// clientConfig 把 ProviderSpec 映射为 provider.ClientOptions 的配置与超时。
//
// 校验失败返回错误（不 panic）：调用方拿到的是「这个 provider 为什么不能用」的明确原因。
func clientConfig(spec model.ProviderSpec, opts Options) (provider.ProviderConfig, time.Duration, error) {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		return provider.ProviderConfig{}, 0, fmt.Errorf("upstream: provider ID 为空")
	}
	if spec.Status == model.ProviderStatusDisabled {
		return provider.ProviderConfig{}, 0, fmt.Errorf("upstream: provider %s 已禁用: %w", id, model.ErrDisabled)
	}

	proto := strings.TrimSpace(spec.Protocol)
	if proto == "" {
		// 目录里未显式声明协议时按 OpenAI 兼容处理（V1 只有这一种上游）。
		proto = model.ProtocolOpenAIChat
	}
	if proto != model.ProtocolOpenAIChat {
		return provider.ProviderConfig{}, 0, fmt.Errorf("upstream: provider %s 协议 %q 暂不支持: %w", id, proto, ErrUnsupportedProtocol)
	}

	endpoint, err := normalizeEndpoint(spec.Endpoint)
	if err != nil {
		return provider.ProviderConfig{}, 0, fmt.Errorf("upstream: provider %s: %w", id, err)
	}

	if strings.TrimSpace(spec.APIKeyRef) == "" {
		// 上游密钥一律走 internal/secrets 的引用，明文不落库、不落配置。
		return provider.ProviderConfig{}, 0, fmt.Errorf("upstream: provider %s 未配置 APIKeyRef: %w", id, ErrMissingAPIKeyRef)
	}

	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = id
	}

	timeout := time.Duration(spec.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		// 0 交给 provider 侧取默认（5 分钟）。
		timeout = opts.DefaultTimeout
	}

	cfg := provider.ProviderConfig{
		ID:           id,
		Name:         name,
		BaseURL:      endpoint,
		IsDeepSeek:   false,
		Capabilities: capabilitiesFor(spec, opts),
		SecretRef:    strings.TrimSpace(spec.APIKeyRef),
		// MaxOutputTokens 是「请求没给 max_tokens」时的兜底：provider 侧会做
		// ClampMaxTokens 钳制，缺省 0 会退化成 max_tokens=1，等于把上游截断。
		// ContextWindow 则留给调用方（网关按模型目录做门控），此处不声明。
		MaxOutputTokens: defaultMaxOutputTokens,
	}
	return cfg, timeout, nil
}

// normalizeEndpoint 校验并规整上游 base URL。
func normalizeEndpoint(raw string) (string, error) {
	ep := strings.TrimSpace(raw)
	if ep == "" {
		return "", ErrMissingEndpoint
	}
	ep = strings.TrimRight(ep, "/")
	if strings.HasSuffix(ep, chatCompletionsSuffix) {
		ep = strings.TrimRight(strings.TrimSuffix(ep, chatCompletionsSuffix), "/")
	}
	if ep == "" {
		return "", ErrMissingEndpoint
	}

	parsed, err := url.Parse(ep)
	if err != nil {
		return "", fmt.Errorf("Endpoint %q 不是合法 URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("Endpoint %q 必须使用 http/https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("Endpoint %q 缺少主机名", raw)
	}
	return ep, nil
}

// capabilitiesFor 解析上游能力开关。
//
// 默认与既有单活跃 provider 装配一致（provider.DefaultCapabilities）：
// 发送 stream_options.include_usage —— 网关结算依赖上游返回的真实 usage，
// 缺了它只能按估算扣费；同时保留 reasoning_content 与思考参数。
// 第三方端点不接受这些扩展字段时，可在 ProviderSpec.ConfigJSON 里逐 provider 关掉：
//
//	{"capabilities":{"stream":true,"reasoning":false}}
//
// 键名与 ModelSpec.CapabilitiesJSON 的约定保持一致；解析失败或字段缺失退回默认。
func capabilitiesFor(spec model.ProviderSpec, opts Options) provider.Capabilities {
	caps := opts.Capabilities
	if caps == (provider.Capabilities{}) {
		caps = provider.DefaultCapabilities()
	}
	if parsed, ok := parseCapabilities(spec.ConfigJSON); ok {
		caps = parsed
	}
	return caps
}

// capabilityConfig ConfigJSON 里允许出现的能力开关。
type capabilityConfig struct {
	Capabilities *struct {
		Stream    *bool `json:"stream"`
		Reasoning *bool `json:"reasoning"`
	} `json:"capabilities"`
}

func parseCapabilities(configJSON string) (provider.Capabilities, bool) {
	raw := strings.TrimSpace(configJSON)
	if raw == "" {
		return provider.Capabilities{}, false
	}
	var cfg capabilityConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil || cfg.Capabilities == nil {
		return provider.Capabilities{}, false
	}
	caps := provider.DefaultCapabilities()
	if cfg.Capabilities.Stream != nil {
		caps.SendStreamUsage = *cfg.Capabilities.Stream
	}
	if cfg.Capabilities.Reasoning != nil {
		caps.SendReasoningParams = *cfg.Capabilities.Reasoning
	}
	return caps, true
}

// fingerprint 计算影响客户端构造的配置摘要。
//
// 只覆盖「改了就必须重建客户端」的字段：endpoint / 协议 / 密钥引用 / 超时 / 能力配置。
// 不含 UpdatedAt —— 改个展示名不该丢掉健康状态，但 ConfigJSON 变了必须重建。
func fingerprint(spec model.ProviderSpec) string {
	h := sha256.New()
	for _, field := range []string{
		spec.ID,
		spec.Endpoint,
		spec.Protocol,
		spec.Status,
		spec.APIKeyRef,
		spec.ConfigJSON,
		strconv.FormatInt(spec.TimeoutMS, 10),
	} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// endpointHost 返回 Endpoint 的 scheme://host，供日志使用。
//
// 刻意不记录完整 URL：部分自建网关把凭据放在查询串里（?api-key=...），
// 完整 URL 进日志即等于泄露。
func endpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
