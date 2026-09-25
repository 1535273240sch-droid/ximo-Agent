package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// ---------------------------------------------------------------- providers

// Provider 对应 providers 表。注意：api_key_ref 只是密钥引用（secrets 模块
// 的键名），绝不存明文密钥（I12）。
type Provider struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	BaseURL   string          `json:"baseUrl,omitempty"`
	APIKeyRef string          `json:"-"`
	Model     string          `json:"model,omitempty"`
	Enabled   bool            `json:"enabled"`
	Priority  int64           `json:"priority"`
	Config    json.RawMessage `json:"config,omitempty"`
	CreatedAt int64           `json:"createdAt"`
	UpdatedAt int64           `json:"updatedAt"`
}

// ProviderRepo 管 providers 表。
type ProviderRepo struct{ db *sqlite.DB }

// Upsert 插入或更新（按 ID）。
func (r *ProviderRepo) Upsert(ctx context.Context, p *Provider) error {
	now := time.Now().UnixMilli()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO providers (id, name, type, base_url, api_key_ref, model, enabled, priority, config_json, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type, base_url=excluded.base_url,
			   api_key_ref=excluded.api_key_ref, model=excluded.model, enabled=excluded.enabled,
			   priority=excluded.priority, config_json=excluded.config_json, updated_at=excluded.updated_at`,
			p.ID, p.Name, p.Type, nullStr(p.BaseURL), nullStr(p.APIKeyRef), nullStr(p.Model),
			boolToInt(p.Enabled), p.Priority, jsonOrNull(p.Config), p.CreatedAt, p.UpdatedAt)
		return wrapErr("provider upsert", err)
	})
}

// List 列出 providers（优先级降序）。
func (r *ProviderRepo) List(ctx context.Context) ([]*Provider, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, type, base_url, api_key_ref, model, enabled, priority, config_json, created_at, updated_at
		 FROM providers ORDER BY priority DESC, created_at ASC`)
	if err != nil {
		return nil, wrapErr("provider list", err)
	}
	defer rows.Close()
	var out []*Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, wrapErr("provider list scan", err)
		}
		out = append(out, p)
	}
	return out, wrapErr("provider list iterate", rows.Err())
}

// ListEnabled 列出启用的 providers。
func (r *ProviderRepo) ListEnabled(ctx context.Context) ([]*Provider, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, type, base_url, api_key_ref, model, enabled, priority, config_json, created_at, updated_at
		 FROM providers WHERE enabled=1 ORDER BY priority DESC, created_at ASC`)
	if err != nil {
		return nil, wrapErr("provider list enabled", err)
	}
	defer rows.Close()
	var out []*Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, wrapErr("provider list enabled scan", err)
		}
		out = append(out, p)
	}
	return out, wrapErr("provider list enabled iterate", rows.Err())
}

// Delete 删除 provider。
func (r *ProviderRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE id=?`, id)
		return wrapErr("provider delete", err)
	})
}

func scanProvider(rows *sql.Rows) (*Provider, error) {
	var p Provider
	var baseURL, keyRef, model, config sql.NullString
	var enabled int64
	if err := rows.Scan(&p.ID, &p.Name, &p.Type, &baseURL, &keyRef, &model, &enabled,
		&p.Priority, &config, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.BaseURL = scanNullStr(baseURL)
	p.APIKeyRef = scanNullStr(keyRef)
	p.Model = scanNullStr(model)
	p.Enabled = enabled != 0
	if config.Valid {
		p.Config = json.RawMessage(config.String)
	}
	return &p, nil
}

// ---------------------------------------------------------------- permissions

// 权限决策常量。
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
	DecisionAsk   = "ask"
)

// Permission 对应 permissions 表。
type Permission struct {
	ID          string        `json:"id"`
	SubjectType string        `json:"subjectType"`
	SubjectID   string        `json:"subjectId"`
	Action      string        `json:"action"`
	Decision    string        `json:"decision"`
	Scope       string        `json:"scope"`
	GrantedBy   string        `json:"grantedBy,omitempty"`
	CreatedAt   int64         `json:"createdAt"`
	ExpiresAt   sql.NullInt64 `json:"-"`
}

// PermissionRepo 管 permissions 表。
type PermissionRepo struct{ db *sqlite.DB }

