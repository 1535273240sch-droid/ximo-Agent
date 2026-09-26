package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

const (
	headerRequestID = "X-Request-Id"
	// maxRequestIDLen 限制外部传入的请求 ID 长度，防止把它当无界字段写日志。
	maxRequestIDLen = 128
	// recoverStackBytes 是 panic 时抓取的栈字节数上限（只进日志，不进响应）。
	recoverStackBytes = 8 << 10
)

// chain 装配全局中间件链，顺序固定为（契约 §11.2）：
//
//	recover → X-Request-Id → 访问日志 → 请求体上限 →（请求超时）→ 路由
//
// 最外层是 recover：任何一层的 panic 都要被兜住；请求 ID 在访问日志之前生成，
// 这样日志与错误响应体都能带上它；请求体上限贴近 handler，避免影响前置逻辑；
// 超时最内层，只约束 handler（含流式生成）本身。
func (s *Server) chain(next http.Handler) http.Handler {
	h := next
	if s.cfg.RequestTimeout > 0 {
		h = Timeout(s.cfg.RequestTimeout, h)
	}
	if s.cfg.MaxBodyBytes > 0 {
		h = LimitBody(s.cfg.MaxBodyBytes, h)
	}
	h = AccessLog(s.logger, h)
	h = RequestID(h)
	return Recover(s.logger, h)
}

// Chain 从右到左套用中间件：Chain(h, a, b) 等价于 a(b(h))。
// 各 api 子包按生命周期（认证 → 限流 → handler）组合自己的包装时可用它。
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		if mw[i] == nil {
			continue
		}
		h = mw[i](h)
	}
	return h
}

// Recover 兜住 handler 的 panic：记结构化日志（含栈，只进日志）+ 回 500。
//
// 响应体只给固定文案，绝不带栈或 panic 值（panic 值可能含请求内容）。
// 若响应头已经发出（流式已开始），只能记日志并中止，此时无法改写状态码。
func Recover(logger *observability.Logger, next http.Handler) http.Handler {
	logger = nonNilLogger(logger)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// net/http 约定：ErrAbortHandler 是「主动中止连接」，不算错误，必须继续向上抛。
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			stack := make([]byte, recoverStackBytes)
			n := runtime.Stack(stack, false)
			logger.Error(r.Context(), "gateway: handler panic 已恢复", map[string]any{
				"method":     r.Method,
				"path":       r.URL.Path,
				"request_id": httpx.RequestID(r),
				"panic":      redactTokens(fmt.Sprint(rec)),
				"stack":      redactTokens(string(stack[:n])),
			})
			if wroteHeader(w) {
				return
			}
			httpx.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// RequestID 保证每个请求都有 ID：沿用客户端传入的合法值（便于跨服务串联），
// 否则用 crypto/rand 生成 "req_<32hex>"。ID 同时写进 context（httpx.RequestID 读取）
// 与响应头，日志、错误响应体、上游调用都复用同一个值。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(headerRequestID))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), id)))
	})
}

// AccessLog 记录方法/路径/状态/耗时/request_id/用户等。
// 只记 URL.Path，**不记 RawQuery**（查询串可能带凭据）；用户只记 ID，不记令牌。
func AccessLog(logger *observability.Logger, next http.Handler) http.Handler {
	logger = nonNilLogger(logger)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		fields := map[string]any{
			"method":      r.Method,
			"path":        redactTokens(r.URL.Path),
			"status":      rec.status,
			"duration_ms": elapsed.Milliseconds(),
			"request_id":  httpx.RequestID(r),
			"bytes":       rec.bytes,
			"ip":          httpx.ClientIP(r),
		}
		if p, ok := PrincipalFrom(r.Context()); ok {
			fields["user"] = p.UserID()
			// 字段名避开 observability 的敏感键名（credential/token），否则值会被整体替换。
			fields["credential_kind"] = p.Credential
		}
		msg := "gateway: 请求完成"
		switch {
		case rec.status >= http.StatusInternalServerError:
			logger.Error(r.Context(), msg, fields)
		case rec.status >= http.StatusBadRequest:
			logger.Warn(r.Context(), msg, fields)
		default:
			logger.Info(r.Context(), msg, fields)
		}
	})
}

// LimitBody 限制请求体字节数：越界后读取返回错误，由 handler 决定怎么应答
// （通常 413）。max <= 0 表示不限制。
func LimitBody(max int64, next http.Handler) http.Handler {
	if max <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// Timeout 给 handler 设置总时限。到点只取消 context（不影响已写出的响应），
// 因此流式 handler 必须监听 ctx.Done 并及时收尾（释放预占、落 usage）。
func Timeout(d time.Duration, next http.Handler) http.Handler {
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// responseRecorder 记录状态码与响应字节数（访问日志用），并把 Flush 透传给底层，
// 否则 SSE 会被缓冲住。
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// WroteHeader 报告响应头是否已发出（panic 恢复与日志用）。
func (r *responseRecorder) WroteHeader() bool { return r.wrote }

// Flush 实现 http.Flusher：SSE 依赖它；下层支持时透传，否则静默（真实服务的底层
// 一定是 *http.response，必然是 Flusher）。刻意不实现 Hijacker：V1 没有连接升级端点，
// 谎报该能力比不支持更危险；需要时可经 Unwrap 拿到底层 writer。
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.NewResponseController 能穿透到下层（Flush/Hijack/SetDeadline）。
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func wroteHeader(w http.ResponseWriter) bool {
	if r, ok := w.(interface{ WroteHeader() bool }); ok {
		return r.WroteHeader()
	}
	return false
}

func newRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败极罕见；退化到纳秒时间戳，仍保证非空且随请求变化。
		return fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(buf)
}

// sanitizeRequestID 只接受 [A-Za-z0-9._-] 且长度受限的 ID；非法值返回空串（由调用方重新生成），
// 避免把换行等注入字符带进响应头与日志。
func sanitizeRequestID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return id
}
