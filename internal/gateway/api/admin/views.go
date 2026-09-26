package admin

import "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"

// 本文件是库内类型 → 响应体的显式视图。刻意不用模型类型直接序列化：
//   - model.User.PasswordHash 绝不能被 JSON 编码出去，靠显式字段而不是 json:"-" 兜底；
//   - model.APIKey.KeyHash 同理（只回 key_prefix 供识别）；
//   - 时间戳保持 unix 毫秒整数，与库内一致，前端自行换算。

type userView struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Status    string `json:"status"`
	GroupID   string `json:"group_id"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func viewUser(u model.User) userView {
	return userView{
		ID:        u.ID,
		Username:  u.Username,
		Status:    u.Status,
		GroupID:   u.GroupID,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}

func viewUsers(us []model.User) []userView {
	out := make([]userView, 0, len(us))
	for _, u := range us {
		out = append(out, viewUser(u))
	}
	return out
}

type keyView struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	KeyPrefix  string `json:"key_prefix"`
	Status     string `json:"status"`
	ExpiresAt  int64  `json:"expires_at"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
}

func viewKey(k model.APIKey) keyView {
	return keyView{
		ID:         k.ID,
		UserID:     k.UserID,
		KeyPrefix:  k.KeyPrefix,
		Status:     k.Status,
		ExpiresAt:  k.ExpiresAt,
		CreatedAt:  k.CreatedAt,
		LastUsedAt: k.LastUsedAt,
	}
}

func viewKeys(ks []model.APIKey) []keyView {
	out := make([]keyView, 0, len(ks))
	for _, k := range ks {
		out = append(out, viewKey(k))
	}
	return out
}

type ledgerView struct {
	ID             string `json:"id"`
	UserID         string `json:"user_id"`
	Type           string `json:"type"`
	RequestID      string `json:"request_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	OperatorID     string `json:"operator_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Amount         int64  `json:"amount"`
	BalanceAfter   int64  `json:"balance_after"`
	ReservedAfter  int64  `json:"reserved_after"`
	CreatedAt      int64  `json:"created_at"`
}

func viewLedger(e model.LedgerEntry) ledgerView {
	return ledgerView{
		ID:             e.ID,
		UserID:         e.UserID,
		Type:           e.Type,
		RequestID:      e.RequestID,
		IdempotencyKey: e.IdempotencyKey,
		OperatorID:     e.OperatorID,
		Reason:         e.Reason,
		Amount:         e.Amount,
		BalanceAfter:   e.BalanceAfter,
		ReservedAfter:  e.ReservedAfter,
		CreatedAt:      e.CreatedAt,
	}
}

func viewLedgers(es []model.LedgerEntry) []ledgerView {
	out := make([]ledgerView, 0, len(es))
	for _, e := range es {
		out = append(out, viewLedger(e))
	}
	return out
}

// accountView 额外给出 available：调用方不必自己按 total-used-reserved 重算。
type accountView struct {
	UserID         string `json:"user_id"`
	TotalAmount    int64  `json:"total_amount"`
	UsedAmount     int64  `json:"used_amount"`
	ReservedAmount int64  `json:"reserved_amount"`
	Available      int64  `json:"available"`
	Version        int64  `json:"version"`
	Status         string `json:"status"`
	UpdatedAt      int64  `json:"updated_at"`
}

func viewAccount(a model.QuotaAccount) accountView {
	return accountView{
		UserID:         a.UserID,
		TotalAmount:    a.TotalAmount,
		UsedAmount:     a.UsedAmount,
		ReservedAmount: a.ReservedAmount,
		Available:      a.Available(),
		Version:        a.Version,
		Status:         a.Status,
		UpdatedAt:      a.UpdatedAt,
	}
}

type modelView struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	Capabilities any    `json:"capabilities"`
	Enabled      bool   `json:"enabled"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

func viewModel(m model.ModelSpec) modelView {
	return modelView{
		ID:           m.ModelID,
		DisplayName:  m.DisplayName,
		Capabilities: jsonField(m.CapabilitiesJSON),
		Enabled:      m.Enabled,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
}

func viewModels(ms []model.ModelSpec) []modelView {
	out := make([]modelView, 0, len(ms))
	for _, m := range ms {
		out = append(out, viewModel(m))
	}
	return out
}

// providerView 只回 api_key_ref（secretref），绝不回明文密钥。
type providerView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Protocol  string `json:"protocol"`
	Status    string `json:"status"`
	APIKeyRef string `json:"api_key_ref,omitempty"`
	Config    any    `json:"config"`
	TimeoutMS int64  `json:"timeout_ms"`
	Weight    int64  `json:"weight"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func viewProvider(p model.ProviderSpec) providerView {
	return providerView{
		ID:        p.ID,
		Name:      p.Name,
		Endpoint:  p.Endpoint,
		Protocol:  p.Protocol,
		Status:    p.Status,
		APIKeyRef: p.APIKeyRef,
		Config:    jsonField(p.ConfigJSON),
		TimeoutMS: p.TimeoutMS,
		Weight:    p.Weight,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

func viewProviders(ps []model.ProviderSpec) []providerView {
	out := make([]providerView, 0, len(ps))
	for _, p := range ps {
		out = append(out, viewProvider(p))
	}
	return out
}

type usageView struct {
	RequestID    string `json:"request_id"`
	UserID       string `json:"user_id"`
	ModelID      string `json:"model_id"`
	ProviderID   string `json:"provider_id"`
	Status       string `json:"status"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	LatencyMS    int64  `json:"latency_ms"`
	CostMicro    int64  `json:"cost_micro"`
	CreatedAt    int64  `json:"created_at"`
}

func viewUsages(us []model.UsageRecord) []usageView {
	out := make([]usageView, 0, len(us))
	for _, u := range us {
		out = append(out, usageView{
			RequestID:    u.RequestID,
			UserID:       u.UserID,
			ModelID:      u.ModelID,
			ProviderID:   u.ProviderID,
			Status:       u.Status,
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			LatencyMS:    u.LatencyMS,
			CostMicro:    u.CostMicro,
			CreatedAt:    u.CreatedAt,
		})
	}
	return out
}

type auditView struct {
	ID        string `json:"id"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Result    string `json:"result"`
	IP        string `json:"ip"`
	Detail    any    `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}

func viewAudits(as []model.AuditLog) []auditView {
	out := make([]auditView, 0, len(as))
	for _, a := range as {
		out = append(out, auditView{
			ID:        a.ID,
			Actor:     a.Actor,
			Action:    a.Action,
			Target:    a.Target,
			Result:    a.Result,
			IP:        a.IP,
			Detail:    jsonField(a.DetailJSON),
			CreatedAt: a.CreatedAt,
		})
	}
	return out
}
