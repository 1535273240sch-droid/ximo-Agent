# XimoAgent v2.6.0 更新说明

发布日期：2026-10-01 ｜ 版本跨度：v2.5.1 → v2.6.0

## 一、本次更新概述

v2.5.x 修完了「专家到底有没有真的干活」这条线上的假完成与谎报能力。v2.6.0 把排查
出来的**全部遗留问题一次修完**，判断标准只有一条：**代码、文案与真实能力三者一致**。

四句话概括：

1. **专家的工具是真的**：补齐 `file_edit` / `multi_edit` / `todo_write`，推荐表与工作流
   不再出现任何未实现的工具名（有测试强制）。
2. **专家库是真的**：254 位内置专家全部可见可选（此前界面只硬编码了 60 位），并且
   可以新建、编辑、删除**自定义专家**（此前只有接口签名，没有落库、没有路由）。
3. **进度是真的**：子代理的每一步（开始 / 调用工具 / 工具返回 / 结束）作为 `expert.work`
   事件进入 Work Log，用户在回答上方就能看到专家做了什么。
4. **恢复是真的**：崩溃后「继续」不再丢用户当初的 prompt 与模型（审核文档 V1）。

---

## 二、新增能力

### 1. 精确编辑工具：`file_edit` / `multi_edit`

此前文件域只有 `file_read/list/search/write/delete` —— Agent 改一个字符也得整文件重写，
而部门推荐表里却写着 `file_edit`。

- `file_edit`：精确字符串替换；未指定 `replace_all` 时要求**唯一匹配**（0 处报「未找到」，
  多处报「找到 N 处，请提供更长的上下文或使用 replace_all」）；保留 CRLF 与「文件末尾
  没有换行」的原状；不改动文件编码；拒绝目录 / 不存在 / 二进制 / 超 8MB。
- `multi_edit`：一次提交多处改动，**先对原文校验全部编辑**（唯一性 + 区间不重叠），任何
  一条不合法就一个字节都不写 —— 部分应用会让文件停在一个既非改前也非改后的状态。
- 两者都走与 `file_write` 相同的两个 Guard 检查（写权限 + 敏感文件），并声明
  `SideEffect`、`ClassDetectable`，与权限/幂等表一致。

### 2. 待办工具：`todo_write`（F1 闭环复活）

F1「待办全部完成就收尾」与 closure 的 `todos_done` 检查都以这个工具为前提，而它在 v2
里**从未注册**（只有推荐表、权限表、幂等分类表和 loop 的解析代码在引用它）—— 于是那两条
闭环在生产里永远不触发（`TodoSeen` 恒为 false）。现在：

- 按 run 隔离的待办清单（进程内，FIFO 上限 128 个 run），支持 `write` / `update` / `list`。
- 结果 Metadata 带 `{total, done}`（与 `agent/loop.go` 的 `todoSnapshot` 同契约），
  全部完成时引擎进入收尾轮。
- 没有 run 上下文时明确拒绝，而不是让所有无上下文调用共享一份清单。

### 3. 专家目录：三个 IPC 帧 + 自定义专家持久化

- 新增迁移 `0004_expert_catalog.sql`：给 `experts` 表补上 v2 需要的
  `division / emoji / vibe / color / personality` 列（纯追加，既有行取默认值）。
- `expert.CustomStore` 终于有了生产实现（`experts` 表），`Dependencies.Experts` 不再
  永远是 nil：引擎的专家执行路径与 IPC 的专家目录用**同一个注册表实例**。
- 三个新帧：`system.expert.list` / `system.expert.save` / `system.expert.delete`
  （载荷形状定义在 `internal/types/t02_expert_catalog.go`，前后端共用一份）。
- 内置专家不可覆盖、不可删除 —— 明确报错，而不是静默把 embed 数据顶掉。

### 4. 专家库页面改用后端目录（含自定义专家的增删改）

- 页面数据源换成 `system.expert.list`：**254 位内置 + 你自己的**，页头显示真实计数，
  筛选条计数 = 点进去能看到的卡片数。
- 新增自定义专家表单（名称/部门/描述/emoji/风格/人格/配色/工具）与编辑、删除（内联二次
  确认）。保存后立刻重新拉取目录，以服务端为准。
- 输入框旁的「专家快选」也改用同一份目录，所以从快选里也能选到任意一位专家。
- 删除渲染层里硬编码的 60 位样本（`SAMPLED` 等）：那是"界面只有 60 位"的根源。

### 5. `expert.work`：子代理步骤进入 Work Log

