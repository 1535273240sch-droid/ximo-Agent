// Package console 提供中转站的管理控制台（静态页面）。
//
// 为什么需要它：网关原先只有 API，运维直接在浏览器打开 http://<host>:8600/ 会得到
// 404，看起来像"后端进不去"。控制台把这些接口串成可点的页面。
//
// 鉴权边界（重要）：**页面本身不需要令牌**（它只是 HTML/CSS/JS，令牌由使用者在
// 页面上输入并只存在浏览器本地）；真正的数据请求全部打到 /admin/*，而那条链路由
// gateway.Authenticator 逐个请求校验 X-Admin-Token。因此把控制台暴露在公网不会
// 泄露数据，但仍然建议把它放在 HTTPS 之后（令牌是明文传输的）。
package console

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
)

//go:embed assets/*
var assetsFS embed.FS

// Routes 返回控制台路由。
//
// 注意 Go 1.22+ ServeMux 的匹配规则，三点都是踩过的坑：
//   - "GET /{$}" 是**精确**匹配根路径；用 "GET /" 会变成 catch-all，
//     把未知的 API 路径也重定向到控制台；
//   - 不能同时注册 "/console/" 与 "/console/{file...}"（子树前缀与通配冲突，
//     ServeMux 直接判为 conflicting patterns 并拒绝启动）；
//   - 因此子树只注册一条 "/console/"，由它自己区分「首页」与「静态资源」。
func Routes() []httpx.Route {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// embed 失败只可能是编译期路径写错；此时让路由缺失，而不是 panic 掉整个进程。
		return nil
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil
	}
	files := http.StripPrefix("/console/", http.FileServer(http.FS(sub)))

	serveConsole := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/console/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(index)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}

	return []httpx.Route{
		{Pattern: "GET /{$}", Handler: redirectToConsole},
		{Pattern: "GET /console", Handler: redirectToConsole},
		{Pattern: "GET /console/", Handler: serveConsole},
	}
}

func redirectToConsole(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/console/", http.StatusFound)
}
