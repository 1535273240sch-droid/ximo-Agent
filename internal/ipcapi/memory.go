package ipcapi

import (
	"context"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// MemoryService 把记忆图的读写面注册成 IPC 帧（P1-c，审核文档 4.9）。
//
// 为什么单独一个服务而不是塞进 EngineService / AdminService：
//
//   - 记忆页与 run 生命周期无关。EngineService 的每一帧都以 run 为中心
//     （提交/取消/状态/事件/恢复/授权），把跨 run 的长期资产混进去会让那层
//     契约的含义变模糊。
//   - AdminService 管的是「配置与密钥」，是写配置文件的路径。记忆是用户数据，
//     不是配置：它需要的是分页查询与逐个节点的事务性修改，混在一起会让
//     「改一个设置」与「删一条记忆」共用一条错误处理路径。
//
// 能力缺失时的行为与其它可选服务一致：明确报错，绝不静默成功——静默成功会让
// 界面显示「已遗忘」而库里那条记忆还在。
type MemoryService struct {
	graph types.MemoryGraphPort
}

// NewMemoryService 构造记忆服务。graph 可以为 nil，此时每个帧都返回明确错误。
func NewMemoryService(graph types.MemoryGraphPort) *MemoryService {
	return &MemoryService{graph: graph}
}

// Register 注册记忆类帧。
func (s *MemoryService) Register(srv *ipc.Server) {
	srv.RegisterHandler(ipc.TypeMemoryGraph, s.handleGraph)
	srv.RegisterHandler(ipc.TypeMemoryNodeGet, s.handleNodeGet)
	srv.RegisterHandler(ipc.TypeMemoryNodeUpdate, s.handleNodeUpdate)
	srv.RegisterHandler(ipc.TypeMemoryNodeDelete, s.handleNodeDelete)
	srv.RegisterHandler(ipc.TypeMemoryLink, s.handleLink)
	srv.RegisterHandler(ipc.TypeMemoryConsolidate, s.handleConsolidate)
	srv.RegisterHandler(ipc.TypeMemoryExport, s.handleExport)
	srv.RegisterHandler(ipc.TypeMemoryImport, s.handleImport)
	srv.RegisterHandler(ipc.TypeMemoryStats, s.handleStats)
	srv.RegisterHandler(ipc.TypeMemoryClear, s.handleClear)
}

// unavailable 是「本装配没有记忆能力」的统一错误。
func (s *MemoryService) unavailable(msgType string, req *ipc.Frame) (*ipc.Frame, error) {
	return ErrorFrame(msgType, req,
		fmt.Errorf("memory graph is not available in this build")), nil
}

// decodeInto 解出请求载荷；空载荷是合法的（走零值）。
func decodeInto(req *ipc.Frame, dst any) error {
	env, err := decodeEnvelope(req)
	if err != nil {
		return err
	}
	return env.Decode(dst)
}

// reply 把结果包成成功响应。
func reply(msgType string, req *ipc.Frame, v any) (*ipc.Frame, error) {
	out, err := EnvelopeFrom(msgType, v, FrameOpts{
		RequestID: req.Header.RequestID,
		SessionID: req.Header.SessionID,
	})
	if err != nil {
		return ErrorFrame(msgType, req, err), nil
	}
	return out, nil
}

// handleGraph 取一个子图（分页 / 过滤 / 邻域 / 搜索）。
func (s *MemoryService) handleGraph(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryGraph, req)
	}
	var p types.MemoryGraphRequest
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeMemoryGraph, req, err), nil
	}
	// 请求里的字段与端口契约同形，直接透传：这里不再抄一层 DTO，否则两份形状
	// 会各自漂移（前端 shared/types.ts 对着的就是这一份）。
	graph, err := s.graph.Graph(ctx, p)
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryGraph, req, err), nil
	}
	return reply(ipc.TypeMemoryGraph, req, graph)
}

