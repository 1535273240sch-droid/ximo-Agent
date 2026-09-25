package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipcapi"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// 本文件实现「拉取可用模型列表」。
//
// 为什么需要它：用户填了 API 密钥之后，界面需要知道这个密钥能用哪些模型，
// 否则只能手敲模型名——敲错就是一个含糊的 400 错误，排查成本很高。
//
// 实现方式：调用服务商的 OpenAI 兼容端点 GET {base_url}/models，解析 id 列表。
// 这是 DeepSeek / OpenAI / Moonshot / 绝大多数兼容网关都支持的标准端点。
//
// 安全约束：请求只带 Authorization 头，密钥不出现在 URL、日志或错误消息里；
// 出错时返回的是脱敏后的描述（例如 HTTP 401），不回显响应体中的敏感内容。

// ModelInfo 是单个可用模型的描述。
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ModelList 是模型查询结果。
type ModelList struct {
	Models []ModelInfo `json:"models"`
	// BaseURL 是实际查询的地址，供界面显示"查的是哪儿"。
	BaseURL string `json:"base_url"`
	// Error 非空表示查询失败，Models 可能为空。
	Error string `json:"error,omitempty"`
}

// ListModels 向服务商查询可用模型。
//
// baseURLHint 是设置页「获取模型」按钮传来的表单当前值（用户可能还没保存）：
// 非空时以它为准 —— 否则用户改了地址没保存就点获取，实际查的还是旧地址，
// 报错看起来莫名其妙。为空时回落到已保存配置。
//
// 密钥来源：优先用配置里的 SecretRef 从安全存储取出；取不到时直接返回错误，
// 不做任何"空密钥尝试"——那只会得到 401，反而掩盖真实原因。
func (a *App) ListModels(ctx context.Context, baseURLHint string) ipcapi.ModelListPayload {
	out := ipcapi.ModelListPayload{}

	if a.cfg == nil {
		out.Error = "配置未加载"
		return out
	}
	hint, err := config.NormalizeBaseURL(baseURLHint)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	raw := hint
	if raw == "" {
		raw = a.cfg.Provider.BaseURL
	}
	base, err := config.NormalizeBaseURL(raw)
	if err != nil {
		out.Error = "接口地址有误：" + err.Error()
		return out
	}
	if base == "" {
		out.Error = "服务商 Base URL 为空，请先填写接口地址"
		return out
	}
	out.BaseURL = base

	// 取密钥：只用于构造请求头，不保留、不记录。
	var apiKey string
	if ss := a.Secrets(); ss != nil {
		ref := a.cfg.Provider.SecretRef
		if ref == "" {
			out.Error = "尚未配置 API 密钥"
			return out
		}
		key, err := ss.mgr.Get(ref)
		if err != nil {
			out.Error = fmt.Sprintf("无法从安全存储读取密钥: %v", err)
			return out
		}
		apiKey = key
	} else {
		out.Error = "当前构建未启用安全存储，无法读取密钥"
		return out
	}

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/models", nil)
	if err != nil {
		out.Error = fmt.Sprintf("构造请求失败: %v", err)
		return out
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	// 代理感知客户端：环境变量代理 → Windows 系统代理 → 直连。直连无法
	// 到达的服务商（被墙/内网）在用户开了系统代理时经由代理可达。
	resp, err := provider.ProxyHTTPClient().Do(req)
	if err != nil {
		// 网络层错误：只回描述，不回显可能含密钥的完整错误。
		out.Error = fmt.Sprintf("请求服务商失败: %v（请检查网络与系统代理是否已开启、地址是否可达）", err)
		return out
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		// 不回显响应体：某些网关会在错误体里回带请求头。
		out.Error = fmt.Sprintf("服务商返回 HTTP %d（请检查 API 密钥是否有效、Base URL 是否正确）", resp.StatusCode)
		return out
	}

	var parsed struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		out.Error = "服务商返回的模型列表格式无法解析（可能不是 OpenAI 兼容端点）"
		return out
	}

	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		out.Models = append(out.Models, ipcapi.ModelInfoPayload{ID: m.ID, OwnedBy: m.OwnedBy})
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })

	if len(out.Models) == 0 {
		out.Error = "服务商返回了空的模型列表"
	}
	return out
}
