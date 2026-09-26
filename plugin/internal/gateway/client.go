package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// maxResponseBytes 是单个响应体的读取上限。目录与用量都是小 JSON，
	// 4MiB 足够，同时避免被超大响应打爆内存。
	maxResponseBytes = 4 << 20
	// maxErrorBody 是解不出结构化错误体时拼进错误消息的响应体上限。
	maxErrorBody = 8 << 10

	// defaultHTTPTimeout 是默认 http.Client 的整体超时。
	defaultHTTPTimeout = 30 * time.Second
	// defaultPollInterval 是服务端没给 interval 时的轮询间隔，与主仓库
	// internal/account.DefaultPollIntervalMS（5000ms）一致。
	defaultPollInterval = 5 * time.Second
	// defaultDeviceWindow 是服务端没给 expires_in、调用方也没设 DeviceLoginTimeout
	// 时的等待上限，与主仓库 internal/account.DefaultDeviceCodeTTL（10 分钟）一致。
	defaultDeviceWindow = 10 * time.Minute
	// defaultRefreshSkew 是 access token 的提前续期余量，避免边界上"请求刚发出就过期"。
	defaultRefreshSkew = 30 * time.Second

	// minPollInterval / maxPollInterval 是服务端给出的轮询间隔的夹取范围，
	// 防止 0 或天量值把客户端卡死/打爆（与 nextPollInterval 的夹取一致）。
	minPollInterval = time.Second
	maxPollInterval = 60 * time.Second

	// deviceCodePrefix 是设备码前缀（主仓库 internal/account/token.go 的
	// devicePrefix）。用户码没有前缀，用它区分两种 Login 入参。
	deviceCodePrefix = "gwd_"

	userAgent = "ximo-plugin"
)

// 契约 §11.3 的端点路径。
const (
	pathAuthDevice  = "/v1/auth/device"
	pathAuthToken   = "/v1/auth/token"
	pathAuthRefresh = "/v1/auth/refresh"
	pathAuthLogin   = "/v1/auth/login"
	pathModels      = "/v1/models"
	pathUsage       = "/v1/usage"
	pathHealth      = "/v1/health"
)

// Client 是网关客户端。契约 §3 冻结的两个字段是 BaseURL 与 CredPath；其余字段
// 都有合理零值，`&Client{BaseURL: gw, CredPath: p}` 或 NewClient 都能直接用。
//
// Client 可并发使用（内部只在记录轮询间隔时加锁），但**不要**在多进程/多 goroutine
// 里同时对同一个凭据文件做续期：refresh 是轮换的，旧 refresh 立刻失效（见 EnsureCred）。
type Client struct {
	// BaseURL 是网关地址（如 https://gw.example.com，可带子路径）。必填。
	BaseURL string
	// CredPath 是凭据文件路径；空则用 DefaultCredPath()。
	CredPath string

	// HTTP 是底层 HTTP 客户端；nil 时用 30s 超时的默认客户端。测试可注入。
	HTTP *http.Client
	// Logf 是可选日志出口（形如 fmt.Printf）。nil 表示静默 —— 脱敏红线优先，
	// 默认不产生任何输出。传入的内容由本包保证已脱敏。
	Logf func(format string, args ...any)
	// Now 是可选时钟（测试用）；nil 表示 time.Now。
	Now func() time.Time
	// RefreshSkew 是 access token 的提前续期余量；0 表示用 defaultRefreshSkew。
	RefreshSkew time.Duration

	// PollInterval 覆盖设备轮询间隔；0 表示用服务端给出的 interval。
	// 显式设置时不做 [1s,60s] 夹取（测试与特殊部署需要更密的轮询）。
	PollInterval time.Duration
	// DeviceLoginTimeout 是设备登录的总等待上限；0 表示取服务端 expires_in，
	// 再退化到 defaultDeviceWindow。
	DeviceLoginTimeout time.Duration
	// OnDeviceCode 在设备码流程拿到用户码后被调用一次，供 CLI 展示给用户。
	// Login("") 依赖它；StartDeviceLogin 的调用方可以不用（自己打印）。
	OnDeviceCode func(DeviceAuth)

	mu              sync.Mutex
	interval        time.Duration // 服务端给的建议轮询间隔
	deviceExpiresAt time.Time     // 设备码寿命（零值 = 未知）
}

// NewClient 返回一个指向 baseURL 的客户端，凭据文件在 credPath（空则用默认路径）。
// URL 的合法性在第一次请求时校验（也可先用 NormalizeGatewayURL 自查）。
func NewClient(baseURL, credPath string) *Client {
	return &Client{BaseURL: baseURL, CredPath: credPath}
}

