// Package gateway 提供 XIMO 中转站的**真二进制**端到端验收（契约 §12.5）。
//
// 为什么必须用真二进制：仓库里 internal/gateway/api/* 的单测都把 handler 直接
// 挂在 httptest 上跑，mock 掉的是网关**自身**的 HTTP 层（路由注册、ServeMux 的
// 路径参数、中间件链、鉴权装配、优雅停机、配置解析、迁移、SQLite 打开方式）。
// 那些恰恰是「装配」而不是「逻辑」，也正是最容易出错的部位 —— 路由没注册、中间件
// 顺序反了、flag 名写错、迁移目录探测不到，在包内单测里全都是绿的。
// 本包因此只允许两种组件：真实编译出来的 ximo-gateway.exe，以及作为**上游**的
// httptest 假服务（上游不是被测系统，用假服务是允许且必要的：我们才能控制它返回
// 什么、并数它被打了几次）。
//
// 本包证明（每条都有断言，没有恒真用例）：
//  1. 迁移 + 启动 + /v1/health 可用（真进程、真 SQLite、真端口）。
//  2. 管理面写入链路可用：建用户、幂等充值、建 provider（密钥经 internal/secrets
//     落地）、建模型与 provider→模型映射。
//  3. 两条用户凭据链路都可用：口令登录换 access token（gwa_）与 /admin/keys
//     签发的 API Key（ximo_sk_）。
//  4. 非流式与流式 /v1/chat/completions 都能打通真 HTTP → 真 provider.Client →
//     假上游，且响应形状、内容、usage 与上游报的一致。
//  5. 额度账本可对账：直接读同一个 SQLite 文件，账本累计额 == 账户 Available()，
//     且账本累计额 == 充值额 - 已记用量成本之和（两个角度互相印证）。
//  6. 幂等充值：同 idempotency_key 提交两次，账本只有一行、余额只增一次。
//  7. 「额度不足 402」与「模型不存在 404」都**不进上游**（用假上游命中计数证明）。
//  8. 日志不泄漏凭据：进程 stdout/stderr 里既没有 admin token 也没有上游密钥。
//
// 另有三个回归用例（见文件下半部分的「回归用例」段）：
//  9. 上游把 usage 分片放在 finish_reason **之后**时仍能拿到真实 token 并正确计费
//     （D1 修复的回归防线）。
//  10. 从未做过额度操作的**全新用户**直接请求 → 402 insufficient_quota，且不进上游
//     （D5 修复的回归防线；旧用例靠 topup 0 绕过了这条真实路径）。
//  11. 上游 5xx 的请求最终不留 held 预占：预占被主动归还（released），账本仍可对账。
//
// 本包刻意不做：
//   - 不测并发压测与故障注入（tests/stress、tests/chaos 各自的职责）。
//   - 不测 Anthropic 入口（/v1/messages 有自己的包内单测与集成测试）。
//   - TestGatewayEndToEnd **不假设流式 usage 一定有值**：它的假上游把 usage 分片放在
//     finish_reason 之前，因此断言只硬性要求「有 settled 的 usage 行」，token 数值
//     只做记录（尾随 usage 的硬断言在下面的回归用例里，见
//     TestGatewayTrailingUsageIsSettled）。
package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// driver 注册走仓库既有的存储包（DriverName = "sqlite"），不引入新依赖，
	// 也不与生产代码各自硬编码驱动名。
	_ "github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

const (
	// adminToken 是启动参数里的管理令牌。它同时出现在「进程命令行」与
	// 「X-Admin-Token 头」，因此也是泄漏检查的被查串之一。
	adminToken = "e2e-admin-token-0a3f9c"

	// 占位单价取 1000 微单位/1K token，让金额量级看得见（默认 1 时一次请求
	// 只花个位数微单位，断言里全是 0 与 1，读起来没有信息量）。
	priceMicroPerKTok = 1000
	maxOutputTokens   = 256
	// topupMicro 是主用户的充值额，远大于一次请求的预占估算（≈256 微单位），
	// 保证「失败原因一定是额度不足」而不是别的（例如估算超过余额）。
	topupMicro int64 = 1_000_000

	userName     = "e2e-user"
	userPassword = "e2e-user-pass"
	poorUser     = "e2e-poor"

	modelID         = "e2e-model"
	upstreamModelID = "e2e-upstream-model"
	providerID      = "e2e-provider"

	topupKey     = "e2e-topup-main"
	poorTopupKey = "e2e-topup-poor"

	upstreamAPIKey = "e2e-upstream-secret-key"

	// 假上游固定上报的 usage 与答复，用来对齐响应体、gw_usage 与账本三处数字。
	upstreamPromptTokens     = 11
	upstreamCompletionTokens = 5
	upstreamAnswer           = "hello from the fake upstream"

	// 失败用例必须用同一套凭据打同一个模型，只有被验条件不同，
	// 这样「上游命中计数没变」才能唯一归因到该条件。
	missingModelID = "e2e-no-such-model"

	startupTimeout = 45 * time.Second
	buildTimeout   = 5 * time.Minute
)

// ---------------------------------------------------------------------------
// 测试主体
// ---------------------------------------------------------------------------

func TestGatewayEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端验收需要真实编译并启动 ximo-gateway，-short 下跳过")
	}

	root := repoRoot(t)
	exe := buildGateway(t, root)

	up := newFakeUpstream(t)
	gw := startGateway(t, root, exe, up)

	hdr := func() map[string]string { return map[string]string{"X-Admin-Token": adminToken} }

	// ---------------------------------------------------------------- 1. 存活
	health := gw.doJSON(t, http.MethodGet, "/v1/health", nil, nil)
	if health.Status != http.StatusOK {
		t.Fatalf("GET /v1/health: 期望 200，得到 %d，body=%s", health.Status, health.Body)
	}
	var healthBody struct {
		Status string `json:"status"`
	}
	mustUnmarshal(t, health.Body, &healthBody)
	if healthBody.Status != "ok" {
		t.Fatalf("GET /v1/health: status=%q，期望 ok", healthBody.Status)
	}
	// 无鉴权端点必须真的无鉴权（否则插件连登录都做不到）。
	if caps := gw.doJSON(t, http.MethodGet, "/v1/capabilities", nil, nil); caps.Status != http.StatusOK {
		t.Fatalf("GET /v1/capabilities: 期望 200，得到 %d，body=%s", caps.Status, caps.Body)
	}

	// ---------------------------------------------------------------- 2. 建用户
	userID := createUser(t, gw, hdr(), userName, userPassword)
	poorID := createUser(t, gw, hdr(), poorUser, "e2e-poor-pass")
	if userID == poorID {
		t.Fatalf("两个用户拿到同一个 ID %q", userID)
	}

	// 管理面鉴权本身要立得住：错令牌必须 401（fail-closed 的反面验证）。
	if bad := gw.doJSON(t, http.MethodGet, "/admin/users?limit=1",
		map[string]string{"X-Admin-Token": "wrong-token"}, nil); bad.Status != http.StatusUnauthorized {
		t.Fatalf("错误管理令牌: 期望 401，得到 %d，body=%s", bad.Status, bad.Body)
	}

	// ---------------------------------------------------------------- 3. 幂等充值
	// 同一个 idempotency_key 提交两次：第二次必须返回同一行、且不再入账。
	first := adjustQuota(t, gw, hdr(), userID, topupKey, topupMicro)
	second := adjustQuota(t, gw, hdr(), userID, topupKey, topupMicro)
	if first.Ledger.ID == "" || second.Ledger.ID != first.Ledger.ID {
		t.Fatalf("幂等充值: 两次返回不同账本行（first=%q second=%q）", first.Ledger.ID, second.Ledger.ID)
	}
	if second.Account == nil {
		t.Fatalf("幂等充值: 第二次响应缺少账户快照")
	}
	if got := second.Account.Available; got != topupMicro {
		t.Fatalf("幂等充值: 余额只应增一次，期望 %d，得到 %d", topupMicro, got)
	}

	// 贫穷用户走的是「有额度账户但可用额为 0」这条路（充值 0 即可，可用额保持 0），
	// 与「账户行本身不存在」是两条不同的路径：后者由本节新增的
	// TestGatewayUnfundedUserGets402 直测（D5 修复前那条路径返回 404 而不是 402）。
	poor := adjustQuota(t, gw, hdr(), poorID, poorTopupKey, 0)
	if poor.Account == nil || poor.Account.Available != 0 {
		t.Fatalf("贫穷用户建户: 期望 available=0，得到 %+v", poor.Account)
	}

	// ---------------------------------------------------------------- 4. 目录装配
	created := gw.doJSON(t, http.MethodPost, "/admin/providers", hdr(), map[string]any{
		"id":       providerID,
		"name":     "e2e fake upstream",
		"endpoint": up.baseURL(),
		"protocol": "openai-chat",
		"status":   "enabled",
		// 明文密钥从这里进 internal/secrets，库里只应留下 secretref。
		"api_key":    upstreamAPIKey,
		"timeout_ms": 15000,
		"weight":     1,
	})
	if created.Status != http.StatusOK {
		t.Fatalf("POST /admin/providers: 期望 200，得到 %d，body=%s", created.Status, created.Body)
	}
	var prov struct {
		ID        string `json:"id"`
		Endpoint  string `json:"endpoint"`
		Protocol  string `json:"protocol"`
		APIKeyRef string `json:"api_key_ref"`
	}
	mustUnmarshal(t, created.Body, &prov)
	if prov.APIKeyRef == "" || prov.APIKeyRef == upstreamAPIKey {
		t.Fatalf("provider.api_key_ref 必须是 secretref 且不能等于明文，得到 %q", prov.APIKeyRef)
	}
	if !strings.HasPrefix(prov.APIKeyRef, "secretref:") {
		t.Fatalf("provider.api_key_ref 形状不对: %q", prov.APIKeyRef)
	}

	modelCreated := gw.doJSON(t, http.MethodPost, "/admin/models", hdr(), map[string]any{
		"id":           modelID,
		"display_name": "E2E 测试模型",
		"capabilities": map[string]any{"stream": true, "tools": true, "vision": false, "reasoning": false},
		"enabled":      true,
	})
	if modelCreated.Status != http.StatusOK {
		t.Fatalf("POST /admin/models: 期望 200，得到 %d，body=%s", modelCreated.Status, modelCreated.Body)
	}

	mapped := gw.doJSON(t, http.MethodPost, "/admin/providers/"+providerID+"/models", hdr(), map[string]any{
		"model_id":          modelID,
		"upstream_model_id": upstreamModelID,
		"enabled":           true,
		"priority":          0,
	})
	if mapped.Status != http.StatusOK {
		t.Fatalf("POST /admin/providers/{id}/models: 期望 200，得到 %d，body=%s", mapped.Status, mapped.Body)
	}

	// ---------------------------------------------------------------- 5. 凭据
	// 两条链路都要能拿到可用凭据：口令登录（access token）与 /admin/keys（API Key）。
	login := gw.doJSON(t, http.MethodPost, "/v1/auth/login", nil, map[string]any{
		"username": userName,
		"password": userPassword,
	})
	if login.Status != http.StatusOK {
		t.Fatalf("POST /v1/auth/login: 期望 200，得到 %d，body=%s", login.Status, login.Body)
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	mustUnmarshal(t, login.Body, &tokens)
	if !strings.HasPrefix(tokens.AccessToken, "gwa_") || tokens.TokenType != "Bearer" {
		t.Fatalf("登录响应凭据形状不对: token_type=%q access_prefix=%.8s", tokens.TokenType, tokens.AccessToken)
	}

	keyResp := gw.doJSON(t, http.MethodPost, "/admin/keys", hdr(), map[string]any{"user_id": userID})
	if keyResp.Status != http.StatusOK {
		t.Fatalf("POST /admin/keys: 期望 200，得到 %d，body=%s", keyResp.Status, keyResp.Body)
	}
	var apiKey struct {
		APIKey string `json:"api_key"`
		Key    struct {
			KeyPrefix string `json:"key_prefix"`
			Status    string `json:"status"`
		} `json:"key"`
	}
	mustUnmarshal(t, keyResp.Body, &apiKey)
	if !strings.HasPrefix(apiKey.APIKey, "ximo_sk_") {
		t.Fatalf("API Key 明文应以 ximo_sk_ 开头，得到 %.12s", apiKey.APIKey)
	}

	// ---------------------------------------------------------------- 6. /v1/models
	models := gw.doJSON(t, http.MethodGet, "/v1/models",
		map[string]string{"Authorization": "Bearer " + tokens.AccessToken}, nil)
	if models.Status != http.StatusOK {
		t.Fatalf("GET /v1/models: 期望 200，得到 %d，body=%s", models.Status, models.Body)
	}
	var catalog struct {
		Data []struct {
			ID           string   `json:"id"`
			DisplayName  string   `json:"display_name"`
			Provider     string   `json:"provider"`
			Protocols    []string `json:"protocols"`
			Enabled      bool     `json:"enabled"`
			Capabilities struct {
				Stream bool `json:"stream"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	mustUnmarshal(t, models.Body, &catalog)
	var found bool
	for _, m := range catalog.Data {
		if m.ID != modelID {
			continue
		}
		found = true
		if !m.Enabled || m.Provider != providerID || m.DisplayName != "E2E 测试模型" {
			t.Fatalf("/v1/models 条目与目录不一致: %+v", m)
		}
		if len(m.Protocols) != 1 || m.Protocols[0] != "openai-chat" {
			t.Fatalf("/v1/models protocols 期望 [openai-chat]，得到 %v", m.Protocols)
		}
		if !m.Capabilities.Stream {
			t.Fatalf("/v1/models capabilities.stream 期望 true，得到 %+v", m.Capabilities)
		}
	}
	if !found {
		t.Fatalf("/v1/models 未返回模型 %s: %s", modelID, models.Body)
	}

	// ---------------------------------------------------------------- 7. 非流式
	nonStreamReqID := "e2e-nonstream"
	nonStream := gw.doJSON(t, http.MethodPost, "/v1/chat/completions",
		map[string]string{
			"Authorization": "Bearer " + apiKey.APIKey,
			"X-Request-Id":  nonStreamReqID,
		},
		map[string]any{
			"model":    modelID,
			"messages": []map[string]string{{"role": "user", "content": "你好，端到端测试"}},
		})
	if nonStream.Status != http.StatusOK {
		t.Fatalf("非流式 /v1/chat/completions: 期望 200，得到 %d，body=%s", nonStream.Status, nonStream.Body)
	}
	var completion struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	mustUnmarshal(t, nonStream.Body, &completion)
	// 响应 id 是 "chatcmpl-" + request_id：它同时证明中间件采纳了客户端传入的
	// X-Request-Id、handler 复用了同一个 ID。gw_usage 的对账就靠这个 request_id。
	if completion.ID != "chatcmpl-"+nonStreamReqID {
		t.Fatalf("非流式响应 id 期望 chatcmpl-%s，得到 %q", nonStreamReqID, completion.ID)
	}
	if completion.Object != "chat.completion" || completion.Model != modelID {
		t.Fatalf("非流式响应形状不对: object=%q model=%q", completion.Object, completion.Model)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("非流式 choices 长度期望 1，得到 %d", len(completion.Choices))
	}
	choice := completion.Choices[0]
	if choice.Index != 0 || choice.Message.Role != "assistant" || choice.FinishReason != "stop" {
		t.Fatalf("非流式 choice 形状不对: %+v", choice)
	}
	if choice.Message.Content != upstreamAnswer {
		t.Fatalf("非流式内容期望 %q，得到 %q", upstreamAnswer, choice.Message.Content)
	}
	if completion.Usage.PromptTokens != upstreamPromptTokens ||
		completion.Usage.CompletionTokens != upstreamCompletionTokens ||
		completion.Usage.TotalTokens != upstreamPromptTokens+upstreamCompletionTokens {
		t.Fatalf("非流式 usage 与上游上报不一致: %+v", completion.Usage)
	}

	// 假上游确实被打了，且拿到的是「映射后的上游模型名 + 解引用出来的密钥」。
	sent := up.last()
	if sent.Path != "/chat/completions" {
		t.Fatalf("上游收到的路径期望 /chat/completions，得到 %q", sent.Path)
	}
	if sent.Authorization != "Bearer "+upstreamAPIKey {
		t.Fatalf("上游收到的 Authorization 不是解引用后的密钥（说明 internal/secrets 链路没走通）")
	}
	if sent.Model != upstreamModelID {
		t.Fatalf("上游收到的模型期望 %q，得到 %q", upstreamModelID, sent.Model)
	}
	if !sent.Stream {
		t.Fatalf("internal/provider 的非流式路径按约定仍以 stream=true 请求上游，实际收到 stream=false")
	}

	// ---------------------------------------------------------------- 8. 流式
	streamReqID := "e2e-stream"
	streamResp := gw.do(t, http.MethodPost, "/v1/chat/completions",
		map[string]string{
			"Authorization": "Bearer " + apiKey.APIKey,
			"X-Request-Id":  streamReqID,
		},
		map[string]any{
			"model":          modelID,
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			"messages":       []map[string]string{{"role": "user", "content": "流式测试"}},
		})
	defer func() { _ = streamResp.Body.Close() }()
	if streamResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("流式 /v1/chat/completions: 期望 200，得到 %d，body=%s", streamResp.StatusCode, raw)
	}
	if ct := streamResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("流式 Content-Type 期望 text/event-stream，得到 %q", ct)
	}
	streamBody, err := io.ReadAll(streamResp.Body)
	if err != nil {
		t.Fatalf("读取流式响应失败: %v", err)
	}
	payloads := dataPayloads(streamBody)
	if len(payloads) == 0 {
		t.Fatalf("流式响应没有任何 data: 分片: %s", streamBody)
	}
	var (
		streamedContent strings.Builder
		streamUsage     *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		}
		sawDone bool
	)
	for _, p := range payloads {
		if p == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		mustUnmarshal(t, []byte(p), &chunk)
		if chunk.ID != "" && chunk.ID != "chatcmpl-"+streamReqID {
			t.Fatalf("流式分片 id 期望 chatcmpl-%s，得到 %q", streamReqID, chunk.ID)
		}
		if chunk.Usage != nil {
			streamUsage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			streamedContent.WriteString(c.Delta.Content)
		}
	}
	if !sawDone {
		t.Fatalf("流式响应缺少 data: [DONE] 收尾: %s", streamBody)
	}
	if got := streamedContent.String(); got != upstreamAnswer {
		t.Fatalf("流式拼接内容期望 %q，得到 %q", upstreamAnswer, got)
	}
	if streamUsage == nil {
		// 契约 §12.4.1 登记的缺口：不能硬失败，但要显式记录下来。
		t.Logf("提示：流式响应未带 usage 分片（契约 §12.4.1 已登记的 provider 侧缺口），该次结算按 0 token 计")
	} else if streamUsage.PromptTokens != upstreamPromptTokens || streamUsage.CompletionTokens != upstreamCompletionTokens {
		t.Fatalf("流式 usage 与上游上报不一致: %+v", streamUsage)
	}

	// ---------------------------------------------------------------- 9. 402
	hitsBefore := up.hits.Load()
	insufficient := gw.doJSON(t, http.MethodPost, "/v1/chat/completions",
		map[string]string{
			"Authorization": "Bearer " + poorAccessToken(t, gw, poorUser, "e2e-poor-pass"),
			"X-Request-Id":  "e2e-poor-402",
		},
		map[string]any{
			"model":    modelID,
			"messages": []map[string]string{{"role": "user", "content": "我充不起"}},
		})
	if insufficient.Status != http.StatusPaymentRequired {
		t.Fatalf("额度不足期望 402，得到 %d，body=%s", insufficient.Status, insufficient.Body)
	}
	if code := errCode(t, insufficient.Body); code != "insufficient_quota" {
		t.Fatalf("额度不足错误码期望 insufficient_quota，得到 %q", code)
	}
	if up.hits.Load() != hitsBefore {
		t.Fatalf("额度不足时不该进上游：命中计数 %d → %d", hitsBefore, up.hits.Load())
	}

	// ---------------------------------------------------------------- 10. 404
	notFound := gw.doJSON(t, http.MethodPost, "/v1/chat/completions",
		map[string]string{
			"Authorization": "Bearer " + apiKey.APIKey,
			"X-Request-Id":  "e2e-model-404",
		},
		map[string]any{
			"model":    missingModelID,
			"messages": []map[string]string{{"role": "user", "content": "模型不存在"}},
		})
	if notFound.Status != http.StatusNotFound {
		t.Fatalf("模型不存在期望 404，得到 %d，body=%s", notFound.Status, notFound.Body)
	}
	if code := errCode(t, notFound.Body); code != "model_not_found" {
		t.Fatalf("模型不存在错误码期望 model_not_found，得到 %q", code)
	}
	if up.hits.Load() != hitsBefore {
		t.Fatalf("模型不存在时不该进上游：命中计数 %d → %d", hitsBefore, up.hits.Load())
	}

	// ---------------------------------------------------------------- 11. 停进程
	out := gw.stop()
	// 先确认进程真的输出了东西：否则下面的「不泄漏」断言是在空串上通过的，等于没测。
	t.Logf("网关进程输出 %d 字节（用于凭据泄漏检查）", len(out))
	if strings.TrimSpace(out) == "" {
		t.Fatalf("网关进程没有任何输出：凭据泄漏检查会退化成空断言，无法成立")
	}
	if strings.Contains(out, adminToken) {
		t.Errorf("网关日志泄漏了 admin token")
	}
	if strings.Contains(out, upstreamAPIKey) {
		t.Errorf("网关日志泄漏了上游明文密钥")
	}
	// 进程确实跑完过整条链路（否则上面各步无法成功，这里只是留证）。
	if up.hits.Load() < 2 {
		t.Fatalf("假上游只被打了 %d 次，期望至少 2 次（非流式 + 流式）", up.hits.Load())
	}

	// ---------------------------------------------------------------- 12. 对账
	// 读同一个 SQLite 文件（进程已停，只做 SELECT，不写库）。
	db := openDB(t, gw.dbPath)
	defer func() { _ = db.Close() }()

	assertUsageSettled(t, db, nonStreamReqID, userID,
		int64(upstreamPromptTokens), int64(upstreamCompletionTokens))
	// 流式那条只硬性要求「有 settled 的 usage 行」：token 数值取决于上游是否
	// 在 finish 之前报 usage（§12.4.1）。
	streamUsageRow := usageRow(t, db, streamReqID)
	if streamUsageRow.Status != "settled" {
		t.Fatalf("流式请求 %s 的 usage.status 期望 settled，得到 %q", streamReqID, streamUsageRow.Status)
	}
	if streamUsageRow.ModelID != modelID || streamUsageRow.ProviderID != providerID {
		t.Fatalf("流式 usage 行未带上 model/provider: %+v", streamUsageRow)
	}
	t.Logf("流式 usage 行: input=%d output=%d cost_micro=%d",
		streamUsageRow.InputTokens, streamUsageRow.OutputTokens, streamUsageRow.CostMicro)

	// 402 / 404 两条都不该留下 usage 行（都没进上游）。
	if n := scalarInt(t, db,
		"SELECT COUNT(*) FROM gw_usage WHERE request_id IN ('e2e-poor-402','e2e-model-404')"); n != 0 {
		t.Fatalf("402/404 请求不该写 usage 行，实际有 %d 行", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM gw_usage WHERE user_id=?", poorID); n != 0 {
		t.Fatalf("贫穷用户不该有任何 usage 行，实际 %d 行", n)
	}

	// 幂等：同 key 只允许一行账本。
	if n := scalarInt(t, db,
		"SELECT COUNT(*) FROM gw_quota_ledger WHERE idempotency_key=?", topupKey); n != 1 {
		t.Fatalf("幂等充值应只入账一行，实际 %d 行", n)
	}

	assertReconciled(t, db, userID, topupMicro)
	assertReconciled(t, db, poorID, 0)

	// 预占必须全部归位：任何残留的 held 预占都意味着额度被永久占住。
	for _, uid := range []string{userID, poorID} {
		if n := scalarInt(t, db,
			"SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=? AND status='held'", uid); n != 0 {
			t.Fatalf("用户 %s 残留 %d 条 held 预占", uid, n)
		}
	}

	// 管理面写操作要留痕（契约 §11.3）。
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM gw_audit WHERE actor='admin'"); n < 8 {
		t.Fatalf("管理写操作审计行太少（%d），期望 user.create×2 + key.create + quota.adjust×3 + provider/model/mapping", n)
	}
}

// ---------------------------------------------------------------------------
// 回归用例：针对已修缺陷逐条钉住（真二进制 + 真 SQLite + 假上游）
//
//	TestGatewayTrailingUsageIsSettled               —— D1：尾随 usage 被丢弃 → 按 0 token 计费
//	TestGatewayUnfundedUserGets402                  —— D5：未充值用户被拒时返回 404 而非 402
//	TestGatewayFailedUpstreamLeavesNoHeldReservation —— 上游失败后 held 预占不泄漏
//
// 与上面 TestGatewayEndToEnd 的分工：那一条是「全链路冒烟」，形态固定、覆盖面宽；
// 本节每条用例只钉一个**曾经真实坏过**的点，可独立失败、独立定位。每个用例各起一个
// 网关进程与一份临时库，因此沿用本文件同一套常量命名不会有冲突。
//
// 为什么这三条不能靠 TestGatewayEndToEnd 覆盖：
//  1. 它的假上游把 usage 分片放在 finish_reason **之前**，而 provider 的缺陷只在
//     「之后」出现 —— 那种顺序物理上验证不到修复；
//  2. 它先用 topup 0 建出额度账户，于是「全新用户首次请求」这条真实路径没人走，
//     D5 的 404 缺陷因此长期测不出来（它自己在注释里承认了这次绕过）；
//  3. 它没有上游失败注入点，预占是否被归还无从验证。
//
// 这三条都是**行为级**断言（只看对外契约与实际落库结果，不依赖具体修法），因此
// 修法换一种实现（例如 D5 换用「账户缺失归一为额度不足」而不是「建户即建账户」）
// 也不会把它们测黄。
// ---------------------------------------------------------------------------

// 未充值用户：建号后**不做任何额度操作**。
const (
	unfundedUser     = "e2e-unfunded"
	unfundedPassword = "e2e-unfunded-pass"
)

// regFixture 是一套最小可用环境：真二进制网关（临时库 + 临时端口）+ 指定应答形态的
// 假上游 + 一条 provider→模型映射 + 一个已充值用户及其 API Key。
type regFixture struct {
	*gatewayProc
	up     *fakeUpstream
	hdr    map[string]string
	userID string
	apiKey string
}

// newRegFixture 装配夹具。
func newRegFixture(t *testing.T, mode upstreamMode) *regFixture {
	t.Helper()

	root := repoRoot(t)
	exe := buildGateway(t, root)
	up := newFakeUpstreamMode(t, mode)
	gw := startGateway(t, root, exe, up)
	hdr := map[string]string{"X-Admin-Token": adminToken}

	created := gw.doJSON(t, http.MethodPost, "/admin/providers", hdr, map[string]any{
		"id":         providerID,
		"name":       "回归用例假上游",
		"endpoint":   up.baseURL(),
		"protocol":   "openai-chat",
		"status":     "enabled",
		"api_key":    upstreamAPIKey,
		"timeout_ms": 15000,
		"weight":     1,
	})
	if created.Status != http.StatusOK {
		t.Fatalf("POST /admin/providers: 期望 200，得到 %d，body=%s", created.Status, created.Body)
	}

	modelCreated := gw.doJSON(t, http.MethodPost, "/admin/models", hdr, map[string]any{
		"id":           modelID,
		"display_name": "回归用例模型",
		"capabilities": map[string]any{"stream": true, "tools": false, "vision": false, "reasoning": false},
		"enabled":      true,
	})
	if modelCreated.Status != http.StatusOK {
		t.Fatalf("POST /admin/models: 期望 200，得到 %d，body=%s", modelCreated.Status, modelCreated.Body)
	}

	mapped := gw.doJSON(t, http.MethodPost, "/admin/providers/"+providerID+"/models", hdr, map[string]any{
		"model_id":          modelID,
		"upstream_model_id": upstreamModelID,
		"enabled":           true,
		"priority":          0,
	})
	if mapped.Status != http.StatusOK {
		t.Fatalf("POST /admin/providers/{id}/models: 期望 200，得到 %d，body=%s", mapped.Status, mapped.Body)
	}

	userID := createUser(t, gw, hdr, userName, userPassword)
	// 充值：让「额度不足」不会成为失败原因（一次请求的预占估算 ≈ 260 微单位）。
	acct := adjustQuota(t, gw, hdr, userID, topupKey, topupMicro)
	if acct.Account == nil || acct.Account.Available != topupMicro {
		t.Fatalf("充值后可用额期望 %d，得到 %+v", topupMicro, acct.Account)
	}

	keyRes := gw.doJSON(t, http.MethodPost, "/admin/keys", hdr, map[string]any{"user_id": userID})
	if keyRes.Status != http.StatusOK {
		t.Fatalf("POST /admin/keys: 期望 200，得到 %d，body=%s", keyRes.Status, keyRes.Body)
	}
	var key struct {
		APIKey string `json:"api_key"`
	}
	mustUnmarshal(t, keyRes.Body, &key)
	if !strings.HasPrefix(key.APIKey, "ximo_sk_") {
		t.Fatalf("API Key 明文应以 ximo_sk_ 开头，得到 %.12s", key.APIKey)
	}

	return &regFixture{gatewayProc: gw, up: up, hdr: hdr, userID: userID, apiKey: key.APIKey}
}

// headers 返回带用户凭据与请求 ID 的请求头。
func (f *regFixture) headers(requestID string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + f.apiKey,
		"X-Request-Id":  requestID,
	}
}

// TestGatewayTrailingUsageIsSettled 钉住 D1：上游把 usage 分片放在 finish_reason
// **之后**（真实 OpenAI include_usage 形态）时，网关必须拿到真实 token 并据此计费。
//
// 这是本轮最重要的断言。缺陷机制：internal/provider 曾在收到 finish_reason 处直接
// break，尾随的 usage 分片被丢弃 → 流式与聚合两条路径都按 0 token 结算
// （cost_micro=0，等于白送）。而网关**始终**以 stream_options.include_usage 形态
// 请求上游，所以只要上游照协议回，线路上必然踩到这个形态。
//
// 断言分三层：响应体 usage、落库 usage 行（token + cost 都非 0）、账本对账。
func TestGatewayTrailingUsageIsSettled(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端验收需要真实编译并启动 ximo-gateway，-short 下跳过")
	}
	fx := newRegFixture(t, modeUsageAfterFinish)
	wantCost := int64(upstreamPromptTokens+upstreamCompletionTokens) * priceMicroPerKTok / 1000

	// ------------------------------------------------------ 非流式（聚合解析路径）
	nonStreamReqID := "reg-trailing-nonstream"
	nonStream := fx.doJSON(t, http.MethodPost, "/v1/chat/completions", fx.headers(nonStreamReqID),
		map[string]any{
			"model":    modelID,
			"messages": []map[string]string{{"role": "user", "content": "尾随 usage（非流式）"}},
		})
	if nonStream.Status != http.StatusOK {
		t.Fatalf("非流式 /v1/chat/completions: 期望 200，得到 %d，body=%s", nonStream.Status, nonStream.Body)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	mustUnmarshal(t, nonStream.Body, &completion)
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != upstreamAnswer {
		t.Fatalf("非流式响应内容不对: %s", nonStream.Body)
	}
	if completion.Usage.PromptTokens != upstreamPromptTokens ||
		completion.Usage.CompletionTokens != upstreamCompletionTokens {
		t.Fatalf("非流式响应 usage 期望 %d/%d（来自 finish_reason 之后的 usage 分片），得到 %d/%d；body=%s",
			upstreamPromptTokens, upstreamCompletionTokens,
			completion.Usage.PromptTokens, completion.Usage.CompletionTokens, nonStream.Body)
	}

	// 前提核对：网关必须真的用 include_usage 形态请求上游，否则「尾随 usage」这个
	// 场景根本没被触发，这条用例就退化成一次普通请求。
	if sent := fx.up.last(); !sent.IncludeUsage {
		t.Fatalf("网关请求上游时未带 stream_options.include_usage=true（尾随 usage 场景不成立）: %+v", sent)
	}

	// ------------------------------------------------------ 流式（SSE 透传路径）
	streamReqID := "reg-trailing-stream"
	streamResp := fx.do(t, http.MethodPost, "/v1/chat/completions", fx.headers(streamReqID),
		map[string]any{
			"model":          modelID,
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			"messages":       []map[string]string{{"role": "user", "content": "尾随 usage（流式）"}},
		})
	defer func() { _ = streamResp.Body.Close() }()
	if streamResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("流式 /v1/chat/completions: 期望 200，得到 %d，body=%s", streamResp.StatusCode, raw)
	}
	streamBody, err := io.ReadAll(streamResp.Body)
	if err != nil {
		t.Fatalf("读取流式响应失败: %v", err)
	}
	var (
		streamedContent strings.Builder
		streamUsage     *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		}
		sawFinish, sawDone, usageAfterFinish bool
	)
	for _, p := range dataPayloads(streamBody) {
		if p == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		mustUnmarshal(t, []byte(p), &chunk)
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				sawFinish = true
			}
			streamedContent.WriteString(c.Delta.Content)
		}
		if chunk.Usage != nil {
			streamUsage = chunk.Usage
			// §11.5 收尾顺序：finish_reason 分片 → usage 分片 → [DONE]。
			usageAfterFinish = sawFinish
		}
	}
	if !sawDone {
		t.Fatalf("流式响应缺少 data: [DONE] 收尾: %s", streamBody)
	}
	if got := streamedContent.String(); got != upstreamAnswer {
		t.Fatalf("流式拼接内容期望 %q，得到 %q", upstreamAnswer, got)
	}
	if streamUsage == nil {
		// 修复前这里必然是 nil：尾随 usage 被丢掉 → 网关没有 usage 可发。
		t.Fatalf("流式响应没有 usage 分片：上游在 finish_reason 之后上报的 usage 未被解析（D1 回归）；原始 SSE:\n%s", streamBody)
	}
	if streamUsage.PromptTokens != upstreamPromptTokens || streamUsage.CompletionTokens != upstreamCompletionTokens {
		t.Fatalf("流式 usage 期望 %d/%d，得到 %d/%d",
			upstreamPromptTokens, upstreamCompletionTokens, streamUsage.PromptTokens, streamUsage.CompletionTokens)
	}
	if !usageAfterFinish {
		t.Fatalf("§11.5 要求 usage 分片排在 finish_reason 之后，实际顺序不符；原始 SSE:\n%s", streamBody)
	}

	// ------------------------------------------------------ 落库与对账
	out := fx.stop()
	t.Logf("网关进程输出 %d 字节（尾随 usage 用例）", len(out))
	db := openDB(t, fx.dbPath)
	defer func() { _ = db.Close() }()

	assertUsageSettled(t, db, nonStreamReqID, fx.userID,
		int64(upstreamPromptTokens), int64(upstreamCompletionTokens))

	streamRow := usageRow(t, db, streamReqID)
	if streamRow.Status != "settled" {
		t.Fatalf("流式请求 %s 的 usage.status 期望 settled，得到 %q", streamReqID, streamRow.Status)
	}
	if streamRow.InputTokens != int64(upstreamPromptTokens) || streamRow.OutputTokens != int64(upstreamCompletionTokens) {
		t.Fatalf("流式 usage 行的 token 期望 %d/%d，得到 %d/%d —— 尾随 usage 又被丢弃（D1 回归）",
			upstreamPromptTokens, upstreamCompletionTokens, streamRow.InputTokens, streamRow.OutputTokens)
	}
	if streamRow.CostMicro != wantCost {
		t.Fatalf("流式 usage 行的 cost_micro 期望 %d，得到 %d", wantCost, streamRow.CostMicro)
	}
	t.Logf("尾随 usage 用例实跑值：非流式 input=%d output=%d cost=%d；流式 input=%d output=%d cost=%d（want cost=%d）",
		upstreamPromptTokens, upstreamCompletionTokens, wantCost,
		streamRow.InputTokens, streamRow.OutputTokens, streamRow.CostMicro, wantCost)

	// 两个角度互相印证：Σledger == Available()，且 == 充值额 - Σusage.cost_micro。
	assertReconciled(t, db, fx.userID, topupMicro)
	if n := scalarInt(t, db,
		"SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=? AND status='held'", fx.userID); n != 0 {
		t.Fatalf("残留 %d 条 held 预占", n)
	}
}

// TestGatewayUnfundedUserGets402 钉住 D5：**全新用户（从未做过任何额度操作）**直接
// 请求对话时必须得到 402 insufficient_quota，且不进上游。
//
// 既有 E2E 先 topup 0 把额度账户行建出来再验 402，于是「账户行不存在」这条真实路径
// 没人走，而修复前那条路径正是返回 404 not_found。本用例刻意**不充值**：无论修法是
// 「建户即建账户」还是「账户缺失归一为额度不足」，只要退化成 404 就会被抓住。
//
// 不再假定库里一定没有 gw_quota_accounts 行（那是实现细节，随修法而变），只复核
// 「该用户没有任何可用额度」这一语义前提。
func TestGatewayUnfundedUserGets402(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端验收需要真实编译并启动 ximo-gateway，-short 下跳过")
	}
	// usage 分片顺序与本用例无关，用默认形态。
	fx := newRegFixture(t, modeUsageBeforeFinish)

	unfundedID := createUser(t, fx.gatewayProc, fx.hdr, unfundedUser, unfundedPassword)
	// 用口令登录换 access token：这条凭据链不依赖额度账户，因此能真实打到额度闸门。
	token := poorAccessToken(t, fx.gatewayProc, unfundedUser, unfundedPassword)

	hitsBefore := fx.up.hits.Load()
	res := fx.doJSON(t, http.MethodPost, "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + token, "X-Request-Id": "reg-unfunded-402"},
		map[string]any{
			"model":    modelID,
			"messages": []map[string]string{{"role": "user", "content": "我还没充值"}},
		})

	if res.Status != http.StatusPaymentRequired {
		if res.Status == http.StatusNotFound {
			t.Fatalf("未充值用户首次请求返回 404：契约 §11.4 只定义了 402 insufficient_quota，"+
				"404 会让客户端把「没充值」误判成「资源不存在」。这是 D5 的回归 —— "+
				"修复前 ReserveTx 读不到 gw_quota_accounts 行时返回 model.ErrNotFound，"+
				"修复后应为「建户即建账户」+「账户缺失归一为额度不足」两层。body=%s", res.Body)
		}
		t.Fatalf("未充值用户首次请求：期望 402，得到 %d，body=%s", res.Status, res.Body)
	}
	if code := errCode(t, res.Body); code != "insufficient_quota" {
		t.Fatalf("未充值用户首次请求的错误码期望 insufficient_quota，得到 %q；body=%s", code, res.Body)
	}
	if hits := fx.up.hits.Load(); hits != hitsBefore {
		t.Fatalf("额度不足时不该进上游：假上游命中计数 %d → %d", hitsBefore, hits)
	}

	out := fx.stop()
	t.Logf("网关进程输出 %d 字节（未充值 402 用例）", len(out))
	db := openDB(t, fx.dbPath)
	defer func() { _ = db.Close() }()

	// 没进上游 → 既不该留 usage 行，也不该留预占行。
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM gw_usage WHERE user_id=?", unfundedID); n != 0 {
		t.Fatalf("未充值用户不该有 usage 行，实际 %d 行", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=?", unfundedID); n != 0 {
		t.Fatalf("未充值用户不该有预占行，实际 %d 行", n)
	}
	// 前提复核（不预设修法）：该用户此刻没有任何可用额度。两种修法（建户时同事务
	// 插 0 额度行 / 账户缺失按额度不足处理）都满足这一条，因此这里对两种都成立。
	var available int64
	err := db.QueryRow(
		"SELECT total_amount - used_amount - reserved_amount FROM gw_quota_accounts WHERE user_id=?",
		unfundedID).Scan(&available)
	switch {
	case err == sql.ErrNoRows:
		t.Logf("该用户没有 gw_quota_accounts 行（账户缺失路径）：契约要求此处即 402")
	case err != nil:
		t.Fatalf("读取额度账户失败: %v", err)
	case available != 0:
		t.Fatalf("未充值用户可用额应为 0，实际 %d", available)
	}
	// 未充值用户被拒这件事不能影响别的用户的账本。
	assertReconciled(t, db, fx.userID, topupMicro)
}

// TestGatewayFailedUpstreamLeavesNoHeldReservation 验证「预占不泄漏」：上游 5xx 时
// 请求必须归还预占（不是把它留在 held 等回收），且账本对账仍然成立。
//
// 为什么值得一条独立用例：api/openai/charge.go 的 settle 失败路径刻意**不**回退为
// Release（避免双重退款），其正确性前提是「预占最终会被回收」。若请求路径本身漏了
// release，用户在 TTL 内会凭空少一块可用额 —— 账目上看得见，行为上却很难发现。
func TestGatewayFailedUpstreamLeavesNoHeldReservation(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端验收需要真实编译并启动 ximo-gateway，-short 下跳过")
	}
	fx := newRegFixture(t, modeUpstreamFailure)

	nonStreamReqID := "reg-upstream-5xx"
	nonStream := fx.doJSON(t, http.MethodPost, "/v1/chat/completions", fx.headers(nonStreamReqID),
		map[string]any{
			"model":    modelID,
			"messages": []map[string]string{{"role": "user", "content": "上游会挂"}},
		})
	// 契约 §11.4：候选全失败 → 502 provider_unavailable。
	if nonStream.Status != http.StatusBadGateway {
		t.Fatalf("上游 5xx 时期望 502，得到 %d，body=%s", nonStream.Status, nonStream.Body)
	}
	if code := errCode(t, nonStream.Body); code != "provider_unavailable" {
		t.Fatalf("上游 5xx 时错误码期望 provider_unavailable，得到 %q；body=%s", code, nonStream.Body)
	}
	if fx.up.hits.Load() == 0 {
		t.Fatalf("假上游一次都没被打到：失败注入没起作用，本用例没有测到失败路径")
	}

	// 流式请求同样必须归还预占（§11.5：首字节前失败回普通 JSON 错误 + 非 2xx）。
	streamReqID := "reg-upstream-5xx-stream"
	streamResp := fx.do(t, http.MethodPost, "/v1/chat/completions", fx.headers(streamReqID),
		map[string]any{
			"model":    modelID,
			"stream":   true,
			"messages": []map[string]string{{"role": "user", "content": "流式也会挂"}},
		})
	rawStream, _ := io.ReadAll(streamResp.Body)
	_ = streamResp.Body.Close()
	if streamResp.StatusCode < 500 || streamResp.StatusCode > 599 {
		t.Fatalf("流式请求在上游 5xx 时应回 5xx 普通 JSON 错误，得到 %d，body=%s", streamResp.StatusCode, rawStream)
	}

	out := fx.stop()
	t.Logf("网关进程输出 %d 字节（上游失败用例）", len(out))
	db := openDB(t, fx.dbPath)
	defer func() { _ = db.Close() }()

	// 核心断言 1：没有任何残留 held 预占（全库口径，不只本用户）。
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM gw_quota_reservations WHERE status='held'"); n != 0 {
		t.Fatalf("残留 %d 条 held 预占：失败请求的额度没有被归还", n)
	}
	// 核心断言 2：两次请求的预占都走的是 released（而不是被回收器回收成 expired）。
	for _, id := range []string{nonStreamReqID, streamReqID} {
		var status string
		if err := db.QueryRow("SELECT status FROM gw_quota_reservations WHERE request_id=?", id).
			Scan(&status); err != nil {
			t.Fatalf("读取请求 %s 的预占行失败: %v", id, err)
		}
		if status != "released" {
			t.Fatalf("请求 %s 的预占终态期望 released，得到 %q", id, status)
		}
	}
	// 核心断言 3：账本对账仍成立（Σledger == Available() == 充值额 - Σusage.cost_micro）。
	assertReconciled(t, db, fx.userID, topupMicro)

	// 失败请求要落 usage 行标明终态，且成本为 0（预占已归还，这次没有计费）。
	for _, id := range []string{nonStreamReqID, streamReqID} {
		row := usageRow(t, db, id)
		if row.Status != "upstream_error" {
			t.Fatalf("请求 %s 的 usage.status 期望 upstream_error，得到 %q", id, row.Status)
		}
		if row.CostMicro != 0 {
			t.Fatalf("请求 %s 失败却被计费 %d 微单位", id, row.CostMicro)
		}
	}
	t.Logf("上游失败用例实跑值：假上游命中 %d 次；两次请求的预占均 released，成本均为 0",
		fx.up.hits.Load())
}

// ---------------------------------------------------------------------------
// 断言辅助
// ---------------------------------------------------------------------------

// assertReconciled 校验额度账本的三方对账：
//
//	Σ ledger.amount == 账户 Available()          （契约 §6 的硬不变量）
//	Σ ledger.amount == 充值额 - Σ usage.cost_micro（换一个角度互相印证）
//
// 第二个等式是前者的独立证据：它同时校验「结算写进 used 的金额」与「usage 行
// 记的成本」是同一个数。任一侧算错（例如 settle 行记成毛额、或成本按另一个
// 公式算），两边就会分叉。
func assertReconciled(t *testing.T, db *sql.DB, userID string, topup int64) {
	t.Helper()

	var total, used, reserved int64
	if err := db.QueryRow(
		"SELECT total_amount, used_amount, reserved_amount FROM gw_quota_accounts WHERE user_id=?",
		userID).Scan(&total, &used, &reserved); err != nil {
		t.Fatalf("读取账户 %s 失败: %v", userID, err)
	}
	available := total - used - reserved

	ledgerSum := scalarInt(t, db,
		"SELECT COALESCE(SUM(amount),0) FROM gw_quota_ledger WHERE user_id=?", userID)
	costSum := scalarInt(t, db,
		"SELECT COALESCE(SUM(cost_micro),0) FROM gw_usage WHERE user_id=?", userID)

	if ledgerSum != available {
		t.Errorf("用户 %s 对账失败: Σ ledger.amount=%d 但 Available()=%d（total=%d used=%d reserved=%d）",
			userID, ledgerSum, available, total, used, reserved)
	}
	if ledgerSum != topup-costSum {
		t.Errorf("用户 %s 对账失败: Σ ledger.amount=%d 但 topup(%d) - Σ usage.cost_micro(%d) = %d",
			userID, ledgerSum, topup, costSum, topup-costSum)
	}
}

// assertUsageSettled 校验某一 request_id 的 usage 行（契约 §12.2：成功终态即 settled）。
func assertUsageSettled(t *testing.T, db *sql.DB, requestID, userID string, inTok, outTok int64) {
	t.Helper()
	row := usageRow(t, db, requestID)
	if row.Status != "settled" {
		t.Fatalf("usage %s: status 期望 settled，得到 %q", requestID, row.Status)
	}
	if row.UserID != userID {
		t.Fatalf("usage %s: user_id 期望 %s，得到 %s", requestID, userID, row.UserID)
	}
	if row.ModelID != modelID || row.ProviderID != providerID {
		t.Fatalf("usage %s: model/provider 期望 %s/%s，得到 %s/%s",
			requestID, modelID, providerID, row.ModelID, row.ProviderID)
	}
	if row.InputTokens != inTok || row.OutputTokens != outTok {
		t.Fatalf("usage %s: token 期望 %d/%d，得到 %d/%d",
			requestID, inTok, outTok, row.InputTokens, row.OutputTokens)
	}
	// cost 必须与「按 token 计价」一致：说明 usage 行与账本同源。
	wantCost := (inTok + outTok) * priceMicroPerKTok / 1000
	if row.CostMicro != wantCost {
		t.Fatalf("usage %s: cost_micro 期望 %d，得到 %d", requestID, wantCost, row.CostMicro)
	}
	if row.LatencyMS < 0 {
		t.Fatalf("usage %s: latency_ms 为负 %d", requestID, row.LatencyMS)
	}
}

type usageRecord struct {
	UserID       string
	ModelID      string
	ProviderID   string
	Status       string
	InputTokens  int64
	OutputTokens int64
	LatencyMS    int64
	CostMicro    int64
}

func usageRow(t *testing.T, db *sql.DB, requestID string) usageRecord {
	t.Helper()
	var u usageRecord
	err := db.QueryRow(`SELECT user_id, model_id, provider_id, status,
		input_tokens, output_tokens, latency_ms, cost_micro
		FROM gw_usage WHERE request_id=?`, requestID).
		Scan(&u.UserID, &u.ModelID, &u.ProviderID, &u.Status,
			&u.InputTokens, &u.OutputTokens, &u.LatencyMS, &u.CostMicro)
	if err != nil {
		t.Fatalf("读取 usage %s 失败（该请求没有留下用量记录）: %v", requestID, err)
	}
	return u
}

// scalarInt 跑一条只回一个整数的查询（COUNT/SUM 都走它）。
func scalarInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询失败 %q: %v", query, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 网关进程
// ---------------------------------------------------------------------------

type gatewayProc struct {
	baseURL string
	dbPath  string
	cmd     *exec.Cmd
	out     *syncBuffer

	done   chan struct{}
	client *http.Client
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// 测试的工作目录恒为包目录（go test 的约定），仓库根在两级之上。
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("解析仓库根失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("仓库根 %s 下没有 go.mod: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "migrations", "0003_gateway.sql")); err != nil {
		t.Fatalf("仓库根 %s 下没有 migrations/0003_gateway.sql: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "ximo-gateway")); err != nil {
		t.Fatalf("cmd/ximo-gateway 尚未落地（%v）：E2E 依赖真二进制，不会退化成 httptest", err)
	}
	return root
}

func buildGateway(t *testing.T, root string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "ximo-gateway")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", exe, "./cmd/ximo-gateway")
	cmd.Dir = root
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build ./cmd/ximo-gateway 失败（%v）: %s", err, out.String())
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("构建产物 %s 不存在: %v", exe, err)
	}
	return exe
}

// startGateway 以临时库 + 临时端口启动真二进制，并轮询 /v1/health 直到 200。
//
// 端口先 Listen→Close 再复用，理论上有被抢占的窗口；因此失败时换端口重试，
// 而不是把偶发的 bind 冲突变成假失败。
func startGateway(t *testing.T, root, exe string, up *fakeUpstream) *gatewayProc {
	t.Helper()

	var lastLog string
	for attempt := 1; attempt <= 3; attempt++ {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "gateway.db")
		port := freePort(t)
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

		cmd := exec.Command(exe,
			"--addr", fmt.Sprintf("127.0.0.1:%d", port),
			"--db", dbPath,
			"--admin-token", adminToken,
			"--price-micro-per-ktok", fmt.Sprint(priceMicroPerKTok),
			"--max-output-tokens", fmt.Sprint(maxOutputTokens),
			"--rate-limit-per-min", "0",
		)
		// 工作目录设为仓库根：这样二进制的迁移目录探测（./migrations）一定命中，
		// 不依赖 --migrations 这个额外 flag 是否已实现。
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "XIMO_GATEWAY_HOME="+dir)
		out := &syncBuffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 ximo-gateway 失败: %v", err)
		}

		p := &gatewayProc{
			baseURL: baseURL,
			dbPath:  dbPath,
			cmd:     cmd,
			out:     out,
			done:    make(chan struct{}),
			client:  &http.Client{Timeout: 30 * time.Second},
		}
		go func() { _ = cmd.Wait(); close(p.done) }()
		t.Cleanup(func() { p.stop() })

		if err := p.waitHealthy(t, up); err != nil {
			lastLog = p.out.String()
			p.stop()
			t.Logf("第 %d 次启动失败: %v", attempt, err)
			continue
		}
		return p
	}
	t.Fatalf("ximo-gateway 三次启动都未能通过 /v1/health 探测，最后一次输出:\n%s", lastLog)
	return nil
}

func (p *gatewayProc) waitHealthy(t *testing.T, up *fakeUpstream) error {
	t.Helper()
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return fmt.Errorf("进程提前退出，输出:\n%s", p.out.String())
		default:
		}
		resp, err := p.client.Get(p.baseURL + "/v1/health")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			return fmt.Errorf("/v1/health 返回 %d: %s", resp.StatusCode, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s 内 /v1/health 未就绪，最后一次错误输出:\n%s", startupTimeout, p.out.String())
}

// stop 结束进程并返回它的全部输出（可重复调用）。
func (p *gatewayProc) stop() string {
	select {
	case <-p.done:
	default:
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
		}
	}
	return p.out.String()
}

func (p *gatewayProc) do(t *testing.T, method, path string, headers map[string]string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, p.baseURL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	return resp
}

type apiResult struct {
	Status int
	Body   []byte
}

func (p *gatewayProc) doJSON(t *testing.T, method, path string, headers map[string]string, body any) apiResult {
	t.Helper()
	resp := p.do(t, method, path, headers, body)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s %s 响应失败: %v", method, path, err)
	}
	return apiResult{Status: resp.StatusCode, Body: raw}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// ---------------------------------------------------------------------------
// 管理面封装
// ---------------------------------------------------------------------------

func createUser(t *testing.T, gw *gatewayProc, hdr map[string]string, username, password string) string {
	t.Helper()
	res := gw.doJSON(t, http.MethodPost, "/admin/users", hdr, map[string]any{
		"username": username,
		"password": password,
		"group_id": "e2e",
	})
	if res.Status != http.StatusOK {
		t.Fatalf("POST /admin/users(%s): 期望 200，得到 %d，body=%s", username, res.Status, res.Body)
	}
	var u struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Status   string `json:"status"`
	}
	mustUnmarshal(t, res.Body, &u)
	if u.ID == "" || u.Username != username {
		t.Fatalf("POST /admin/users 响应不完整: %s", res.Body)
	}
	if bytes.Contains(res.Body, []byte(password)) {
		t.Fatalf("POST /admin/users 响应体回显了口令")
	}
	return u.ID
}

type adjustResult struct {
	Ledger struct {
		ID             string `json:"id"`
		Type           string `json:"type"`
		Amount         int64  `json:"amount"`
		IdempotencyKey string `json:"idempotency_key"`
		BalanceAfter   int64  `json:"balance_after"`
	} `json:"ledger"`
	Account *struct {
		UserID         string `json:"user_id"`
		TotalAmount    int64  `json:"total_amount"`
		UsedAmount     int64  `json:"used_amount"`
		ReservedAmount int64  `json:"reserved_amount"`
		Available      int64  `json:"available"`
		Status         string `json:"status"`
	} `json:"account"`
}

func adjustQuota(t *testing.T, gw *gatewayProc, hdr map[string]string,
	userID, idempotencyKey string, amount int64) adjustResult {
	t.Helper()
	res := gw.doJSON(t, http.MethodPost, "/admin/quota/adjust", hdr, map[string]any{
		"user_id":         userID,
		"kind":            "topup",
		"amount":          amount,
		"reason":          "e2e topup",
		"idempotency_key": idempotencyKey,
	})
	if res.Status != http.StatusOK {
		t.Fatalf("POST /admin/quota/adjust: 期望 200，得到 %d，body=%s", res.Status, res.Body)
	}
	var out adjustResult
	mustUnmarshal(t, res.Body, &out)
	if out.Ledger.Type != "topup" || out.Ledger.IdempotencyKey != idempotencyKey {
		t.Fatalf("额度调整账本行不符合预期: %+v", out.Ledger)
	}
	return out
}

// poorAccessToken 为指定用户做一次口令登录并返回 access token。
//
// 用 access token 而不是 API Key 打 402 用例，是为了让「两种凭据都能走到额度
// 闸门」这件事在同一份测试里被覆盖。
func poorAccessToken(t *testing.T, gw *gatewayProc, username, password string) string {
	t.Helper()
	res := gw.doJSON(t, http.MethodPost, "/v1/auth/login", nil, map[string]any{
		"username": username,
		"password": password,
	})
	if res.Status != http.StatusOK {
		t.Fatalf("POST /v1/auth/login(%s): 期望 200，得到 %d，body=%s", username, res.Status, res.Body)
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	mustUnmarshal(t, res.Body, &tokens)
	if tokens.AccessToken == "" {
		t.Fatalf("登录响应缺少 access_token: %s", res.Body)
	}
	return tokens.AccessToken
}

// ---------------------------------------------------------------------------
// 假上游
// ---------------------------------------------------------------------------

type upstreamSeen struct {
	Path          string
	Authorization string
	Model         string
	Stream        bool
	// IncludeUsage 报告网关是否用 stream_options.include_usage 形态请求上游。
	// 它是「上游会不会把 usage 放在 finish_reason 之后」的前提条件，因此必须能断言，
	// 否则尾随 usage 的用例可能悄悄退化成一次普通请求。
	IncludeUsage bool
}

// upstreamMode 决定假上游按哪种形态应答。
//
// 形态差异是「某条代码路径能不能被测到」的决定因素，不是风格问题：既有 E2E 的假上游
// 把 usage 分片放在 finish_reason **之前**，而 provider 曾经的缺陷恰恰只在「之后」
// 出现，因此那种顺序验证不到修复（契约 §12.4.1 登记的就是这一条）。
type upstreamMode int

const (
	// modeUsageBeforeFinish：内容 → usage → finish_reason → [DONE]。
	// 既有 E2E 一直用的形态，保留为默认。
	modeUsageBeforeFinish upstreamMode = iota
	// modeUsageAfterFinish：内容 → finish_reason → usage → [DONE]。
	// OpenAI stream_options.include_usage 与 DeepSeek 默认形态，也是 provider 曾经
	// 在 finish_reason 处 break 而丢掉尾随 usage 的位置。
	modeUsageAfterFinish
	// modeUpstreamFailure：直接回 503（失败注入，用来验证预占不泄漏）。
	modeUpstreamFailure
)

type fakeUpstream struct {
	srv  *httptest.Server
	hits atomic.Int64
	mode upstreamMode

	mu   sync.Mutex
	seen upstreamSeen
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	return newFakeUpstreamMode(t, modeUsageBeforeFinish)
}

// newFakeUpstreamMode 构造指定应答形态的假上游（默认形态见 newFakeUpstream）。
func newFakeUpstreamMode(t *testing.T, mode upstreamMode) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{mode: mode}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) baseURL() string { return f.srv.URL }

func (f *fakeUpstream) last() upstreamSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

// handle 默认固定以 SSE 应答。
//
// 这不是偷懒：internal/provider 的非流式路径（Complete）在注释里就写明
// 「请求体仍带 stream:true，解析阶段把流式响应聚合为单个响应」，也就是说
// 上游**必须**按流式返回，两种客户端请求共用同一条解析路径。假上游照着真实
// 上游的流式行为返回，才能测到网关真正的解析逻辑。
//
// modeUpstreamFailure 是唯一的例外：它回 503 + JSON（不是 SSE），用来模拟「连流都
// 没建立起来」的上游故障，走网关首字节前的失败路径。
func (f *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	_ = json.Unmarshal(raw, &req)

	f.mu.Lock()
	f.seen = upstreamSeen{
		Path:          r.URL.Path,
		Authorization: r.Header.Get("Authorization"),
		Model:         req.Model,
		Stream:        req.Stream,
		IncludeUsage:  req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
	}
	f.mu.Unlock()

	if f.mode == modeUpstreamFailure {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"fake upstream unavailable","type":"server_error"}}`)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	usageChunk := fakeChunk{ID: "upstream-e2e", Choices: []fakeChoice{},
		Usage: &fakeUsage{
			PromptTokens:     upstreamPromptTokens,
			CompletionTokens: upstreamCompletionTokens,
			TotalTokens:      upstreamPromptTokens + upstreamCompletionTokens,
		}}

	half := len(upstreamAnswer) / 2
	chunks := []fakeChunk{
		{ID: "upstream-e2e", Object: "chat.completion.chunk", Model: upstreamModelID,
			Choices: []fakeChoice{{Index: 0, Delta: fakeDelta{Role: "assistant", Content: upstreamAnswer[:half]}}}},
		{ID: "upstream-e2e", Object: "chat.completion.chunk", Model: upstreamModelID,
			Choices: []fakeChoice{{Index: 0, Delta: fakeDelta{Content: upstreamAnswer[half:]}}}},
	}
	if f.mode == modeUsageBeforeFinish {
		// usage 分片放在 finish 之前：这条形态下 usage 在主循环里就被取到，
		// 收尾扫描（drainTrailingUsage）因此不会触发 —— 它只在「读到结束分片时
		// 还没有 usage」时才启动。两种顺序各自覆盖一条代码路径，缺一条就有半段
		// 没被验证到，所以这个顺序差异仍然保留着。
		chunks = append(chunks, usageChunk)
	}
	for _, c := range chunks {
		writeSSE(w, flusher, mustJSON(c))
	}
	stop := "stop"
	finish := mustJSON(fakeChunk{ID: "upstream-e2e", Object: "chat.completion.chunk",
		Model: upstreamModelID, Choices: []fakeChoice{{Index: 0, FinishReason: &stop}}})
	writeSSE(w, flusher, finish)
	if f.mode == modeUsageAfterFinish {
		// 真实上游形态：usage 落在 finish_reason **之后**的独立分片里。
		// provider 的 drainTrailingUsage（internal/provider/stream.go）正是为这一段加的：
		// 主循环在结束分片处停下，之后扫尾把 usage 捞回来。
		writeSSE(w, flusher, mustJSON(usageChunk))
	}
	writeSSE(w, flusher, "[DONE]")
}

type fakeChunk struct {
	ID      string       `json:"id,omitempty"`
	Object  string       `json:"object,omitempty"`
	Model   string       `json:"model,omitempty"`
	Choices []fakeChoice `json:"choices"`
	Usage   *fakeUsage   `json:"usage,omitempty"`
}

type fakeChoice struct {
	Index        int       `json:"index"`
	Delta        fakeDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason,omitempty"`
}

type fakeDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type fakeUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func writeSSE(w io.Writer, flusher http.Flusher, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if flusher != nil {
		flusher.Flush()
	}
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// 杂项
// ---------------------------------------------------------------------------

// dataPayloads 抽出 SSE 里所有 data: 负载（忽略空行与注释心跳）。
func dataPayloads(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	return out
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	mustUnmarshal(t, body, &e)
	return e.Error.Code
}

func mustUnmarshal(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("解析响应 JSON 失败: %v; body=%s", err, body)
	}
}

// openDB 打开网关写过的 SQLite 文件做只读断言。
//
// 刻意只用普通连接而不是 `mode=ro`：WAL 库的只读连接要求 -shm 共享内存可写，
// 在进程被强杀后不一定成立，会把「读不到」变成假失败。本函数只发 SELECT
// （契约里「禁止用 ReadDB() 写」约束的是服务端写路径，不是测试的读断言）。
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据库文件 %s 不存在: %v", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	return db
}

// syncBuffer 是并发安全的输出缓冲：子进程的 stdout/stderr 由多个 goroutine
// 写入，读取时不能与写并发。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
