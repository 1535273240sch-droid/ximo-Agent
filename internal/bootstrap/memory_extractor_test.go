package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
)

// 本文件是 provider → memory.Extractor 适配层的测试：严格 JSON 抽取要走通，
// 失败必须**以错误的形式**交回 memory 包（由它回退规则抽取），而不是抛给 run。

// extractorFakeProvider 是只实现 ports.Provider 的脚本化替身。
type extractorFakeProvider struct {
	resp ports.ProviderResponse
	err  error
	reqs []ports.ProviderRequest
}

func (p *extractorFakeProvider) Complete(_ context.Context, req ports.ProviderRequest) (ports.ProviderResponse, error) {
	p.reqs = append(p.reqs, req)
	return p.resp, p.err
}

func TestMemoryProviderExtractorParsesStrictJSON(t *testing.T) {
	p := &extractorFakeProvider{resp: ports.ProviderResponse{
		FinishReason: ports.FinishStop,
		Content: "好的：\n```json\n" +
			`{"episode":{"title":"配色","summary":"改成暗色"},` +
			`"facts":[{"text":"用户偏好深色主题","importance":0.8,"entities":["XimoAgent"]}]}` +
			"\n```",
	}}
	x := memoryProviderExtractor{provider: p, model: "test-model", timeout: time.Second}

	ex, err := x.Extract(context.Background(), "材料：用户说界面要深色")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if ex.Episode == nil || ex.Episode.Title != "配色" {
		t.Fatalf("episode = %+v", ex.Episode)
	}
	if len(ex.Facts) != 1 || ex.Facts[0].Text != "用户偏好深色主题" {
		t.Fatalf("facts = %+v", ex.Facts)
	}
	if len(p.reqs) != 1 {
		t.Fatalf("应当恰好调用模型一次，实际 %d 次", len(p.reqs))
	}
	req := p.reqs[0]
	if req.Model != "test-model" {
		t.Fatalf("请求模型 = %q，期望 test-model", req.Model)
	}
	if req.MaxTokens != memoryExtractionMaxTokens {
		t.Fatalf("抽取输出上限 = %d，期望 %d", req.MaxTokens, memoryExtractionMaxTokens)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != ports.RoleUser {
		t.Fatalf("抽取请求应是一条 user 消息，实际 %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "深色") {
		t.Fatalf("提示词应原样透传，实际 %q", req.Messages[0].Content)
	}
}

func TestMemoryProviderExtractorFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		x    memoryProviderExtractor
	}{
		{"没有 provider", memoryProviderExtractor{}},
		{"提示词为空", memoryProviderExtractor{provider: &extractorFakeProvider{}}},
		{"传输错误", memoryProviderExtractor{
			provider: &extractorFakeProvider{err: errors.New("connection refused")},
		}},
		{"服务商拒绝", memoryProviderExtractor{
			provider: &extractorFakeProvider{resp: ports.ProviderResponse{
				FinishReason: ports.FinishError, Error: "rate limited",
			}},
		}},
		{"空内容", memoryProviderExtractor{
			provider: &extractorFakeProvider{resp: ports.ProviderResponse{FinishReason: ports.FinishStop}},
		}},
		{"非 JSON 输出", memoryProviderExtractor{
			provider: &extractorFakeProvider{resp: ports.ProviderResponse{
				FinishReason: ports.FinishStop, Content: "我觉得这条记忆挺重要的",
			}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt := "材料：随便写点什么"
			if tc.name == "提示词为空" {
				prompt = "   "
			}
			if _, err := tc.x.Extract(context.Background(), prompt); err == nil {
				t.Fatal("必须返回错误，让 memory 包回退规则抽取")
			}
		})
	}
}

// 服务商回显的错误文本进日志前必须脱敏。
func TestMemoryProviderExtractorRedactsProviderError(t *testing.T) {
	x := memoryProviderExtractor{provider: &extractorFakeProvider{resp: ports.ProviderResponse{
		FinishReason: ports.FinishError,
		Error:        "invalid api key: sk-live-abcdef0123456789",
	}}}
	_, err := x.Extract(context.Background(), "材料")
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if strings.Contains(err.Error(), "sk-live-abcdef0123456789") {
		t.Fatalf("错误文本泄漏了密钥: %v", err)
	}
}

func TestSynapseOptionsFromMapsConfig(t *testing.T) {
	cfg := memory.DefaultConfig()
	cfg.UserID = "graph-user"
	cfg.AgentID = "agent-1"
	cfg.TopK = 9
	cfg.RecallMaxChars = 777
	cfg.ExtractTimeout = 3 * time.Second
	cfg.QueueDepth = 7

	opts := synapseOptionsFrom(cfg)
	if opts.UserID != "graph-user" || opts.AgentID != "agent-1" {
		t.Fatalf("归属映射错误: %+v", opts)
	}
	if opts.TopK != 9 || opts.RecallMaxChars != 777 {
		t.Fatalf("召回预算映射错误: %+v", opts)
	}
	if opts.ExtractTimeout != 3*time.Second || opts.QueueDepth != 7 {
		t.Fatalf("超时 / 队列映射错误: %+v", opts)
	}
	// 零值语义：没配的项交给 SynapseOptions.withDefaults 补齐（这里只验不为负）。
	if opts.ConsolidateTimeout < 0 {
		t.Fatalf("整理超时应为 0（用默认值），实际 %v", opts.ConsolidateTimeout)
	}
}

// memoryAdapter 的结构化召回必须真的转发到 Service，并且带上 via 路径。
func TestMemoryAdapterRecallDetailedForwardsItems(t *testing.T) {
	ctx := context.Background()
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.Backend = memory.BackendSynapse
	cfg.UserID = "adapter-user"

	sb, err := memory.NewSynapseBackend(filepath.Join(t.TempDir(), "memory.db"), memory.SynapseOptions{
		UserID: "adapter-user",
	})
	if err != nil {
		t.Fatalf("打开 synapse 后端: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	if _, err := sb.Add(ctx, []memory.Message{
		{Role: "user", Content: "项目偏好深色主题，界面都用暗色"},
	}, memory.AddOptions{RunID: "r-adapter"}); err != nil {
		t.Fatalf("写入记忆: %v", err)
	}

	svc := memory.NewService(cfg, sb, nil)
	t.Cleanup(svc.Close)
	adapter := memoryAdapter{svc: svc}

	text, items := adapter.RecallDetailed(ctx, "深色主题")
	if text == "" {
		t.Fatal("应渲染出注入文本")
	}
	if len(items) == 0 {
		t.Fatal("应返回结构化召回条目（memory.recalled 事件的来源）")
	}
	if items[0].ID == "" || items[0].Text == "" {
		t.Fatalf("条目缺少 id / 正文: %+v", items[0])
	}
	if items[0].Via != "seed" {
		t.Fatalf("词法命中的条目 via 应为 seed，实际 %q", items[0].Via)
	}
	if !strings.Contains(text, "深色主题") {
		t.Fatalf("注入文本应包含记忆正文，实际 %q", text)
	}

	// 未装配服务时是空结果，不 panic。
	var bare memoryAdapter
	if got, gotItems := bare.RecallDetailed(ctx, "任意"); got != "" || gotItems != nil {
		t.Fatalf("未装配时应返回空，实际 %q / %+v", got, gotItems)
	}
}
