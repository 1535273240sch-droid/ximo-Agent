package openai

import "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"

// Routes 返回本包贡献的路由，由 main 装配进 http.ServeMux。
//
// 只注册 POST /v1/chat/completions（契约 §11.2）：其余入口（/v1/models、/v1/usage、
// 认证、管理态）分别属于 api/meta 与 api/admin。
func Routes(d Deps) []httpx.Route {
	h := &handler{d: d}
	return []httpx.Route{
		{Pattern: "POST /v1/chat/completions", Handler: h.chatCompletions},
	}
}
