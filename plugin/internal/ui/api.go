package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/1535273240sch-droid/ximo-plugin/internal/engine"
	"github.com/1535273240sch-droid/ximo-plugin/internal/gateway"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// maxBodyBytes 是请求体上限：入参都是小 JSON（spec_id/model/api_key），1MiB 足够。
const maxBodyBytes = 1 << 20

// Backend 是 UI 服务端需要的全部业务能力，由 cmd 侧实现（复用 internal/{spec,engine,
// gateway,agents}，见 cmd/ximo-plugin/ui.go）。这里定义形状、不实现业务。
//
// 约定：返回的凭据一律是**掩码后**的（本包不再二次加工）；错误用 CodeError 携带稳定
// 错误码，前端按 code 分支，message 只用于展示。
type Backend interface {
	State(ctx context.Context) (State, error)
	Detect(ctx context.Context) ([]Adapter, error)
	LoginStart(ctx context.Context) (LoginStart, error)
	LoginPoll(ctx context.Context) (LoginPoll, error)
	Models(ctx context.Context) ([]gateway.Model, error)
	Plan(ctx context.Context, req PlanRequest) (Plan, error)
	Apply(ctx context.Context, req PlanRequest) ([]ApplyItem, error)
	Doctor(ctx context.Context) ([]DoctorItem, error)
	Specs(ctx context.Context) ([]spec.Spec, error)
}

// Adapter 是 /api/state 与 /api/detect 里的一条适配器检测结果（契约 §3）。
type Adapter struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Detected bool              `json:"detected"`
	Evidence []engine.Evidence `json:"evidence"`
}

// State 是 /api/state 的载荷。凭据只有掩码形态。
type State struct {
	Version      string    `json:"version"`
	Home         string    `json:"home"`
	IsolatedHome bool      `json:"isolated_home"`
	Gateway      string    `json:"gateway"`
	LoggedIn     bool      `json:"logged_in"`
	CredMasked   string    `json:"cred_masked"`
	Adapters     []Adapter `json:"adapters"`
}

