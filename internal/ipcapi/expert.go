package ipcapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ExpertService 把专家目录的读写面注册成 IPC 帧（v2.6.0）。
//
// 为什么需要它：专家库此前有两份互不相干的数据 —— 前端硬编码 60 位、后端 embed
// 254 位。界面只能展示那 60 位，用户点不到其余专家，而"自定义专家"在整个 v2 里
// 没有落库的地方也没有路由（CustomStore 只有测试假件）。这一层帧把目录变成单一
// 真相：界面从后端取列表，保存/删除也走同一条路。
//
// 与 MemoryService 同样的取舍：单独一个服务而不是塞进 EngineService（后者每帧都以
// run 为中心），能力缺失时明确报错而不是静默成功（静默成功会让界面显示"已保存"
// 而库里什么都没有）。
type ExpertService struct {
	dir types.ExpertDirectoryPort
}

// NewExpertService 构造专家目录服务。dir 可以为 nil，此时每个帧都返回明确错误。
func NewExpertService(dir types.ExpertDirectoryPort) *ExpertService {
	return &ExpertService{dir: dir}
}

// Register 注册专家目录帧。
func (s *ExpertService) Register(srv *ipc.Server) {
	srv.RegisterHandler(ipc.TypeExpertList, s.handleList)
	srv.RegisterHandler(ipc.TypeExpertSave, s.handleSave)
	srv.RegisterHandler(ipc.TypeExpertDelete, s.handleDelete)
}

func (s *ExpertService) unavailable(msgType string, req *ipc.Frame) (*ipc.Frame, error) {
	return ErrorFrame(msgType, req,
		fmt.Errorf("expert directory is not available in this build")), nil
}

// ExpertListResult 是 TypeExpertList 的响应体。
//
// 用对象包一层而不是直接回数组：列表接口以后很可能要带分页、总数或"数据来源"
// 之类的元信息，而把数组换成对象是**破坏性**的 DTO 变更（契约只允许追加字段）。
type ExpertListResult struct {
	Experts []types.ExpertCard `json:"experts"`
	// Total 是本次返回的条数（等于 len(Experts)）。留着它是为了让界面不必去猜
	// "空列表"与"没取到"的区别。
	Total int `json:"total"`
	// Divisions 是目录里出现的部门名（去重、有序），界面据此渲染筛选条 ——
	// 免得前端再抄一份部门清单。
	Divisions []string `json:"divisions"`
}

// handleList 返回完整专家目录。
func (s *ExpertService) handleList(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.dir == nil {
		return s.unavailable(ipc.TypeExpertList, req)
	}
	list, err := s.dir.ListExperts(ctx)
	if err != nil {
		return ErrorFrame(ipc.TypeExpertList, req, err), nil
	}
	if list == nil {
		// 空目录也必须是 []，不能是 null：前端会直接 .map，null 会炸。
		list = []types.ExpertCard{}
	}
	seen := make(map[string]bool, len(list))
	divisions := make([]string, 0, 16)
	for _, e := range list {
		if e.Division == "" || seen[e.Division] {
			continue
		}
		seen[e.Division] = true
		divisions = append(divisions, e.Division)
	}
	return reply(ipc.TypeExpertList, req, ExpertListResult{
		Experts:   list,
		Total:     len(list),
		Divisions: divisions,
	})
}

// handleSave 新建或覆盖一位自定义专家。
//
// 校验放在这里而不是存储层：存储层只负责"存下来"，而"什么算合法的专家"是契约
// 问题。内置专家（非 Custom）不允许被覆盖 —— 那不是"保存失败"，而是用户不该走上
// 来的路（界面只允许编辑自定义项），所以给一个说清楚原因的错误。
func (s *ExpertService) handleSave(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.dir == nil {
		return s.unavailable(ipc.TypeExpertSave, req)
	}
	var card types.ExpertCard
	if err := decodeInto(req, &card); err != nil {
		return ErrorFrame(ipc.TypeExpertSave, req, err), nil
	}
	card.ID = strings.TrimSpace(card.ID)
	card.Name = strings.TrimSpace(card.Name)
	card.Division = strings.TrimSpace(card.Division)
	if card.Name == "" {
		return ErrorFrame(ipc.TypeExpertSave, req,
			fmt.Errorf("专家名称不能为空")), nil
	}
	if card.Division == "" {
		return ErrorFrame(ipc.TypeExpertSave, req,
			fmt.Errorf("专家部门不能为空（用于工具推荐与界面分组）")), nil
	}
	if len(card.Name) > 60 {
		return ErrorFrame(ipc.TypeExpertSave, req,
			fmt.Errorf("专家名称最长 60 个字符")), nil
	}
	// 强制 Custom：这条路径只写自定义专家。不加这一句，客户端可以把 Custom=false
	// 发上来，注册表就会把它当内置专家处理（内置不可删，且会覆盖同名内置项）。
	card.Custom = true

	saved, err := s.dir.SaveExpert(ctx, card)
	if err != nil {
		return ErrorFrame(ipc.TypeExpertSave, req, err), nil
	}
	return reply(ipc.TypeExpertSave, req, saved)
}

// ExpertDeleteResult 是 TypeExpertDelete 的响应体。
type ExpertDeleteResult struct {
	// Deleted 为 false 表示目标不存在或属于内置专家（内置专家删不掉）。
	Deleted bool `json:"deleted"`
}

// handleDelete 删除一位自定义专家。
func (s *ExpertService) handleDelete(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.dir == nil {
		return s.unavailable(ipc.TypeExpertDelete, req)
	}
	var p ExpertIDPayload
	if err := decodeInto(req, &p); err != nil {
		return ErrorFrame(ipc.TypeExpertDelete, req, err), nil
	}
	id := strings.TrimSpace(p.ExpertID)
	if id == "" {
		return ErrorFrame(ipc.TypeExpertDelete, req,
			fmt.Errorf("缺少 expert_id")), nil
	}
	ok, err := s.dir.DeleteExpert(ctx, id)
	if err != nil {
		return ErrorFrame(ipc.TypeExpertDelete, req, err), nil
	}
	return reply(ipc.TypeExpertDelete, req, ExpertDeleteResult{Deleted: ok})
}