// handleNodeGet 取单个节点的完整详情。
func (s *MemoryService) handleNodeGet(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryNodeGet, req)
	}
	var p MemoryIDPayload
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeMemoryNodeGet, req, err), nil
	}
	if p.NodeID == "" {
		return ErrorFrame(ipc.TypeMemoryNodeGet, req, fmt.Errorf("node_id is required")), nil
	}
	detail, err := s.graph.Node(ctx, p.NodeID)
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryNodeGet, req, err), nil
	}
	return reply(ipc.TypeMemoryNodeGet, req, detail)
}

// handleNodeUpdate 改标题 / 正文 / 重要度 / 置顶 / 状态。
//
// 载荷里的可选字段用指针的三态，因此这里必须用 types.MemoryNodeUpdate 而不是
// 一个「全字段值类型」的 DTO：用零值表示"清空"会把「用户没动这个字段」和
// 「用户想把它置空」混为一谈。
func (s *MemoryService) handleNodeUpdate(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryNodeUpdate, req)
	}
	var p types.MemoryNodeUpdate
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeMemoryNodeUpdate, req, err), nil
	}
	if p.NodeID == "" {
		return ErrorFrame(ipc.TypeMemoryNodeUpdate, req, fmt.Errorf("node_id is required")), nil
	}
	if p.Status != nil && !validMemoryStatus(*p.Status) {
		return ErrorFrame(ipc.TypeMemoryNodeUpdate, req,
			fmt.Errorf("unknown status %q", *p.Status)), nil
	}
	if p.Importance != nil && (*p.Importance < 0 || *p.Importance > 1) {
		return ErrorFrame(ipc.TypeMemoryNodeUpdate, req,
			fmt.Errorf("importance must be within [0,1], got %v", *p.Importance)), nil
	}
	detail, err := s.graph.UpdateNode(ctx, p)
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryNodeUpdate, req, err), nil
	}
	return reply(ipc.TypeMemoryNodeUpdate, req, detail)
}

// handleNodeDelete 硬删除节点及其所有边。
//
// 这是「遗忘」，不是归档：归档保留节点只让它不参与召回，而用户说「忘掉」时
// 期待的是库里真的没有它了（审核文档 4.8 对 forget 的说明）。
func (s *MemoryService) handleNodeDelete(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryNodeDelete, req)
	}
	var p MemoryIDPayload
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeMemoryNodeDelete, req, err), nil
	}
	if p.NodeID == "" {
		return ErrorFrame(ipc.TypeMemoryNodeDelete, req, fmt.Errorf("node_id is required")), nil
	}
	if err := s.graph.DeleteNode(ctx, p.NodeID); err != nil {
		return ErrorFrame(ipc.TypeMemoryNodeDelete, req, err), nil
	}
	return reply(ipc.TypeMemoryNodeDelete, req, map[string]any{
		"node_id": p.NodeID, "deleted": true,
	})
}

// handleLink 显式建立一条边。
func (s *MemoryService) handleLink(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryLink, req)
	}
	var p MemoryLinkPayload
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeMemoryLink, req, err), nil
	}
	if p.Src == "" || p.Dst == "" {
		return ErrorFrame(ipc.TypeMemoryLink, req, fmt.Errorf("src and dst are required")), nil
	}
	if p.Src == p.Dst {
		// 自环在扩散激活里没有意义，而且会让"路径解释"退化成噪声。
		return ErrorFrame(ipc.TypeMemoryLink, req,
			fmt.Errorf("a node cannot be linked to itself")), nil
	}
	if p.Rel != "" && !validMemoryRel(p.Rel) {
		return ErrorFrame(ipc.TypeMemoryLink, req, fmt.Errorf("unknown rel %q", p.Rel)), nil
	}
	mut, err := s.graph.Link(ctx, types.MemoryLink{
		Src: p.Src, Dst: p.Dst, Rel: p.Rel, Weight: p.Weight,
	})
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryLink, req, err), nil
	}
	return reply(ipc.TypeMemoryLink, req, mutationPayload(mut))
}

// handleConsolidate 手动触发一次「睡眠整理」。
func (s *MemoryService) handleConsolidate(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryConsolidate, req)
	}
	mut, err := s.graph.Consolidate(ctx)
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryConsolidate, req, err), nil
	}
	return reply(ipc.TypeMemoryConsolidate, req, mutationPayload(mut))
}

