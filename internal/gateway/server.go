package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

const (
	// ShutdownTimeout 是优雅停机的等待上限：到点即强制断开，避免停机把进程挂住。
	ShutdownTimeout = 15 * time.Second
	// readHeaderTimeout 防 slowloris：请求头必须在此时限内读完。
	readHeaderTimeout = 10 * time.Second
	// idleTimeout 是 keep-alive 空闲连接回收时间。
	idleTimeout = 90 * time.Second
	// maxHeaderBytes 是请求头大小上限（net/http 默认值，显式写出以免被误改）。
	maxHeaderBytes = 1 << 20
)

// Server 持有一组已装配的路由与固定顺序的中间件链，负责监听与优雅停机。
// 零值不可用，请用 New 构造。Server 不持有 api 子包的依赖（契约 §11.2）。
type Server struct {
	cfg     Config
	logger  *observability.Logger
	handler http.Handler
}

// New 校验配置、装配路由与中间件链。任何装配错误都在这里暴露（而不是等到第一个请求）。
//
// logger 传 nil 时退回 observability 的全局默认 logger。
func New(cfg Config, routes []httpx.Route, logger *observability.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	logger = nonNilLogger(logger)
	mux, err := buildMux(routes)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, logger: logger}
	s.handler = s.chain(mux)
	return s, nil
}

// Handler 返回带完整中间件链的处理器（测试与嵌入场景用）。
func (s *Server) Handler() http.Handler { return s.handler }

// Config 返回生效配置（只读副本，便于日志与诊断）。
func (s *Server) Config() Config { return s.cfg }

// Run 在 cfg.Addr 上监听并服务，直到 ctx 被取消后优雅停机。
// 启动失败（端口占用等）立即返回错误。
func (s *Server) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("gateway: Run 需要非 nil 的 context")
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("gateway: 监听 %s 失败: %w", s.cfg.Addr, err)
	}
	s.logger.Info(ctx, "gateway: 开始监听", merge(1, s.cfg.LogFields(), map[string]any{"listen": ln.Addr().String()}))
	if err := s.Serve(ctx, ln); err != nil {
		_ = ln.Close()
		return err
	}
	return nil
}

// Serve 在给定 listener 上服务并在 ctx 取消后优雅停机（等待在途请求结束，
// 上限 ShutdownTimeout）。把监听与停机拆开是为了测试能用 127.0.0.1:0 拿随机端口。
//
// 停机期间 listener 先关闭（新连接立刻被拒），在途请求继续跑完；超过上限则强制
// 关闭并返回错误，不留下后台 goroutine。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if ctx == nil {
		return errors.New("gateway: Serve 需要非 nil 的 context")
	}
	if ln == nil {
		return errors.New("gateway: Serve 需要非 nil 的 listener")
	}
	hs := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// WriteTimeout 刻意不设：SSE 长连接会被它误杀，超时由 RequestTimeout 与上游客户端负责。
		ErrorLog: log.New(&serverLogWriter{logger: s.logger}, "", 0),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve(ln) }()

	select {
	case err := <-errCh:
		return serveExitError(err)
	case <-ctx.Done():
	}

	// 停机日志用 WithoutCancel：此时 ctx 已取消，直接传会丢掉 trace/日志上下文。
	base := context.WithoutCancel(ctx)
	s.logger.Info(base, "gateway: 收到停机请求，开始优雅停机", map[string]any{"addr": ln.Addr().String()})
	shutdownCtx, cancel := context.WithTimeout(base, ShutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		_ = hs.Close()
		<-errCh
		return fmt.Errorf("gateway: 优雅停机超过 %s，已强制关闭: %w", ShutdownTimeout, err)
	}
	s.logger.Info(base, "gateway: 已停机", map[string]any{"addr": ln.Addr().String()})
	return serveExitError(<-errCh)
}

func serveExitError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("gateway: 服务异常退出: %w", err)
}

// buildMux 把路由注册进 Go 1.22+ 的 ServeMux（支持 "POST /admin/users/{id}/status"
// 这类 method+通配符 pattern，handler 内用 r.PathValue 取值）。
//
// 重复 pattern、空 pattern、nil handler 都是装配错误：ServeMux 对重复注册直接 panic，
// 这里改成带路由名的 error，启动期就能定位是哪个子包给错了。
func buildMux(routes []httpx.Route) (http.Handler, error) {
	mux := http.NewServeMux()
	seen := make(map[string]struct{}, len(routes))
	for _, rt := range routes {
		if strings.TrimSpace(rt.Pattern) == "" {
			return nil, errors.New("gateway: 路由 pattern 不能为空")
		}
		if rt.Handler == nil {
			return nil, fmt.Errorf("gateway: 路由 %q 的 handler 为 nil", rt.Pattern)
		}
		if _, dup := seen[rt.Pattern]; dup {
			return nil, fmt.Errorf("gateway: 路由 %q 重复注册", rt.Pattern)
		}
		seen[rt.Pattern] = struct{}{}
		if err := registerRoute(mux, rt); err != nil {
			return nil, err
		}
	}
	return mux, nil
}

func registerRoute(mux *http.ServeMux, rt httpx.Route) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("gateway: 注册路由 %q 失败: %v", rt.Pattern, rec)
		}
	}()
	mux.HandleFunc(rt.Pattern, rt.Handler)
	return nil
}

// serverLogWriter 把 net/http 的内部错误（TLS 握手失败、坏请求行等）导到结构化日志，
// 并强制过一遍凭据脱敏。
type serverLogWriter struct{ logger *observability.Logger }

func (w *serverLogWriter) Write(p []byte) (int, error) {
	if msg := strings.TrimRight(string(p), "\r\n"); msg != "" {
		w.logger.Error(context.Background(), "gateway: http 服务器内部错误",
			map[string]any{"detail": redactTokens(msg)})
	}
	return len(p), nil
}

func nonNilLogger(l *observability.Logger) *observability.Logger {
	if l == nil {
		return observability.DefaultLogger()
	}
	return l
}

// merge 合并多个日志字段表；nil map 安全。
func merge(minCap int, maps ...map[string]any) map[string]any {
	out := make(map[string]any, minCap)
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