// Grant 授予权限（同 subject+action+scope 覆盖更新）。
func (r *PermissionRepo) Grant(ctx context.Context, p *Permission) error {
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().UnixMilli()
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO permissions (id, subject_type, subject_id, action, decision, scope, granted_by, created_at, expires_at)
			 VALUES (?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(subject_type, subject_id, action, scope)
			 DO UPDATE SET decision=excluded.decision, granted_by=excluded.granted_by,
			   created_at=excluded.created_at, expires_at=excluded.expires_at`,
			p.ID, p.SubjectType, p.SubjectID, p.Action, p.Decision, p.Scope,
			nullStr(p.GrantedBy), p.CreatedAt, p.ExpiresAt)
		return wrapErr("permission grant", err)
	})
}

// Check 查询对 subject+action 的决策。返回 decision 与是否存在；
// scope 优先精确匹配，其次空 scope（全局）。已过期的条目被忽略。
func (r *PermissionRepo) Check(ctx context.Context, subjectType, subjectID, action, scope string) (decision string, found bool, err error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT decision, scope, expires_at FROM permissions
		 WHERE subject_type=? AND subject_id=? AND action=? AND (scope=? OR scope='')`,
		subjectType, subjectID, action, scope)
	if err != nil {
		return "", false, wrapErr("permission check", err)
	}
	defer rows.Close()
	now := time.Now().UnixMilli()
	best := ""
	bestScore := -1
	for rows.Next() {
		var d, s string
		var exp sql.NullInt64
		if err := rows.Scan(&d, &s, &exp); err != nil {
			return "", false, wrapErr("permission check scan", err)
		}
		if exp.Valid && exp.Int64 > 0 && exp.Int64 < now {
			continue // 已过期
		}
		score := 0
		if s == scope && scope != "" {
			score = 2
		} else if s == "" {
			score = 1
		}
		if score > bestScore {
			best, bestScore = d, score
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, wrapErr("permission check iterate", err)
	}
	if bestScore < 0 {
		return "", false, nil
	}
	return best, true, nil
}

// Revoke 撤销权限。
func (r *PermissionRepo) Revoke(ctx context.Context, subjectType, subjectID, action, scope string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM permissions WHERE subject_type=? AND subject_id=? AND action=? AND scope=?`,
			subjectType, subjectID, action, scope)
		return wrapErr("permission revoke", err)
	})
}

// ---------------------------------------------------------------- mcp_servers

// McpServer 对应 mcp_servers 表（v1 mcp-config.json 的对等结构）。
type McpServer struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Enabled    bool              `json:"enabled"`
	ImportedAt int64             `json:"importedAt"`
	Config     json.RawMessage   `json:"config,omitempty"`
}

// McpServerRepo 管 mcp_servers 表。
type McpServerRepo struct{ db *sqlite.DB }

// Upsert 插入或更新（按 ID）。
func (r *McpServerRepo) Upsert(ctx context.Context, s *McpServer) error {
	if s.ImportedAt == 0 {
		s.ImportedAt = time.Now().UnixMilli()
	}
	args, err := json.Marshal(s.Args)
	if err != nil {
		return wrapErr("mcp marshal args", err)
	}
	env, err := json.Marshal(s.Env)
	if err != nil {
		return wrapErr("mcp marshal env", err)
	}
	headers, err := json.Marshal(s.Headers)
	if err != nil {
		return wrapErr("mcp marshal headers", err)
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx,
			`INSERT INTO mcp_servers (id, name, transport, command, args_json, env_json, url, headers_json, enabled, imported_at, config_json)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, transport=excluded.transport, command=excluded.command,
			   args_json=excluded.args_json, env_json=excluded.env_json, url=excluded.url,
			   headers_json=excluded.headers_json, enabled=excluded.enabled,
			   imported_at=excluded.imported_at, config_json=excluded.config_json`,
			s.ID, s.Name, s.Transport, nullStr(s.Command), args, env, nullStr(s.URL), headers,
			boolToInt(s.Enabled), s.ImportedAt, jsonOrNull(s.Config))
		return wrapErr("mcp upsert", e)
	})
}

// List 列出 MCP 服务器。
func (r *McpServerRepo) List(ctx context.Context) ([]*McpServer, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, transport, command, args_json, env_json, url, headers_json, enabled, imported_at, config_json
		 FROM mcp_servers ORDER BY imported_at ASC`)
	if err != nil {
		return nil, wrapErr("mcp list", err)
	}
	defer rows.Close()
	var out []*McpServer
	for rows.Next() {
		s, err := scanMcpServer(rows)
		if err != nil {
			return nil, wrapErr("mcp list scan", err)
		}
		out = append(out, s)
	}
	return out, wrapErr("mcp list iterate", rows.Err())
}