// LoginStart 是 /api/login/start 的载荷。设备码（device_code）等价于短期凭据，
// 由服务端自己留着轮询，**绝不**回给前端。
type LoginStart struct {
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri,omitempty"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval,omitempty"`
}

// LoginPoll 是 /api/login/poll 的载荷。status=pending 表示用户还没在授权页确认。
type LoginPoll struct {
	Status     string `json:"status"` // pending | ok
	CredMasked string `json:"cred_masked,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

// PlanRequest 是 /api/plan 与 /api/apply 的入参（契约 §3）。网关地址不在入参里：
// 它来自 `ui --gateway` 或已保存的凭据，避免前端把请求引到别的地址上。
type PlanRequest struct {
	SpecID  string `json:"spec_id"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
	Confirm bool   `json:"confirm"`
}

// PlanItem 对应一条待执行的变更（字段与 engine.PlanStep 的前四个一一对应）。
type PlanItem struct {
	SpecID  string `json:"spec_id"`
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Summary string `json:"summary"`
}

// Plan 是 /api/plan 的载荷：待写计划（未落盘）+ 已掩码的差异文本。
type Plan struct {
	Items   []PlanItem `json:"items"`
	Diff    string     `json:"diff"`
	Warning string     `json:"warning,omitempty"`
}

// ApplyItem 是一条执行结果（契约 §3）。失败是数据，不是 HTTP 错误。
type ApplyItem struct {
	SpecID  string `json:"spec_id"`
	Target  string `json:"target"`
	Result  string `json:"result"` // ok | failed
	Message string `json:"message"`
}

// DoctorItem 是一条体检项（契约 §3：status 用小写 pass|warn|fail）。
type DoctorItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// --------------------------------------------------------------------------- 信封

// envelope 是统一响应信封：{"ok":bool,"error":{code,message}?,"data":...}。
type envelope struct {
	OK    bool       `json:"ok"`
	Error *ErrorBody `json:"error,omitempty"`
	Data  any        `json:"data,omitempty"`
}

// ErrorBody 是信封里的错误体。
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CodeError 是带稳定错误码的失败。cmd 侧用 Errorf/ErrorStatus 构造，前端按 Code 分支。
type CodeError struct {
	Code    string
	Message string
	Status  int
}

func (e *CodeError) Error() string { return e.Message }

// Errorf 构造一个「请求/前置条件不满足」的失败（默认 HTTP 400）。
func Errorf(code, format string, args ...any) error {
	return &CodeError{Code: code, Message: fmt.Sprintf(format, args...), Status: http.StatusBadRequest}
}

// ErrorStatus 同 Errorf，但显式指定 HTTP 状态码。
func ErrorStatus(status int, code, format string, args ...any) error {
	return &CodeError{Code: code, Message: fmt.Sprintf(format, args...), Status: status}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"ok":false,"error":{"code":"internal","message":"响应序列化失败"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, envelope{OK: true, Data: data})
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, envelope{OK: false, Error: &ErrorBody{Code: code, Message: message}})
}

// writeFail 把后端错误翻译成信封：CodeError 用它的码与状态，其余按 500 处理。
func writeFail(w http.ResponseWriter, err error) {
	var ce *CodeError
	if errors.As(err, &ce) {
		status := ce.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeErr(w, status, ce.Code, ce.Message)
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", err.Error())
}

// decodeBody 解析请求体。未知字段宽容处理（前端加字段不该 400）。
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return Errorf("bad_request", "读取请求体失败: %v", err)
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return Errorf("bad_request", "请求体不是合法 JSON: %v", err)
	}
	return nil
}

// --------------------------------------------------------------------------- 处理器

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	raw, err := readAsset("index.html")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "no_index", "内嵌资源里没有 index.html（构建不完整）")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	st, err := s.cfg.Backend.State(ctx)
	if err != nil {
		writeFail(w, err)
		return
	}
	if st.Adapters == nil {
		st.Adapters = []Adapter{}
	}
	writeOK(w, st)
}

func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	adapters, err := s.cfg.Backend.Detect(ctx)
	if err != nil {
		writeFail(w, err)
		return
	}
	if adapters == nil {
		adapters = []Adapter{}
	}
	writeOK(w, adapters)
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	out, err := s.cfg.Backend.LoginStart(ctx)
	if err != nil {
		writeFail(w, err)
		return
	}
	writeOK(w, out)
}

func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	out, err := s.cfg.Backend.LoginPoll(ctx)
	if err != nil {
		writeFail(w, err)
		return
	}
	writeOK(w, out)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	models, err := s.cfg.Backend.Models(ctx)
	if err != nil {
		writeFail(w, err)
		return
	}
	if models == nil {
		models = []gateway.Model{}
	}
	writeOK(w, map[string]any{"data": models})
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if err := decodeBody(r, &req); err != nil {
		writeFail(w, err)
		return
	}
	if req.SpecID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少 spec_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	plan, err := s.cfg.Backend.Plan(ctx, req)
	if err != nil {
		writeFail(w, err)
		return
	}
	if plan.Items == nil {
		plan.Items = []PlanItem{}
	}
	writeOK(w, plan)
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if err := decodeBody(r, &req); err != nil {
		writeFail(w, err)
		return
	}
	if req.SpecID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少 spec_id")
		return
	}
	// 写盘是破坏性操作（备份 + 改用户配置），必须显式确认；handler 这一层就先拦一道，
	// 后端再拦一道（两道都要求 confirm=true）。
	if !req.Confirm {
		writeErr(w, http.StatusBadRequest, "confirm_required", "未确认：写盘请求必须带 confirm:true")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	items, err := s.cfg.Backend.Apply(ctx, req)
	if err != nil {
		writeFail(w, err)
		return
	}
	if items == nil {
		items = []ApplyItem{}
	}
	writeOK(w, items)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	items, err := s.cfg.Backend.Doctor(ctx)
	if err != nil {
		writeFail(w, err)
		return
	}
	if items == nil {
		items = []DoctorItem{}
	}
	writeOK(w, items)
}

func (s *Server) handleSpecs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	specs, err := s.cfg.Backend.Specs(ctx)
	if err != nil {
		writeFail(w, err)
		return
	}
	if specs == nil {
		specs = []spec.Spec{}
	}
	writeOK(w, specs)
}
