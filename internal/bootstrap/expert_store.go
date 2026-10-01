package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/engine"
	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/repository"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ---------------------------------------------------------------------------
// 专家目录：自定义专家的持久化 + 目录读写面
//
// 这一层此前**完全缺失**：expert.CustomStore 只有测试假件，Dependencies.Experts
// 从未被赋值（引擎于是每次 NewRegistry(nil)），也没有任何 IPC 路由能创建自定义专家。
// 结果是"自定义专家"这个功能在整个 v2 里只是一个接口签名。
// ---------------------------------------------------------------------------

// expertRepos 构造仓库集合；store 为 nil（部分测试只装配局部依赖）时返回 nil，
// 由调用方走"没有存储层"的降级分支。
func expertRepos(store *storage.Store) *repository.Repos {
	if store == nil {
		return nil
	}
	return repository.New(store)
}

// runsOf 从仓库集合里取 RunRepo；repos 为 nil 时返回 nil（适配器会退化成 ok=false）。
func runsOf(repos *repository.Repos) *repository.RunRepo {
	if repos == nil {
		return nil
	}
	return repos.Runs
}

// buildExpertRegistry 组装专家目录：内置专家（embed）+ 自定义专家（experts 表）。
//
// repos 为 nil（某些测试只装配部分依赖）时退化成"纯内置"，与改动前的行为一致。
func buildExpertRegistry(repos *repository.Repos) *expert.Registry {
	if repos == nil {
		return expert.NewRegistry(nil)
	}
	return expert.NewRegistry(expertStore{repo: repos.Experts})
}

// runRequestCatalog 从 runs 表回填崩溃恢复所需的原始 prompt/model（审核文档 V1）。
//
// runs 表在提交时就写入了 prompt 与 model（repository.RunRepo.Create），但恢复路径
// 此前只用事件日志重建请求 —— 日志里没有这两个字段，于是恢复出来的 run 用占位
// prompt + provider 默认模型继续执行。这里把它读回来，接上 engine.Dependencies
// 的 RunRequestCatalog。
type runRequestCatalog struct {
	runs *repository.RunRepo
}

var _ engine.RunRequestCatalog = runRequestCatalog{}

func (c runRequestCatalog) RunRequest(ctx context.Context, runID string) (string, string, bool) {
	if c.runs == nil {
		return "", "", false
	}
	run, err := c.runs.Get(ctx, runID)
	if err != nil || run == nil {
		// 查不到不是错误：老库、被清理过的库、或本装配没有存储层时，恢复流程
		// 退回占位 prompt 的旧行为即可。
		return "", "", false
	}
	return run.Prompt, run.Model, true
}

// expertStore 把 experts 表适配成 expert.CustomStore。
//
// 字段映射：experts 表是 v1 的形状（name/description/prompt/model/tools），
// 0004 迁移补上了 v2 需要的 division/emoji/vibe/color/personality。
// 其中 personality 与 prompt 是同一个东西的两种叫法（v1 叫 prompt），写入时两边
// 都填，读取时优先取 personality —— 这样 v1 时代留下的行读出来也不会丢掉人格。
type expertStore struct {
	repo *repository.ExpertRepo
}

var _ expert.CustomStore = expertStore{}

// Load 返回全部自定义专家。
//
// 跳过 Enabled=false 的行：专家表自带启用开关（v1 遗留），用户显式停用的专家
// 不应该出现在目录里。内置专家没有这个开关（它们来自 embed 数据）。
func (s expertStore) Load() ([]expert.Expert, error) {
	if s.repo == nil {
		return nil, nil
	}
	rows, err := s.repo.List(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]expert.Expert, 0, len(rows))
	for _, r := range rows {
		if r == nil || !r.Enabled {
			continue
		}
		out = append(out, expert.Expert{
			ID:          r.ID,
			Division:    r.Division,
			Name:        r.Name,
			Description: r.Description,
			Emoji:       r.Emoji,
			Vibe:        r.Vibe,
			Personality: firstNonEmptyStr(r.Personality, r.Prompt),
			Tools:       append([]string(nil), r.Tools...),
			Color:       r.Color,
			Custom:      true,
		})
	}
	return out, nil
}

// Save 写入（同 ID 覆盖）。
func (s expertStore) Save(e expert.Expert) error {
	if s.repo == nil {
		return fmt.Errorf("bootstrap: 自定义专家存储不可用")
	}
	row := &repository.Expert{
		ID:          e.ID,
		Name:        e.Name,
		Description: e.Description,
		Prompt:      e.Personality,
		Tools:       append([]string(nil), e.Tools...),
		Enabled:     true,
		Source:      "custom",
		Division:    e.Division,
		Emoji:       e.Emoji,
		Vibe:        e.Vibe,
		Color:       e.Color,
		Personality: e.Personality,
	}
	return s.repo.Upsert(context.Background(), row)
}

