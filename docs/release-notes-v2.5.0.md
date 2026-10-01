# XimoAgent v2.5.0 更新说明

发布日期：2026-10-01 ｜ 版本跨度：v2.4.0 → v2.5.0

## 一、本次更新概述

v2.4.0 之后做了一次**专家 / 子 Agent 子系统的专项排查**（用户手选专家、Agent 集群、
专家库页面三条路径），发现 18 处问题，本轮修掉其中会影响正确性与可观测性的部分。

一句话概括本轮：**让「专家到底有没有真的干活」这件事在每一层都说得清楚**。

四类改动：

1. **工具真的存在才给模型**——专家的推荐工具集来自部门表与关键词，其中含大量本
   build 未注册的名字；此前它们被原样塞进 function schema，模型选中后必然报
   「工具未注册」，白烧一轮往返。现在按运行时注册表过滤，并且参数 schema 用工具的
   **真实定义**，不再是无参数的占位 `{"type":"object","properties":{}}`。
2. **消灭「假完成」**——专家子 Agent 失败时会降级成「专家信息 + 手动指引」的文案，
   而这段文案非空：
   - 专家直连：此前照样以 `completed` 收尾 → 现在报 `failed`；
   - Agent 集群：此前统计口径是「Content 非空即成功」，8 位专家全挂也会写出
     「8 位给出了产出」→ 现在只有**真的跑过子代理**才计入产出，一位都没跑就报 `failed`。
3. **专家路径补上收尾判据（run.closure）**——此前只有主 Agent 循环发这个事件，于是
   专家/集群 run 的 Work Log 永远没有结论、也没有「继续」按钮的失败项可引用。
4. **用户手选的模型不再被静默忽略**——专家/集群路径此前会丢掉 `SubmitPayload.model`
   （有模型池时被池选路覆盖，没有池时回退配置默认模型）。

---

## 二、修复清单

### 1. 工具可用性与参数 schema（`internal/expert`、`internal/tool`、`internal/bootstrap`、`internal/engine`）

- `tool.ToolRuntime` 新增 `Has(name)` 与 `Describe(name) (description, parameters, ok)`：
  判定依据是**运行时自己的注册表**，不另抄一份名单（两份名单一定会漂移）。
- `bootstrap.toolRuntimeAdapter` 通过可选接口探测把这两个能力透传出去，不扩张冻结的
  `ports.ToolRuntime` 契约（避免波及所有实现方与假件）。
- `engine.expertToolExecutor` 实现 `expert.ToolDescriber`，`expert.SubAgentOptions.resolveTools`
  重写为：先按「是否真的注册」过滤，再取真实描述与参数 schema；执行器没有导出能力时
  退回占位 schema，但**不会**把未注册的工具发给模型。
- 效果：部门表里那些本 build 未实现的工具名（`ui_generate` / `code_execute` /
  `browser_navigate` 等）不再进入子 Agent 的 schema。

### 2. Agent 集群的产出统计与终态（`internal/engine/cluster_run.go`）

- 统计口径改为「子代理真的跑过」（`SubAgentMode == true` 且无错误），并区分
  **降级指引**与**执行报错**：收尾句满足
  `真正执行 + 降级 + 报错 == 派出人数`，不再漏掉报错的那部分。
- 一位专家都没真正执行 → 终态 `failed`（此前无论结果如何都是 `completed`）。
- `final_answer` 载荷新增 `executedExperts` / `degradedExperts`，前端不必去数正文里的警告行。

### 3. 专家直连不再把降级当成功（`internal/engine/expert_run.go`）

- `outcome.SubAgentMode == false`（子 Agent 未真正执行）→ 终态 `failed`，错误信息为
  真实原因；降级文案仍然作为 `final_answer` 发出（失败不等于什么都不给），并伴随
  `degradedToInfo: true` 的错误事件。

### 4. 专家/集群路径发出 `run.closure`（`internal/engine`、`internal/types`）