- 新事件类型 `expert.work`（持久事件），载荷
  `{expertId, expertName, stage, detail, toolArgs, result, timestamp}`，文本一律经
  `types.RedactString`。
- 单专家直连与 Agent 集群都接线；事件在状态下发，用户可以实时看到专家在做什么。
- 前端 Work Log 新增 `expert` 步骤（标题是专家名，结果是可展开预览），同一
  (专家, 阶段) 重复投递按稳定 id 幂等 upsert。

---

## 三、修复

### 1. 推荐表与工作流不再引用未实现的工具（有测试强制）

v1 的能力清单里有 20 多个名字在 v2 没有实现（`code_execute` / `code_lint` /
`code_format` / `git_operations` / `web_search` / `web_research` / `web_cache` /
`ui_generate` / `design_*` / `project_index` / `project_context` / `dependency_check` /
`network_capture` / `network_replay` / `api_extract`…）。它们此前出现在部门推荐表、
关键词规则与**工作流文案**里（工作流是直接写进提示词的操作指南），模型会照着去调用
一个必然失败的工具，或者干脆在回答里谎称自己用过了。

- 17 个部门的推荐表、12 条关键词规则、17 段工作流与默认工具集全部改写为**只列真实工具**，
  并保留「v1 名字 → v2 真实能力」的映射（`code_execute → terminal_exec`、
  `git_operations → git_status/git_diff/git_log/git_branch`、
  `web_search/web_research → web_fetch + browser_navigate/browser_get_content` 等）。
- 新增测试强制这条不变量：推荐表/规则/工作流里出现的每个工具名，要么本 build 实现了，
  要么必须显式登记（当前登记表为空）；默认配置注册表清单另有一份快照测试。
- 顺带把 `knowledge` / `memory` / `git_*` 这些"实现了却从不推荐"的工具补进相关部门的
  推荐表（此前专家永远拿不到它们）。

### 2. 崩溃恢复不再丢 prompt 与模型（审核文档 V1）

`prompt` 与 `model` 不在事件日志里（日志只存边界），而恢复路径此前只用日志重建请求 ——
于是用户点「继续」后，模型收到的是占位文本 `(recovered run: original prompt
unavailable in this process)`，模型名也回落到配置默认值。

- 新增可选端口 `engine.RunRequestCatalog`：从 `runs` 表（提交时就写了这两列）把原始
  prompt/model 读回来；查不到时保持旧的占位行为，恢复流程不受影响。
- 顺带修好「恢复备注丢在副本里」：`applyRecoveryPlan` / `rehydrateRun` 改为传指针，
  Notes 现在真的能到达恢复报告（例如「recovered the original model from the run record」）。

### 3. office 路径判定：8.3 短名导致的假「路径不在工作区」

`office` 的 `resolvePath` 对整条路径调 `filepath.EvalSymlinks`，而它在 Windows 上**只对
已存在的路径**才把 8.3 短名展开成长名。`create`/`add` 的目标文件通常还不存在，于是
「已归一化的根」与「未归一的用户路径」被直接比较：

    C:\Users\ADMINI~1\...\x.docx  vs  C:\Users\Administrator\...

结果是一个**合法的写入被拒绝**（"路径不在 allowed_roots 内"）。修法：只对**最长已存在
前缀**做 `EvalSymlinks` 再把未创建的尾部拼回去；区分「确实还没创建」与「悬空符号链接」
（后者 fail-closed）；包含性判断改用 `filepath.Rel` + 拒绝 `..`/绝对路径，仍然拒绝
`C:\ws-evil` 这类同前缀绕过。新增测试覆盖 `..` 逃逸、符号链接逃逸（含悬空链接）、
等价拼写、空 roots 不放宽策略，以及 Windows 专用的短名=长名回归。

### 4. 「已完成但没有回答」的偶发窗口

`finishRun` 必须先做状态跃迁（产生 `run.state_changed`）、再 `CloseRun`（产生
`run.completed` 并把答案写进 actor）—— 顺序不能反，否则会在终态事件之后继续写事件
（事件流在终态关闭）。两步之间必然存在一个瞬间：actor 说「已 completed」、答案是空的。
`GetRun` 是状态轮询与 IPC 状态帧的来源，谁在这个瞬间读到它，就会看到一条没有回复的完成
记录（全量并行测试时曾偶发触发）。修法：读取方在「终态 + 空答案」时回到日志取权威答案
（`final_answer` 事件先于状态跃迁落库，一定读得到），错误字段同理。

### 5. 迁移版本号不再写死