// Delete 删除自定义专家。
//
// 返回 false 表示库里没有这一行。**内置专家不会被删**：注册表的 DeleteCustom 只
// 走这条路，而内置专家从来不进 experts 表，所以这里天然只可能删到自定义项。
func (s expertStore) Delete(id string) (bool, error) {
	if s.repo == nil {
		return false, fmt.Errorf("bootstrap: 自定义专家存储不可用")
	}
	rows, err := s.repo.List(context.Background())
	if err != nil {
		return false, err
	}
	found := false
	for _, r := range rows {
		if r != nil && r.ID == id {
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	if err := s.repo.Delete(context.Background(), id); err != nil {
		return false, err
	}
	return true, nil
}

// expertDirectory 把 *expert.Registry 适配成 types.ExpertDirectoryPort。
//
// List 走注册表（内置 + 自定义的合并视图），Save/Delete 走注册表的自定义入口 ——
// 后者会顺带失效索引缓存（见 expert.Registry.SaveCustom），所以界面保存后立刻再取
// 列表就能看到新专家，不需要重启。
type expertDirectory struct {
	reg *expert.Registry
}

var _ types.ExpertDirectoryPort = expertDirectory{}

func (d expertDirectory) ListExperts(_ context.Context) ([]types.ExpertCard, error) {
	if d.reg == nil {
		return nil, fmt.Errorf("bootstrap: 专家目录不可用")
	}
	list, err := d.reg.Load()
	if err != nil {
		return nil, err
	}
	out := make([]types.ExpertCard, 0, len(list))
	for _, e := range list {
		out = append(out, types.ExpertCard{
			ID:          e.ID,
			Division:    e.Division,
			Name:        e.Name,
			Description: e.Description,
			Emoji:       e.Emoji,
			Vibe:        e.Vibe,
			Personality: e.Personality,
			Color:       e.Color,
			Tools:       recommendedTools(e),
			Custom:      e.Custom,
		})
	}
	return out, nil
}

// recommendedTools 返回要展示给界面的推荐工具名。
//
// 内置目录（agents-raw.json）里每位专家的 tools 字段都是空的：v2 的推荐工具是
// AnalyzeExpert 按「部门表 + 关键词规则」现算出来的，不是数据里带的。界面要显示
// 工具标签，就必须走同一个函数 —— 否则卡片上会是空的，而专家实际拿着 3~7 个工具。
//
// 注意这是**推荐**而不是可用性承诺：真正下发给子 Agent 的 schema 会再按工具注册表
// 过滤（expert.SubAgentOptions.resolveTools）。
func recommendedTools(e expert.Expert) []string {
	if len(e.Tools) > 0 {
		return append([]string(nil), e.Tools...)
	}
	return append([]string(nil), expert.AnalyzeExpert(e).Tools...)
}

func (d expertDirectory) SaveExpert(_ context.Context, card types.ExpertCard) (types.ExpertCard, error) {
	if d.reg == nil {
		return types.ExpertCard{}, fmt.Errorf("bootstrap: 专家目录不可用")
	}
	card.ID = strings.TrimSpace(card.ID)
	if card.ID == "" {
		// 没有 ID 时生成一个稳定的 slug：自定义专家的 ID 会出现在 export/import、
		// run 提交与后续的编辑请求里，随机 ID 会让"再次编辑"变得困难。
		card.ID = "custom-" + slugify(card.Name)
	}
	// 同 ID 已存在时只允许覆盖自定义专家。内置专家是 embed 数据，覆盖它会让内置
	// 目录在该进程内被用户数据顶掉（下次启动又会变回去），宁可明确报错。
	if existing, ok := d.reg.Get(card.ID); ok && !existing.Custom {
		return types.ExpertCard{}, fmt.Errorf(
			"专家 ID %q 已属于内置专家，不能覆盖；请换一个 ID 或名称", card.ID)
	}
	card.Custom = true
	if err := d.reg.SaveCustom(expert.Expert{
		ID:          card.ID,
		Division:    card.Division,
		Name:        card.Name,
		Description: card.Description,
		Emoji:       card.Emoji,
		Vibe:        card.Vibe,
		Personality: card.Personality,
		Color:       card.Color,
		Tools:       append([]string(nil), card.Tools...),
	}); err != nil {
		return types.ExpertCard{}, err
	}
	return card, nil
}

func (d expertDirectory) DeleteExpert(_ context.Context, id string) (bool, error) {
	if d.reg == nil {
		return false, fmt.Errorf("bootstrap: 专家目录不可用")
	}
	existing, ok := d.reg.Get(id)
	if !ok {
		return false, nil
	}
	if !existing.Custom {
		// 内置专家删不掉：它们来自 embed 数据，删除只会让界面下次刷新时"复活"，
		// 不如明确告诉用户这是内置项。
		return false, fmt.Errorf("专家 %q 是内置专家，不能删除", id)
	}
	return d.reg.DeleteCustom(id)
}

// ExpertDirectory 返回专家目录的读写面（types.ExpertDirectoryPort）。
//
// 与 MemoryGraph 同样的形态：装配缺失时返回 nil，ipcapi 据此让每个 expert 帧返回
// 明确错误，而不是"成功但什么都没做"。
func (a *App) ExpertDirectory() types.ExpertDirectoryPort {
	if a == nil || a.expertRegistry == nil {
		return nil
	}
	return expertDirectory{reg: a.expertRegistry}
}

// firstNonEmptyStr 返回第一个非空字符串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// slugify 把专家名转成可用作 ID 的 ASCII slug。
//
// 中文名不会被音译（那需要一个词典），此时退化成逐字符的十六进制编码 —— 关键是
// **确定性**：同一个名字每次都得到同一个 ID，用户再次保存同一份草稿是更新而不是
// 新增一条重复记录。
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '/':
			b.WriteByte('-')
		default:
			fmt.Fprintf(&b, "%x", r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "expert"
	}
	return out
}