// NormalizeGatewayURL 校验并规整网关地址：必须 http/https、必须有主机名、
// 不允许查询串或片段，结尾的 / 会被去掉。
func NormalizeGatewayURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("网关地址非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("网关地址必须以 http:// 或 https:// 开头，收到 %q", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("网关地址缺少主机名: %q", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("网关地址不应带查询串或片段: %q", raw)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

// target 返回规整后的地址与前缀。
func (c *Client) target() (string, error) {
	return NormalizeGatewayURL(c.BaseURL)
}

func (c *Client) setPoll(interval time.Duration, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if interval > 0 {
		c.interval = interval
	}
	c.deviceExpiresAt = expiresAt
}

func (c *Client) pollState() (time.Duration, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interval, c.deviceExpiresAt
}

// request 是一次 JSON 调用。secrets 是"本次请求携带过的凭据"，只用于对回包做
// 最后一道掩码（Redact），不会出现在请求头之外的任何地方。
type request struct {
	method  string
	path    string
	bearer  string
	body    any
	out     any
	secrets []string
}

// do 发一个 JSON 请求并解码响应。bearer 为空表示不带 Authorization
// （登录链的四个端点都不需要）。
func (c *Client) do(ctx context.Context, r request) error {
	base, err := c.target()
	if err != nil {
		return err
	}

	var rdr io.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, r.method, base+r.path, rdr)
	if err != nil {
		return fmt.Errorf("构造请求 %s %s 失败: %w", r.method, r.path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// 传输层错误：只拼方法与路径，请求头/体一律不进错误文本。
		return fmt.Errorf("请求 %s %s 失败: %w", r.method, r.path, redactErr(err, r.secrets...))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("读取 %s 响应失败: %w", r.path, redactErr(err, r.secrets...))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := newAPIError(resp.StatusCode, raw, r.secrets...)
		c.logf("gateway: %s %s -> %s", r.method, r.path, ae.Error())
		return ae
	}
	if r.out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, r.out); err != nil {
		return fmt.Errorf("解析 %s 响应失败: %w", r.path, err)
	}
	return nil
}

// --------------------------------------------------------------------------- 登录链的形状