// Delete 删除 MCP 服务器。
func (r *McpServerRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id=?`, id)
		return wrapErr("mcp delete", err)
	})
}

func scanMcpServer(rows *sql.Rows) (*McpServer, error) {
	var s McpServer
	var command, url, config sql.NullString
	var args, env, headers []byte
	var enabled int64
	if err := rows.Scan(&s.ID, &s.Name, &s.Transport, &command, &args, &env, &url, &headers, &enabled, &s.ImportedAt, &config); err != nil {
		return nil, err
	}
	s.Command = scanNullStr(command)
	s.URL = scanNullStr(url)
	s.Enabled = enabled != 0
	if len(args) > 0 {
		_ = json.Unmarshal(args, &s.Args)
	}
	if len(env) > 0 {
		_ = json.Unmarshal(env, &s.Env)
	}
	if len(headers) > 0 {
		_ = json.Unmarshal(headers, &s.Headers)
	}
	if config.Valid {
		s.Config = json.RawMessage(config.String)
	}
	return &s, nil
}

// ---------------------------------------------------------------- experts

// Expert 对应 experts 表（v1 experts.json 对等结构）。
type Expert struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Enabled     bool     `json:"enabled"`
	Source      string   `json:"source"`
	CreatedAt   int64    `json:"createdAt"`
	UpdatedAt   int64    `json:"updatedAt"`
}

// ExpertRepo 管 experts 表。
type ExpertRepo struct{ db *sqlite.DB }

// Upsert 插入或更新（按 ID）。
func (r *ExpertRepo) Upsert(ctx context.Context, e *Expert) error {
	now := time.Now().UnixMilli()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	if e.Source == "" {
		e.Source = "custom"
	}
	tools, err := json.Marshal(e.Tools)
	if err != nil {
		return wrapErr("expert marshal tools", err)
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, e2 := tx.ExecContext(ctx,
			`INSERT INTO experts (id, name, description, prompt, model, tools_json, enabled, source, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, description=excluded.description,
			   prompt=excluded.prompt, model=excluded.model, tools_json=excluded.tools_json,
			   enabled=excluded.enabled, source=excluded.source, updated_at=excluded.updated_at`,
			e.ID, e.Name, nullStr(e.Description), nullStr(e.Prompt), nullStr(e.Model),
			tools, boolToInt(e.Enabled), e.Source, e.CreatedAt, e.UpdatedAt)
		return wrapErr("expert upsert", e2)
	})
}

// List 列出专家。
func (r *ExpertRepo) List(ctx context.Context) ([]*Expert, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, description, prompt, model, tools_json, enabled, source, created_at, updated_at
		 FROM experts ORDER BY created_at ASC`)
	if err != nil {
		return nil, wrapErr("expert list", err)
	}
	defer rows.Close()
	var out []*Expert
	for rows.Next() {
		e, err := scanExpert(rows)
		if err != nil {
			return nil, wrapErr("expert list scan", err)
		}
		out = append(out, e)
	}
	return out, wrapErr("expert list iterate", rows.Err())
}

// Delete 删除专家。
func (r *ExpertRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM experts WHERE id=?`, id)
		return wrapErr("expert delete", err)
	})
}

func scanExpert(rows *sql.Rows) (*Expert, error) {
	var e Expert
	var desc, prompt, model sql.NullString
	var tools []byte
	var enabled int64
	if err := rows.Scan(&e.ID, &e.Name, &desc, &prompt, &model, &tools, &enabled, &e.Source, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.Description = scanNullStr(desc)
	e.Prompt = scanNullStr(prompt)
	e.Model = scanNullStr(model)
	e.Enabled = enabled != 0
	if len(tools) > 0 {
		_ = json.Unmarshal(tools, &e.Tools)
	}
	return &e, nil
}

// ---------------------------------------------------------------- skills

// Skill 对应 skills 表（v1 skills.json / imported-skills.json 对等结构）。
type Skill struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Source      string          `json:"source"`
	Steps       json.RawMessage `json:"steps,omitempty"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   int64           `json:"createdAt"`
	UpdatedAt   int64           `json:"updatedAt"`
}

// SkillRepo 管 skills 表。
type SkillRepo struct{ db *sqlite.DB }

