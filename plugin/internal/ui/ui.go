// Package ui 是 ximo-plugin 的本地网页界面服务端，形态与安全边界见
// recon/插件UI契约-冻结.md §1/§2/§3：
//
//   - 只绑回环地址（127.0.0.1），端口被占用时报错，不静默换端口；
//   - 每次启动用 crypto/rand 生成 32 位十六进制会话令牌，打开的地址形如
//     http://127.0.0.1:8787/?t=<token>；所有 /api/* 都要带 X-UI-Token，不匹配 401；
//   - 静态资源用 //go:embed 嵌进二进制，运行时零外部请求。
//
// 本包只做「HTTP + 令牌 + 响应信封 + 静态资源」，业务（检测/计划/应用/体检）全部由
// Backend 接口交给 cmd 侧接线到既有包（internal/{spec,engine,gateway,agents}），
// 这里不重写业务逻辑，也不 import 主仓库的任何包。
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TokenHeader 是会话令牌的请求头名（契约 §2.2）。
const TokenHeader = "X-UI-Token"

// listenHost 是唯一的监听地址：只绑回环，绝不绑 0.0.0.0。
const listenHost = "127.0.0.1"

// opTimeout 是单个接口的后端调用上限（契约 §3：长任务不要超过 ~20s 无响应）。
const opTimeout = 20 * time.Second

// readHeaderTimeout 限制握手阶段，避免半开连接占着不放。
const readHeaderTimeout = 10 * time.Second

// TokenBytes 是会话令牌的随机字节数（16 字节 → 32 位十六进制，契约 §2.1）。
const TokenBytes = 16

// Config 是服务端配置。业务能力全部来自 Backend。
type Config struct {
	// Port 是监听端口；默认 8787 由 cmd 侧给（0 表示显式要一个随机端口）。
	Port int
	// Backend 提供全部业务能力，必填。
	Backend Backend
}

// Server 是一个本地界面服务端。生命周期：New → Listen → Serve。
type Server struct {
	cfg   Config
	token string

	ln      net.Listener
	http    *http.Server
	handler http.Handler
}

// New 建一个服务端并生成会话令牌。生成的令牌只在内存里，不落盘、不进日志
// （调用方只应把 Token()/URL() 用于「打开浏览器」或打印一次给用户）。
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("ui: 缺少 Backend（业务由 cmd 侧接线）")
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("ui: 端口 %d 非法（应在 0..65535）", cfg.Port)
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, token: token}
	s.handler = s.routes()
	return s, nil
}

// newToken 生成会话令牌：16 字节 crypto/rand → 32 位十六进制。
func newToken() (string, error) {
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成会话令牌失败（crypto/rand）: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Token 返回本次会话的令牌。
func (s *Server) Token() string { return s.token }

// Listen 绑定 127.0.0.1:<port>。端口被占用时返回带提示的错误——**不静默换端口**：
// 用户以为界面在 8787，实际跑到别的端口上，比直接失败更难查。
func (s *Server) Listen() error {
	if s.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return fmt.Errorf("无法监听 %s:%d：%w；端口可能已被占用或无权绑定，请换一个端口：ximo-plugin ui --port <其它端口>",
			listenHost, s.cfg.Port, err)
	}
	s.ln = ln
	return nil
}

// BaseURL 是不带令牌的界面地址（可安全打印/记录）。
func (s *Server) BaseURL() string {
	return "http://" + s.addr() + "/"
}

// URL 是带会话令牌的界面地址：把它交给浏览器即可，等价于「一次性访问凭证」，
// 不要贴进日志、工单或聊天记录。
func (s *Server) URL() string {
	return s.BaseURL() + "?t=" + s.token
}

// addr 返回实际的 host:port（已 Listen 时用真实端口，否则用配置里的端口）。
func (s *Server) addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return net.JoinHostPort(listenHost, strconv.Itoa(s.cfg.Port))
}

// Handler 返回完整的 HTTP 处理器（含令牌校验），供测试或自定义托管使用。
func (s *Server) Handler() http.Handler { return s.handler }

// Serve 阻塞地提供服务，直到 ctx 结束或服务出错。必须先在 Listen 之后调用
// （或由本方法代为 Listen）。
func (s *Server) Serve(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	s.http = srv

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(s.ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("界面服务异常退出: %w", err)
	}
}

// Close 关闭监听（测试用）。
func (s *Server) Close() error {
	if s.http != nil {
		_ = s.http.Close()
	}
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// routes 注册全部端点（契约 §3）。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assetFS())))

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/detect", s.handleDetect)
	mux.HandleFunc("POST /api/login/start", s.handleLoginStart)
	mux.HandleFunc("POST /api/login/poll", s.handleLoginPoll)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("POST /api/plan", s.handlePlan)
	mux.HandleFunc("POST /api/apply", s.handleApply)
	mux.HandleFunc("GET /api/doctor", s.handleDoctor)
	mux.HandleFunc("GET /api/specs", s.handleSpecs)

	// 未注册的 /api/* 也返回统一信封，前端不必为 404 单独解析纯文本；
	// 「路径对、方法错」返回 405 而不是 404（注册了带方法的模式后，ServeMux 会把
	// 这类请求交给这条兜底模式，所以这里自己区分）。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if apiPaths[r.URL.Path] {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持 "+r.Method)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "未知接口 "+r.Method+" "+r.URL.Path)
	})

	return s.withToken(mux)
}

// apiPaths 是全部已注册的接口路径（契约 §3）。
var apiPaths = map[string]bool{
	"/api/state":       true,
	"/api/detect":      true,
	"/api/login/start": true,
	"/api/login/poll":  true,
	"/api/models":      true,
	"/api/plan":        true,
	"/api/apply":       true,
	"/api/doctor":      true,
	"/api/specs":       true,
}

// withToken 是 /api/* 的令牌闸门（契约 §2.2）：其它程序拿不到令牌就不能驱动插件
// 改配置。静态资源不校验令牌——页面必须能在浏览器里正常加载（令牌在页面 URL 的
// 查询串里，子资源请求带不上它）。
func (s *Server) withToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Referrer-Policy 很重要：不带它的话，页面里的外链可能把 URL 上的 ?t= 带出去。
		w.Header().Set("Referrer-Policy", "no-referrer")
		// 静态资源全部内嵌，CSP 用来兜住「零外部请求」这条约定（'unsafe-inline' 只为
		// 兼容单文件页面的内联样式/脚本，不放开任何远程来源）。
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")

		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !s.tokenEqual(r.Header.Get(TokenHeader)) {
				writeErr(w, http.StatusUnauthorized, "unauthorized",
					"缺少或错误的 "+TokenHeader+"：请用启动 ui 时打印的带令牌地址打开界面")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// tokenEqual 用常数时间比较令牌，避免按字节比较泄露前缀信息。
func (s *Server) tokenEqual(got string) bool {
	if len(got) != len(s.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}