- 新增 `Engine.emitRunClosure`：在终态跃迁之前发出一条 `run.closure`，载荷与主循环同构
  （`verdict` / `checks` / `incomplete` / `reason`），额外带 `expertPath` 与 `degraded`。
- `types.ClosureInput` 新增 `Degraded`，并新增检查项
  `experts_executed`（标签「专家全部真正执行」）：有专家降级 → `partial`；一位都没跑
  （答案为空）→ `failed`。该检查项**追加在最后**，主循环的六项检查顺序不变。
- 两条硬约束（都是踩过的坑，已写进代码注释）：
  1. **终态之后不再写事件**。用户在子 Agent 半途取消时 `Cancel` 会先落库
     `run.cancelled`；此时再追加 closure，日志重建取「最新事件携带的状态」会折叠回
     非终态，`WaitRun` 会一直等下去（开发过程中真的这样挂住过）。
  2. closure 事件的 `State` 取 run 当时的真实状态，不写死 `thinking`。

### 5. 手选模型在专家/集群路径生效（`internal/engine/expert_run.go`）

- `newExpertOrchestrator(model)`：`SubmitPayload.model` 非空时直接作为子 Agent 的模型，
  并且**不挂模型池**——池选路会用自己的候选覆盖 `Model`，那正是「输入框旁选了模型却
  跑出另一个模型」的根因。这与主 Agent 路径一致（主循环同样只把 `Model` 交给 provider）。
- 代价（有意为之）：这一次执行失去「限流/连接类失败换下一个候选」的能力。自动选路的
  补偿机制不该反过来覆盖用户的显式决定。

### 6. 计划阶段的事件标注（`internal/expert/orchestrator.go`）

- `planPhase` 现在带上 `runner.ExpertID/ExpertName`。此前规划阶段没有身份，`RunSubAgent`
  会退回「从系统提示词反解身份」，而规划阶段的提示词是通用的
  `e2ePlanInstruction()`，反解结果必然是 `unknown-expert`。

### 7. 专家注册表的缓存真的生效（`internal/expert/registry.go`）

- `Load()` 此前**无条件重建** 254 条索引，于是 `SaveCustom`/`DeleteCustom` 里那句
  「使缓存失效」是死代码（缓存从未生效，失效自然无意义），而 `Get`/`Search`/`Count`
  每次都要付一次全量重建。
- 现在 `Load()` 命中缓存直接返回副本，自定义读取与重建在写锁内完成（避免「读旧列表」
  与并发 `SaveCustom` 失效交错时把新数据覆盖掉）。

### 8. 界面不再虚报数量（`frontend/.../experts`）

- 专家库页此前写「内置 254 位…点击可直接激活对话」，而列表只有 60 张卡片；筛选条上的
  「工程研发 (58)」点开只有 6 张卡。现在：
  - 页头文案区分「本页精选 60 位」与「专家库共 254 位（其余由 Agent 集群模式自动选用）」；
  - 筛选条计数改用**本页实际条数**（`SAMPLE_DIVISION_COUNTS`，由 `EXPERTS` 派生）。
- 「以该专家身份开启会话」现在真的带 `expert_id` 提交。此前只发了一段专家口吻的
  `system_prompt`（后端专家路径根本不读 `SubmitRequest.SystemPrompt`），于是专家库页面
  走的是通用主循环 —— 规划阶段、子 Agent、工具链、模型池一个都不会启动。

### 9. 文案不再指向一个不存在的工具（`internal/expert/orchestrator.go`）

- 「无 task」返回的信息文案此前写着「主 Agent 可使用
  `agent_expert(action="activate", …)`」——v2 **从未注册**这个工具（`ToolDefinitionName`
  全仓零引用）。主模型照着这段指引调用只会得到一次「未知工具」的失败往返。
  文案改为描述真实可达的能力（用户选择后由引擎直接执行）。

---

## 三、测试

新增测试文件：