gateway 的迁移测试原本断言 `schema version == 3`，加上 0004 后失效。改为**从迁移目录
算出最高版本**再断言"迁移全部应用完"：以后再新增迁移不需要改这条测试。

---

## 四、测试与验证

```
go build ./...                 → 通过
go vet ./...                   → 通过
go test -short -count=1 ./...  → 58/58 个包全部通过（exit 0）
                                 含此前长期失败的 internal/worker/office
                                 与曾偶发失败的 internal/bootstrap 装配测试
tsc（web + node 两个工程）      → 无错误
vitest run                     → 110/110 通过（8 个文件）
electron-vite build            → 成功
gofmt（本轮改动的 23 个 Go 文件，LF 归一）→ 干净
```

本轮新增/强化的测试：

| 范围 | 测试 |
| --- | --- |
| 文件编辑 | `edit_test.go`：唯一匹配 / CRLF 保留 / 末尾无换行 / 0 处 / 多处 / replace_all / 删除 / 越权路径 / `multi_edit` 原子性与区间重叠（21 个用例） |
| 待办 | `todo_test.go`：`{total, done}` 契约 / 按 run 隔离 / 参数校验 / 超限 |
| 工具与推荐表一致性 | `expert_tools_test.go`：默认注册表快照、推荐表与工作流的工具名必须真实存在、每部门至少剩 1 个真工具 |
| 专家提示词 | `prompt_tools_test.go`：提示词只列真实可用工具、与 schema 同集合、无工具时明确声明并禁止虚构 |
| 自定义专家 | `expert_catalog_test.go`：内置目录可见 → 保存 → **重启后仍在** → 删除 → 内置不可覆盖/删除 |
| 专家目录 IPC | `expert_test.go`：三个帧的转发与校验、空目录编码为 `[]`、`custom` 强制、线名契约 |
| 端到端 | `tests/e2e/expert_catalog_e2e_test.go`：真实 IPC 往返 + 真实 SQLite + 重启后仍存在 + 内置保护 |
| 子代理步骤 | `expert_work_test.go`：事件恰好一次且在终态之前、取消后不再写（这条守卫一旦去掉会让 `WaitRun` 永久挂住） |
| 恢复回填 | `recovery_catalog_test.go`：回填原始 prompt/model、查不到/空值时退回占位、未装配时行为不变 |
| 终态空答案 | `getrun_answer_test.go`：中间窗口用日志补答案、运行中的 run 不受影响 |
| 前端 | `experts-store.test.ts`（16）、`experts-data.test.ts`（9）、`steps.test.ts`（新增 expert 步骤 3 例） |

---

## 五、已知边界（明确不做，而不是悄悄留着）

1. **`agent_expert` 工具仍未注册**。v2 的设计是「专家由用户明确选择后由引擎执行」，
   而不是「让主模型自己决定去调用专家」。现在 254 位专家都能在界面上选到（含快选），
   这条路径已经完整；真要把 `agent_expert` 注册成工具是一次功能扩展（新工具 + 子代理
   事件回灌 + 权限判定），不属于修 bug。
2. **v1 里那些 v2 没有实现的能力不做**（设计生成、独立搜索 API、lint/format、项目索引、
   依赖检查、网络抓包/回放等）。本轮把它们从推荐表与工作流里移除并在代码注释里写明
   映射关系 —— 宁可不承诺，也不谎报。
3. **自定义专家的 `tools` 字段不做白名单校验**：运行时按工具注册表过滤（未实现的会被
   过滤掉），所以用户填了不存在的工具名不会让 run 失败，只会少一个工具。
4. **待办清单只在进程内、按 run 隔离**：跨进程恢复后旧待办不重放（重放一份过期的分解
   反而会让模型以为任务还在中途）。F1 的判据由 run 内的闭环跟踪器持有。

---

## 六、升级说明

- **有数据库迁移**：`0004_expert_catalog.sql`（给 `experts` 表追加 5 列，纯追加，
  既有数据取默认值，不需要人工干预）。启动时自动应用。
- 无配置变更。
- 行为变化：
  1. 专家库页展示**全部 254 位**专家（此前 60 位），并可管理自定义专家；
  2. 专家可用的工具变多（`file_edit` / `multi_edit` / `todo_write` / `git_*` 等按部门
     推荐，并会被真实注册表过滤），提示词与 function schema 保持一致；
  3. 专家 run 的 Work Log 会出现「专家」步骤；
  4. 崩溃后「继续」会用当初的 prompt 与模型。

```powershell
ximo-agent.exe --version              REM 应打印 v2.6.0
```
