package types

import "context"

// 专家目录（v2.6.0）
//
// 背景：专家库此前是"前端硬编码 60 位 + 后端 embed 254 位"的两份数据 —— 界面只能
// 展示 60 位，用户点不到另外 194 位；自定义专家更是全链路不通（expert.CustomStore
// 只有测试假件、Dependencies.Experts 从未赋值、也没有任何 IPC 路由）。
//
// 这一组类型是专家目录的**单一形状**：端口（本文件）、IPC 帧（internal/ipcapi）
// 与前端 frontend/src/shared/types.ts 都对着它，避免三份结构体各自漂移。
//
// 契约规则与其它 DTO 一致：字段只能追加。

// ExpertCard 是专家目录里一位专家的展示与提交形状。
//
// 它刻意与 internal/expert.Expert 字段对齐（ID/Division/Name/Description/Emoji/
// Vibe/Personality/Color/Tools/Custom），只是不含 embed 数据里的内部细节：
// internal/types 是叶子包，不能 import internal/expert，因此这里定义一份等价的
// 线上形状，转换由装配层（bootstrap）负责。
type ExpertCard struct {
	ID          string `json:"id"`
	Division    string `json:"division"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Emoji 是卡片上的头像字符（内置专家来自 agents-raw.json）。
	Emoji string `json:"emoji,omitempty"`
	// Vibe 是执业风格的一句话描述，展示在专家档案里。
	Vibe string `json:"vibe,omitempty"`
	// Personality 是喂给模型的人格提示词。自定义专家大多会填它。
	Personality string `json:"personality,omitempty"`
	// Color 是卡片配色（#RRGGBB 或颜色名）。
	Color string `json:"color,omitempty"`
	// Tools 是该专家的推荐工具名。
	//
	// 这里列的是"推荐"，不保证本 build 都实现了：真正下发给子 Agent 的 schema
	// 会按工具注册表过滤（见 expert.SubAgentOptions.resolveTools）。界面应把它当作
	// 能力倾向展示，而不是可用性承诺。
	Tools []string `json:"tools,omitempty"`
	// Custom 标记这条记录来自用户（experts 表），而非内嵌目录。
	Custom bool `json:"custom,omitempty"`
}

// ExpertDirectoryPort 是专家目录的读写面。
//
// 装配层（bootstrap）用 expert.Registry 实现它：List 返回内置 + 自定义的合并视图，
// Save/Delete 只作用于自定义专家（内置专家是 embed 数据，删不掉也不该删）。
type ExpertDirectoryPort interface {
	// ListExperts 返回全部专家（内置 + 自定义）。
	ListExperts(ctx context.Context) ([]ExpertCard, error)
	// SaveExpert 新建或覆盖一位自定义专家，返回落库后的形状。
	SaveExpert(ctx context.Context, card ExpertCard) (ExpertCard, error)
	// DeleteExpert 删除一位自定义专家；返回 false 表示不存在或该 ID 属于内置专家。
	DeleteExpert(ctx context.Context, id string) (bool, error)
}