| 文件 | 覆盖 |
| --- | --- |
| `internal/expert/subagent_tools_test.go` | 未注册工具被过滤、真实参数 schema 透传、无导出能力时回退占位、全部不可用时干脆不发 `tools` |
| `internal/expert/registry_cache_test.go` | 索引缓存命中 / 保存与删除后失效 / `Load` 返回副本 |
| `internal/engine/expert_fixes_test.go` | 手选模型真的到达 provider、缺省不填模型、closure 恰好一次且在终态之前、降级报 `failed`、集群全员降级报 `failed`、部分降级得到 `partial`、集群统计数字对得上 |
| `frontend/.../experts-data.test.ts` | 可选数=列表长度、ID 唯一、每个部门都有可选专家与标签、筛选计数与实际卡片数一致 |

实测结果（本机，Go 1.27.1 / Node 20）：

- `go build ./...`：通过
- `go vet ./...`：通过
- `go test -short -count=1 ./...`：除 `internal/worker/office` 的
  `TestResolvePathOutsideRootsRejected` 外全部通过。该失败是**既有问题**（`t.TempDir()`
  返回 Windows 8.3 短名 `ADMINI~1`，与允许列表里的长路径不一致），在改动前的干净树上
  同样复现，本轮未改。
- 前端：`vitest run` 91/91 通过；`tsc`（renderer 与 node 两个工程）无错误；
  `electron-vite build` 成功。

---

## 四、有意保留 / 未做（需要产品决策）

1. **`agent_expert` 工具没有接线**。v1 里主 Agent 可以「调用专家」；v2 的专家只由用户
   显式选择（`expert_id`）或集群模式（`cluster_size`）触发。把 `agent_expert` 真的注册成
   工具是一次功能扩展（新工具 + 子代理事件回灌主 run + 权限判定），本轮只做了诚实化
   （删掉误导文案并在常量上写明它未注册）。
2. **自定义专家仍然是「有接口、无实现」**。`expert.CustomStore` 只有测试假件，
   `Dependencies.Experts` 从未被赋值，也没有任何 IPC 路由能创建自定义专家。
   注册表这一侧的缓存与失效语义已经修正，接上存储层即可用；但整条链路要接线
   （`ExpertRepo` → `CustomStore` → IPC → 界面）属于新功能，本轮未做。
3. **专家库页面只展示 60 位精选**（后端目录有 254 位）。要让另外 194 位也能被点名，
   需要新增 `expert.list` 一类 IPC 路由让界面从后端取全量列表；本轮只把话说清楚。
4. **子 Agent 的内部步骤事件仍未上界面**。子代理的工具调用会因为引擎的
   `markToolStarted` 在 Work Log 里出现「工具已启动」行，收尾也有 closure 卡片了；
   但 `outcome.Events`（专家开始/调用工具/结束这类阶段事件）目前只在 `final_answer`
   载荷里以 `workEvents: N` 计数存在。完整呈现需要一条新的子代理事件契约。
5. **`DIVISION_COUNTS`（后端全量 254 位的部门分布）仍保留在数据文件里**，只是界面不再
   用它做计数展示 —— 将来做「全量专家库」时会用到。
6. 审核文档里提到的「温度 0.7 与 EffortHigh 同时下发」经核查**不是缺陷**：
   `provider.BuildRequestBody` 在开启思考模式时本来就不发 `temperature`（两者互斥），
   无需改动。

---

## 五、升级说明

- 无数据库迁移，无配置变更。
- 行为变化（可能影响既有使用习惯）：
  1. 专家子 Agent 没真正执行时，run 的终态由 `completed` 变为 `failed`（答案文本仍然
     照发，界面同时给出降级说明）。
  2. Agent 集群在「一位专家都没真正执行」时报 `failed`。
  3. 输入框旁选了模型时，该次专家/集群执行使用该模型且不做池内失败转移。

```powershell
# 校验版本与升级
ximo-agent.exe --version              REM 应打印 v2.5.0
```
