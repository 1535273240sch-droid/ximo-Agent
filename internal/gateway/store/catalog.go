package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// ---------------------------------------------------------------- gw_models

const modelColumns = `model_id, display_name, capabilities_json, enabled, created_at, updated_at`

func scanModelSpec(sc scanner) (model.ModelSpec, error) {
	var m model.ModelSpec
	var enabled int64
	err := sc.Scan(&m.ModelID, &m.DisplayName, &m.CapabilitiesJSON, &enabled, &m.CreatedAt, &m.UpdatedAt)
	m.Enabled = enabled != 0
	return m, err
}

// UpsertModel 插入或更新模型目录条目。冲突时保留首次写入的 created_at，
// 只更新展示名、能力、启用状态与 updated_at。
func (s *Store) UpsertModel(ctx context.Context, m model.ModelSpec) error {
	if m.ModelID == "" {
		return fmt.Errorf("%w: upsert model requires model_id", ErrInvalidArgument)
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = nowMS()
	}
	if m.UpdatedAt == 0 {
		m.UpdatedAt = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_models (`+modelColumns+`) VALUES (?,?,?,?,?,?)
			 ON CONFLICT(model_id) DO UPDATE SET display_name=excluded.display_name,
			   capabilities_json=excluded.capabilities_json, enabled=excluded.enabled,
			   updated_at=excluded.updated_at`,
			m.ModelID, m.DisplayName, m.CapabilitiesJSON, boolToInt(m.Enabled), m.CreatedAt, m.UpdatedAt)
		return wrap("upsert model", err)
	})
}

// GetModel 查模型条目；不存在返回 model.ErrNotFound。
func (s *Store) GetModel(ctx context.Context, modelID string) (model.ModelSpec, error) {
	m, err := scanModelSpec(s.db.QueryRowContext(ctx,
		`SELECT `+modelColumns+` FROM gw_models WHERE model_id=?`, modelID))
	if err != nil {
		return model.ModelSpec{}, wrapNotFound("get model", err)
	}
	return m, nil
}

// ListModels 列出模型目录（按 model_id 正序）；onlyEnabled 为真时只回启用的。
func (s *Store) ListModels(ctx context.Context, onlyEnabled bool) ([]model.ModelSpec, error) {
	query := `SELECT ` + modelColumns + ` FROM gw_models`
	if onlyEnabled {
		query += ` WHERE enabled=1`
	}
	query += ` ORDER BY model_id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list models", err)
	}
	return collect(rows, scanModelSpec, "list models")
}

// ---------------------------------------------------------------- gw_providers

const providerColumns = `id, name, endpoint, protocol, status, api_key_ref,
	config_json, timeout_ms, weight, created_at, updated_at`

func scanProviderSpec(sc scanner) (model.ProviderSpec, error) {
	var p model.ProviderSpec
	err := sc.Scan(&p.ID, &p.Name, &p.Endpoint, &p.Protocol, &p.Status, &p.APIKeyRef,
		&p.ConfigJSON, &p.TimeoutMS, &p.Weight, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// UpsertProvider 插入或更新上游服务商（status 缺省为 enabled）。
func (s *Store) UpsertProvider(ctx context.Context, p model.ProviderSpec) error {
	if p.ID == "" {
		return fmt.Errorf("%w: upsert provider requires id", ErrInvalidArgument)
	}
	if p.Status == "" {
		p.Status = model.ProviderStatusEnabled
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = nowMS()
	}
	if p.UpdatedAt == 0 {
		p.UpdatedAt = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_providers (`+providerColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, endpoint=excluded.endpoint,
			   protocol=excluded.protocol, status=excluded.status, api_key_ref=excluded.api_key_ref,
			   config_json=excluded.config_json, timeout_ms=excluded.timeout_ms,
			   weight=excluded.weight, updated_at=excluded.updated_at`,
			p.ID, p.Name, p.Endpoint, p.Protocol, p.Status, p.APIKeyRef,
			p.ConfigJSON, p.TimeoutMS, p.Weight, p.CreatedAt, p.UpdatedAt)
		return wrap("upsert provider", err)
	})
}

// GetProvider 查服务商；不存在返回 model.ErrNotFound。
func (s *Store) GetProvider(ctx context.Context, id string) (model.ProviderSpec, error) {
	p, err := scanProviderSpec(s.db.QueryRowContext(ctx,
		`SELECT `+providerColumns+` FROM gw_providers WHERE id=?`, id))
	if err != nil {
		return model.ProviderSpec{}, wrapNotFound("get provider", err)
	}
	return p, nil
}

// ListProviders 列出服务商（按 id 正序）；onlyEnabled 为真时只回 status=enabled。
func (s *Store) ListProviders(ctx context.Context, onlyEnabled bool) ([]model.ProviderSpec, error) {
	query := `SELECT ` + providerColumns + ` FROM gw_providers`
	var args []any
	if onlyEnabled {
		query += ` WHERE status=?`
		args = append(args, model.ProviderStatusEnabled)
	}
	query += ` ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list providers", err)
	}
	return collect(rows, scanProviderSpec, "list providers")
}

// ---------------------------------------------------------------- gw_provider_models

const providerModelColumns = `provider_id, model_id, upstream_model_id, enabled, priority`

func scanProviderModel(sc scanner) (model.ProviderModel, error) {
	var pm model.ProviderModel
	var enabled int64
	err := sc.Scan(&pm.ProviderID, &pm.ModelID, &pm.UpstreamModelID, &enabled, &pm.Priority)
	pm.Enabled = enabled != 0
	return pm, err
}

// UpsertProviderModel 插入或更新「模型 → 上游模型名」映射（PK 为
// provider_id+model_id）。provider 与 model 必须先登记，否则外键报错。
func (s *Store) UpsertProviderModel(ctx context.Context, pm model.ProviderModel) error {
	if pm.ProviderID == "" || pm.ModelID == "" {
		return fmt.Errorf("%w: upsert provider model requires provider_id and model_id", ErrInvalidArgument)
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_provider_models (`+providerModelColumns+`) VALUES (?,?,?,?,?)
			 ON CONFLICT(provider_id, model_id) DO UPDATE SET
			   upstream_model_id=excluded.upstream_model_id, enabled=excluded.enabled,
			   priority=excluded.priority`,
			pm.ProviderID, pm.ModelID, pm.UpstreamModelID, boolToInt(pm.Enabled), pm.Priority)
		return wrap("upsert provider model", err)
	})
}

// ListProviderModels 列出某模型的全部 provider 映射，priority 数值小者优先
// （供 catalog.Candidates 排序）。
func (s *Store) ListProviderModels(ctx context.Context, modelID string) ([]model.ProviderModel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+providerModelColumns+` FROM gw_provider_models
		 WHERE model_id=? ORDER BY priority ASC, provider_id ASC`, modelID)
	if err != nil {
		return nil, wrap("list provider models", err)
	}
	return collect(rows, scanProviderModel, "list provider models")
}