// handleExport 导出全部记忆。
func (s *MemoryService) handleExport(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryExport, req)
	}
	ex, err := s.graph.Export(ctx)
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryExport, req, err), nil
	}
	return reply(ipc.TypeMemoryExport, req, ex)
}

// handleImport 导入一份导出文件。
func (s *MemoryService) handleImport(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryImport, req)
	}
	var p types.MemoryExport
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeMemoryImport, req, err), nil
	}
	if len(p.Nodes) == 0 {
		return ErrorFrame(ipc.TypeMemoryImport, req,
			fmt.Errorf("import payload has no nodes")), nil
	}
	mut, err := s.graph.Import(ctx, p)
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryImport, req, err), nil
	}
	return reply(ipc.TypeMemoryImport, req, mutationPayload(mut))
}

// handleStats 返回计数快照与当前后端名。
func (s *MemoryService) handleStats(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryStats, req)
	}
	return reply(ipc.TypeMemoryStats, req, statsPayload(s.graph.Stats(ctx)))
}

// handleClear 清空全部记忆。
//
// 需要显式的确认字面量：这是一个不可撤销的操作（删库文件），而 IPC 载荷是可以
// 被脚本拼出来的。要求一个字面量 "DELETE_ALL" 是刻意让"人写的确认"成为必要条件。
func (s *MemoryService) handleClear(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.graph == nil {
		return s.unavailable(ipc.TypeMemoryClear, req)
	}
	var p MemoryClearPayload
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeMemoryClear, req, err), nil
	}
	if p.Confirm != memoryClearConfirm {
		return ErrorFrame(ipc.TypeMemoryClear, req,
			fmt.Errorf("refusing to clear memory without confirm=%q", memoryClearConfirm)), nil
	}
	mut, err := s.graph.Clear(ctx)
	if err != nil {
		return ErrorFrame(ipc.TypeMemoryClear, req, err), nil
	}
	return reply(ipc.TypeMemoryClear, req, mutationPayload(mut))
}

// memoryClearConfirm 是清空记忆要求的确认字面量。
const memoryClearConfirm = "DELETE_ALL"

// mutationPayload 把端口的结果类型转成线上 DTO。
//
// 这一层转换是必要的：types.MemoryGraphMutation 的 Stats 是值类型（端口契约里
// 「一定有统计」），而线上 DTO 用指针（"这次没带统计"要能和"统计全为 0"区分）。
func mutationPayload(m types.MemoryGraphMutation) MemoryGraphMutationResult {
	out := MemoryGraphMutationResult{
		OK: m.OK, Affected: m.Affected, Notes: m.Notes,
	}
	if m.Stats != nil {
		s := statsPayload(*m.Stats)
		out.Stats = &s
	}
	return out
}

// statsPayload 把端口快照转成线上 DTO。
func statsPayload(s types.MemoryStats) MemoryStatsPayload {
	return MemoryStatsPayload{
		UserID: s.UserID, Nodes: s.Nodes, Edges: s.Edges,
		RecallCalls: s.RecallCalls, RecallErrors: s.RecallErrors,
		RecallChars: s.RecallChars, BgDropped: s.BgDropped,
		Consolidations: s.Consolidations, MergedFacts: s.MergedFacts,
		ArchivedNodes: s.ArchivedNodes, TopicsCreated: s.TopicsCreated,
		LastError: s.LastError, LastErrorAt: s.LastErrorAt,
		Backend: s.Backend, Enabled: s.Enabled,
	}
}

// validMemoryStatus 报告 status 是否是 mem_nodes.status 允许的取值。
func validMemoryStatus(s string) bool {
	switch s {
	case types.MemoryStatusActive, types.MemoryStatusSuperseded, types.MemoryStatusArchived:
		return true
	default:
		return false
	}
}

// validMemoryRel 报告 rel 是否是 mem_edges.rel 允许的取值。
func validMemoryRel(rel string) bool {
	for _, r := range types.AllMemoryRels {
		if r == rel {
			return true
		}
	}
	return false
}