// Upsert 插入或更新（按 ID）。
func (r *SkillRepo) Upsert(ctx context.Context, s *Skill) error {
	now := time.Now().UnixMilli()
	if s.CreatedAt == 0 {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Source == "" {
		s.Source = "custom"
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO skills (id, name, description, source, steps_json, enabled, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, description=excluded.description,
			   source=excluded.source, steps_json=excluded.steps_json, enabled=excluded.enabled,
			   updated_at=excluded.updated_at`,
			s.ID, s.Name, nullStr(s.Description), s.Source, jsonOrNull(s.Steps),
			boolToInt(s.Enabled), s.CreatedAt, s.UpdatedAt)
		return wrapErr("skill upsert", err)
	})
}

// List 列出技能。
func (r *SkillRepo) List(ctx context.Context) ([]*Skill, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, description, source, steps_json, enabled, created_at, updated_at
		 FROM skills ORDER BY created_at ASC`)
	if err != nil {
		return nil, wrapErr("skill list", err)
	}
	defer rows.Close()
	var out []*Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, wrapErr("skill list scan", err)
		}
		out = append(out, s)
	}
	return out, wrapErr("skill list iterate", rows.Err())
}

// Delete 删除技能。
func (r *SkillRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM skills WHERE id=?`, id)
		return wrapErr("skill delete", err)
	})
}

func scanSkill(rows *sql.Rows) (*Skill, error) {
	var s Skill
	var desc, steps sql.NullString
	var enabled int64
	if err := rows.Scan(&s.ID, &s.Name, &desc, &s.Source, &steps, &enabled, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.Description = scanNullStr(desc)
	s.Enabled = enabled != 0
	if steps.Valid {
		s.Steps = json.RawMessage(steps.String)
	}
	return &s, nil
}

// ---------------------------------------------------------------- knowledge_entries

// KnowledgeEntry 对应 knowledge_entries 表（v1 KnowledgeStore 对等结构，
// 按 mode 隔离）。
type KnowledgeEntry struct {
	ID        string   `json:"id"`
	Mode      string   `json:"mode"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags,omitempty"`
	Source    string   `json:"source,omitempty"`
	CreatedAt int64    `json:"createdAt"`
	UpdatedAt int64    `json:"updatedAt"`
}

// KnowledgeRepo 管 knowledge_entries 表。
type KnowledgeRepo struct{ db *sqlite.DB }

// Upsert 插入或更新（按 ID）。
func (r *KnowledgeRepo) Upsert(ctx context.Context, e *KnowledgeEntry) error {
	now := time.Now().UnixMilli()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	if e.Mode == "" {
		e.Mode = SessionModeDefault
	}
	tags, err := json.Marshal(e.Tags)
	if err != nil {
		return wrapErr("knowledge marshal tags", err)
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, e2 := tx.ExecContext(ctx,
			`INSERT INTO knowledge_entries (id, mode, title, content, tags_json, source, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET mode=excluded.mode, title=excluded.title, content=excluded.content,
			   tags_json=excluded.tags_json, source=excluded.source, updated_at=excluded.updated_at`,
			e.ID, e.Mode, e.Title, e.Content, tags, nullStr(e.Source), e.CreatedAt, e.UpdatedAt)
		return wrapErr("knowledge upsert", e2)
	})
}

// ListByMode 按 mode 分页列出（更新时间倒序）。
func (r *KnowledgeRepo) ListByMode(ctx context.Context, mode string, limit, offset int) ([]*KnowledgeEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, mode, title, content, tags_json, source, created_at, updated_at
		 FROM knowledge_entries WHERE mode=? ORDER BY updated_at DESC LIMIT ? OFFSET ?`, mode, limit, offset)
	if err != nil {
		return nil, wrapErr("knowledge list", err)
	}
	defer rows.Close()
	var out []*KnowledgeEntry
	for rows.Next() {
		e, err := scanKnowledge(rows)
		if err != nil {
			return nil, wrapErr("knowledge list scan", err)
		}
		out = append(out, e)
	}
	return out, wrapErr("knowledge list iterate", rows.Err())
}

// CountByMode 统计某 mode 的条目数（迁移校验用）。
func (r *KnowledgeRepo) CountByMode(ctx context.Context, mode string) (int, error) {
	var n int
	row := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_entries WHERE mode=?`, mode)
	if err := row.Scan(&n); err != nil {
		return 0, wrapErr("knowledge count", err)
	}
	return n, nil
}

// Delete 删除条目。
func (r *KnowledgeRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM knowledge_entries WHERE id=?`, id)
		return wrapErr("knowledge delete", err)
	})
}

func scanKnowledge(rows *sql.Rows) (*KnowledgeEntry, error) {
	var e KnowledgeEntry
	var tags []byte
	var source sql.NullString
	if err := rows.Scan(&e.ID, &e.Mode, &e.Title, &e.Content, &tags, &source, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.Source = scanNullStr(source)
	if len(tags) > 0 {
		_ = json.Unmarshal(tags, &e.Tags)
	}
	return &e, nil
}

// ---------------------------------------------------------------- 公共

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
