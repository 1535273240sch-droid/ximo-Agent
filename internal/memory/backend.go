package memory

import (
	"context"
	"time"
)

// Backend 是长期记忆的存储后端。
//
// 两个实现：
//   - *Client —— mem0 自托管服务（HTTP）；需要额外跑一个 mem0 + Postgres + 向量模型
//   - *EmbeddedBackend —— 进程内 SQLite；零外部依赖，随引擎一起跑
//
// 接口由消费方（本包）定义，实现方只需满足它。加后端不需要改 Service 的任何逻辑。
type Backend interface {
	// Ping 做一次健康检查（供设置页「测试连接」使用）。
	Ping(ctx context.Context) error
	// Search 按查询串检索记忆。
	Search(ctx context.Context, query string, opts SearchOptions) ([]Record, error)
	// Add 写入一轮消息。
	Add(ctx context.Context, msgs []Message, opts AddOptions) ([]Record, error)
	// GetAll 列出最近若干条记忆。
	GetAll(ctx context.Context, topK int) ([]Record, error)
	// Delete 删除一条记忆。
	Delete(ctx context.Context, id string) error
	// LastError 返回最近一次失败的脱敏描述与时间。
	LastError() (string, time.Time)
}

// 后端名字。后端名出现在配置与诊断输出里，因此是稳定的字面量。
const (
	// BackendMem0 是 mem0 自托管服务（HTTP）。
	BackendMem0 = "mem0"
	// BackendEmbedded 是进程内 SQLite（本包自带，零外部依赖）。
	BackendEmbedded = "embedded"
)

var _ Backend = (*Client)(nil)