// deviceWire 对应 POST /v1/auth/device 的响应。
//
// 服务端（internal/gateway/api/meta/auth.go）只回 device_code / user_code /
// expires_in / interval（interval 单位是**秒**，由 ceilSeconds 取整）。这里额外
// 容忍 interval_ms 与 verification_uri*：服务端当前不返回它们，但若将来/别家
// 网关返回则直接采用，而不是按秒误判。
type deviceWire struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
	IntervalMS              int64  `json:"interval_ms"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
}

// tokenWire 是登录链三个换发令牌端点的统一响应体。
type tokenWire struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// Health 对应 GET /v1/health。
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	TimeMS  int64  `json:"time_ms"`
}

// DeviceAuth 是一次设备授权登录的开始信息。
type DeviceAuth struct {
	// GatewayURL 是发起该流程的网关地址（规整后）。
	GatewayURL string
	// DeviceCode 是给机器用的设备码（gwd_ 前缀），等价于短期凭据：
	// 任何输出/日志都不得出现明文（本包内部只用 Mask 后的形式展示）。
	DeviceCode string
	// UserCode 是给人抄的用户码，需要用户填进网关授权页。
	UserCode string
	// ExpiresIn 是设备码寿命；0 表示服务端没给（此时按 DeviceLoginTimeout/
	// 默认窗口处理）。服务端真实返回该字段，正常不为 0。
	ExpiresIn time.Duration
	// Interval 是服务端要求的轮询间隔（已夹取到 [1s,60s]）。
	Interval time.Duration
	// VerificationURI 是用户授权页地址。V1 服务端的 /v1/auth/device **不返回**
	// 该字段（deviceResponse 只有四个字段），因此通常为空 —— 调用方自行提示用户
	// 去网关的授权页，不要臆造地址。
	VerificationURI string
}

// devicePollInterval 解析服务端给的轮询间隔。单位歧义一律按下面三条规则处理：
// 显式 interval_ms 优先；否则 interval > 1000 视为毫秒，其余按秒（RFC 8628）；
// 都没有则用 defaultPollInterval。结果夹在 [minPollInterval, maxPollInterval]。
func (r deviceWire) devicePollInterval() time.Duration {
	var d time.Duration
	switch {
	case r.IntervalMS > 0:
		d = time.Duration(r.IntervalMS) * time.Millisecond
	case r.Interval > 0:
		if r.Interval > 1000 {
			d = time.Duration(r.Interval) * time.Millisecond
		} else {
			d = time.Duration(r.Interval) * time.Second
		}
	default:
		d = defaultPollInterval
	}
	if d < minPollInterval {
		return minPollInterval
	}
	if d > maxPollInterval {
		return maxPollInterval
	}
	return d
}

// StartDeviceLogin 申请设备码（POST /v1/auth/device，无鉴权）。
func (c *Client) StartDeviceLogin(ctx context.Context) (DeviceAuth, error) {
	base, err := c.target()
	if err != nil {
		return DeviceAuth{}, err
	}
	var out deviceWire
	// 请求体带客户端自述，便于网关侧审计；服务端忽略未知字段。
	body := map[string]string{"client": userAgent}
	if err := c.do(ctx, request{method: http.MethodPost, path: pathAuthDevice, body: body, out: &out}); err != nil {
		return DeviceAuth{}, fmt.Errorf("发起设备码登录失败: %w", err)
	}

	da := DeviceAuth{
		GatewayURL:      base,
		DeviceCode:      strings.TrimSpace(out.DeviceCode),
		UserCode:        strings.TrimSpace(out.UserCode),
		Interval:        out.devicePollInterval(),
		VerificationURI: firstNonEmpty(out.VerificationURIComplete, out.VerificationURI),
	}
	if out.ExpiresIn > 0 {
		da.ExpiresIn = time.Duration(out.ExpiresIn) * time.Second
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return DeviceAuth{}, errors.New("网关 /v1/auth/device 响应缺少 device_code 或 user_code（无法继续授权）")
	}

	if c.PollInterval > 0 {
		da.Interval = c.PollInterval
	}
	var expiresAt time.Time
	if da.ExpiresIn > 0 {
		expiresAt = c.now().Add(da.ExpiresIn)
	}
	c.setPoll(da.Interval, expiresAt)

	// 日志只记轮询间隔与掩码后的设备码。**不记用户码**：它是给人抄的一次性展示码，
	// 由调用方（CLI）直接显示给用户，没有必要落进日志。
	c.logf("gateway: 已申请设备码（轮询间隔 %s，设备码 %s）", da.Interval, Mask(da.DeviceCode))
	return da, nil
}

// PollDeviceLoginOnce 轮询一次设备码（POST /v1/auth/token）。
//
// 尚未授权时**原样返回**服务端的 400 authorization_pending 错误（因此
// errors.Is(err, ErrAuthorizationPending) 与 errors.As(err, **APIError) 都成立），
// 由调用方决定继续等还是放弃。设备码已过期/已使用返回 ErrCredentialExpired /
// ErrCredentialRevoked。
func (c *Client) PollDeviceLoginOnce(ctx context.Context, deviceCode string) (Cred, error) {
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return Cred{}, ErrDeviceCodeEmpty
	}
	base, err := c.target()
	if err != nil {
		return Cred{}, err
	}

	var out tokenWire
	err = c.do(ctx, request{
		method: http.MethodPost,
		path:   pathAuthToken,
		body:   map[string]string{"device_code": deviceCode},
		out:    &out,
		// 设备码等价于短期凭据：回包里若回显它，必须被掩掉。
		secrets: []string{deviceCode},
	})
	if err != nil {
		return Cred{}, err
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return Cred{}, errors.New("网关 /v1/auth/token 响应缺少 access_token：按失败处理，不保存半截凭据")
	}
	return c.credFromToken(base, "", out), nil
}

// PollDeviceLogin 按服务端要求的间隔轮询设备码，直到成功、超时或明确的失败。
//
// 会先等待再轮询（既遵守 interval，也避免刚拿到设备码就打网关）。等待窗口取三者
// 的最小值：ctx 截止时间、DeviceLoginTimeout、设备码寿命（expires_in）；都没有时
// 用 defaultDeviceWindow（10 分钟，与服务端设备码寿命一致）。超时返回 ErrLoginTimeout。
//
// 成功后凭据会写入 CredPath（原子写 + 0600）；写盘失败时返回的 Cred 仍然有效
// （见 EnsureCred 的说明）。
func (c *Client) PollDeviceLogin(ctx context.Context, deviceCode string) (Cred, error) {
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return Cred{}, ErrDeviceCodeEmpty
	}

	interval := c.PollInterval
	// override 表示调用方显式指定了轮询间隔：此时不做 [1s,60s] 夹取、也不按
	// 连续 pending 增长（测试与"受控部署"需要固定节奏）。
	override := interval > 0
	stored, deviceExpiresAt := c.pollState()
	if !override {
		interval = defaultPollInterval
		if stored > 0 {
			interval = stored
		}
	}
	deadline := c.loginDeadline(ctx, deviceExpiresAt)
	window := durationText(deadline.Sub(c.now()))

	pending := 0
	for {
		// 等待不超过剩余的授权窗口：服务端给 5s 间隔而窗口只剩 100ms 时，
		// 不能傻等 5s 才报超时。
		wait := interval
		if left := deadline.Sub(c.now()); left < wait {
			wait = left
		}
		if !c.now().Before(deadline) {
			return Cred{}, fmt.Errorf("%w：%s 内未完成授权，请重新登录", ErrLoginTimeout, window)
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return Cred{}, fmt.Errorf("%w：已被取消（%v）", ErrLoginTimeout, err)
		}
		if !c.now().Before(deadline) {
			return Cred{}, fmt.Errorf("%w：%s 内未完成授权，请重新登录", ErrLoginTimeout, window)
		}

		cred, err := c.PollDeviceLoginOnce(ctx, deviceCode)
		switch {
		case err == nil:
			cred.Username = c.usernameHint()
			return cred, c.SaveCred(cred)
		case errors.Is(err, ErrAuthorizationPending):
			pending++
			if !override {
				interval = nextPollInterval(interval, pending, false)
			}
			c.logf("gateway: 等待用户在授权页确认（第 %d 次轮询，下次间隔 %s）", pending, interval)
		case errors.Is(err, ErrSlowDown):
			pending++
			if !override {
				interval = nextPollInterval(interval, pending, true)
			}
			c.logf("gateway: 服务端要求放慢轮询（下次间隔 %s）", interval)
		default:
			return Cred{}, err
		}
	}
}

// Login 完成一次登录并返回凭据（契约 §3）。ref 的语义：
//
//   - ""：发起完整设备码流程。用户码通过 OnDeviceCode 交给调用方展示；未设置
//     回调时直接报错（本包不能替调用方把用户码写到终端）。
//   - "gwd_..."：直接轮询该设备码（设备流由调用方自己用 StartDeviceLogin 发起）。
//   - 其它串（用户码）：报 ErrUserCodeNotUsable —— V1 的 /v1/auth/device 只回
//     device_code 与 user_code，没有"用用户码换令牌"的端点，用户码必须由用户在
//     网关授权页输入。
//
// 用户名口令登录请用 LoginPassword。两者都会把凭据写入 CredPath。
func (c *Client) Login(ctx context.Context, ref string) (Cred, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		da, err := c.StartDeviceLogin(ctx)
		if err != nil {
			return Cred{}, err
		}
		if c.OnDeviceCode == nil {
			return Cred{}, errors.New("Login(\"\") 需要设置 OnDeviceCode 回调来把用户码展示给用户；" +
				"或改用 StartDeviceLogin + PollDeviceLogin")
		}
		c.OnDeviceCode(da)
		return c.PollDeviceLogin(ctx, da.DeviceCode)
	case strings.HasPrefix(ref, deviceCodePrefix):
		return c.PollDeviceLogin(ctx, ref)
	default:
		return Cred{}, fmt.Errorf("%w（收到 %s）：请在网关授权页输入该用户码，或把设备码（%s 前缀）交给本命令",
			ErrUserCodeNotUsable, Mask(ref), deviceCodePrefix)
	}
}

// LoginPassword 用用户名口令登录（POST /v1/auth/login）并落盘。
// 口令只出现在请求体里：不进日志、不进错误文本（回包若回显也会被掩码）。
func (c *Client) LoginPassword(ctx context.Context, username, password string) (Cred, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return Cred{}, errors.New("登录需要用户名与口令")
	}
	base, err := c.target()
	if err != nil {
		return Cred{}, err
	}

	var out tokenWire
	err = c.do(ctx, request{
		method:  http.MethodPost,
		path:    pathAuthLogin,
		body:    map[string]string{"username": username, "password": password},
		out:     &out,
		secrets: []string{password},
	})
	if err != nil {
		return Cred{}, fmt.Errorf("口令登录失败: %w", err)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return Cred{}, errors.New("网关 /v1/auth/login 响应缺少 access_token：按失败处理，不保存半截凭据")
	}
	cred := c.credFromToken(base, username, out)
	return cred, c.SaveCred(cred)
}

// Refresh 用 refresh token 换一对新令牌（POST /v1/auth/refresh）。服务端是轮换的：
// 旧 refresh 立刻失效。本方法**不写盘**，需要持久化请用 SaveCred 或 EnsureCred。
func (c *Client) Refresh(ctx context.Context, cred Cred) (Cred, error) {
	refreshToken := strings.TrimSpace(cred.RefreshToken)
	if refreshToken == "" {
		return cred, fmt.Errorf("%w：没有 refresh_token，请重新登录", ErrNoCredential)
	}
	base, err := c.target()
	if err != nil {
		return cred, err
	}

	var out tokenWire
	err = c.do(ctx, request{
		method:  http.MethodPost,
		path:    pathAuthRefresh,
		body:    map[string]string{"refresh_token": refreshToken},
		out:     &out,
		secrets: []string{refreshToken},
	})
	if err != nil {
		if errors.Is(err, ErrCredentialRevoked) || errors.Is(err, ErrCredentialExpired) || errors.Is(err, ErrUnauthorized) {
			return cred, fmt.Errorf("refresh token 已失效，请重新登录: %w", err)
		}
		return cred, fmt.Errorf("续期令牌失败: %w", err)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return cred, errors.New("网关 /v1/auth/refresh 响应缺少 access_token：按失败处理")
	}

	next := cred
	next.Gateway = base
	next.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		next.RefreshToken = out.RefreshToken
	}
	next.AccessExpiresAt = 0
	if out.ExpiresIn > 0 {
		next.AccessExpiresAt = c.now().Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
	}
	next.UpdatedAt = c.now().Unix()
	// 服务端不返回 refresh 的到期时间（tokenResponse 只有 expires_in，指 access 的
	// 寿命），所以这里保留原值而不是臆造一个新的。
	c.logf("gateway: 已续期令牌（access %s）", Mask(next.AccessToken))
	return next, nil
}

// Health 探活（GET /v1/health，无鉴权），供 doctor 使用。
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	if err := c.do(ctx, request{method: http.MethodGet, path: pathHealth, out: &out}); err != nil {
		return Health{}, err
	}
	return out, nil
}

// credFromToken 把一次换发令牌的结果转成凭据。username 为空时不改动既有值
// （轮询续期场景由调用方补齐）。
func (c *Client) credFromToken(gateway, username string, t tokenWire) Cred {
	now := c.now()
	cred := Cred{
		Gateway:      gateway,
		Username:     username,
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		UpdatedAt:    now.Unix(),
	}
	if t.ExpiresIn > 0 {
		cred.AccessExpiresAt = now.Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
	}
	return cred
}

// loginDeadline 取 ctx 截止时间、DeviceLoginTimeout、设备码寿命三者的最早值。
func (c *Client) loginDeadline(ctx context.Context, deviceExpiresAt time.Time) time.Time {
	deadline := c.now().Add(defaultDeviceWindow)
	if !deviceExpiresAt.IsZero() && deviceExpiresAt.Before(deadline) {
		deadline = deviceExpiresAt
	}
	if c.DeviceLoginTimeout > 0 {
		if d := c.now().Add(c.DeviceLoginTimeout); d.Before(deadline) {
			deadline = d
		}
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}

// nextPollInterval 决定下一次轮询的等待时长：slow_down 加 5 秒；连续 10 次
// authorization_pending 后乘 1.5（长时间挂机时减少对网关的压力）；其余保持服务端
// 给的间隔。结果夹在 [minPollInterval, maxPollInterval]。
func nextPollInterval(cur time.Duration, consecutivePending int, slowDown bool) time.Duration {
	next := cur
	switch {
	case slowDown:
		next = cur + 5*time.Second
	case consecutivePending >= 10:
		next = cur * 3 / 2
	}
	if next < minPollInterval {
		return minPollInterval
	}
	if next > maxPollInterval {
		return maxPollInterval
	}
	return next
}

// sleepCtx 可被 ctx 打断的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// durationText 把等待窗口渲染成人类可读文本：不足 1 秒时用毫秒，
// 避免把 60ms 的窗口打印成 "0s"。
func durationText(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// usernameHint 返回已保存凭据里的用户名（若有），只用于展示。
func (c *Client) usernameHint() string {
	if cred, err := c.LoadCred(); err == nil {
		return cred.Username
	}
	return ""
}
