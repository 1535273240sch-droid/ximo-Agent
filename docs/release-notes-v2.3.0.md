# XimoAgent v2.3.0 更新说明

发布日期：2026-10-01 ｜ 版本跨度：v2.2.1 → v2.3.0

## 一、本次更新概述

这是一次按《XimoAgent v2 审核报告 + 重构设计文档》执行的结构性版本。审核报告的
结论是「闭环有问题，且 4 个是会让用户以为完成了其实没完成的硬伤」。本版本把
**P0 全部四项**（模型选择器、循环闭环、事件补全、授权闭环）与 **P1-a 工作日志**
落地，并实现了 **P1-b Synapse 图记忆后端**（尚未装配到配置，见第五节）。

一句话概括：**任务做没做完，现在会如实说出来，而且用户第一次既有按钮可点、
也有证据可看。**

---

## 二、新增

### 1. 闭环校验与 `run.closure` 事件（F1–F4）

- 每个终态路径（正常完成、待办完成收尾、预算耗尽收尾、截断收尾、空答案失败、
  失败、取消）**恰好发一次** `run.closure`，且**先于终态迁移**——事件流在终态
  关闭，晚了前端收不到。顺序被单测钉住：`final_answer` → `run.closure` → 终态。
- 报告内容（`internal/types/t02_closure.go`，纯确定性检查，不额外花模型调用）：

  | check id | 通过条件 |
  |---|---|
  | `answer_nonempty` | 最终答案 `TrimSpace != ""` |
  | `answer_not_truncated` | 不是「续写次数用尽」的截断收尾 |
  | `todos_done` | 用过 `todo_write` 时最后一次快照 `done == total`（没用过则不出现在报告里） |
  | `tool_errors_resolved` | 不存在「某工具目标最后一次调用仍失败」（同目标后来成功即视为已解决） |
  | `budget_ok` | 未走 `budget_exhausted` 收尾 |
  | `review_ok` | 有复核时最后一次 `verdict == on_track`（无复核则不出现） |

- verdict 规则：任一 `answer_nonempty` 失败 → `failed`；`budget_ok` /
  `answer_not_truncated` / `todos_done` / `tool_errors_resolved` / `review_ok`
  失败 → `partial`；停在等待授权 / 计划 → `needs_user`；全通过 → `closed`。
- **状态机一字未改**：不新增状态、不新增迁移边。「是否完成得干净」用事件表达。

### 2. 工作日志（Work Log）时间线

一次任务的全部内部工作收敛成回答正上方的一张可折叠卡片：规划、记忆召回、每轮
思考、每次工具调用、复核纠偏、长任务续段、上下文压缩、等待授权、闭环校验。

- 折叠态一行实时显示「第 N 步 · 正在做什么」＋已用时；完成后变成摘要。
- 展开态是竖向时间线，每步可再展开看**入参 / 结果预览 / 错误原因 / 记忆路径 /
  复核问题**。
- 动效：`grid-template-rows: 0fr → 1fr` 折叠（不测高、不跳）、进行中圆点脉冲、
  进行中连线流动渐变、ticker 文字上滑淡入；`prefers-reduced-motion` 下全部关闭。
- 展开态是**用户主权**的：用户手动开合后，事件更新不再覆盖它；自动展开只发生在
  等待授权 / 失败 / 部分完成时。

### 3. 工具授权内联批准（F5）

- 新增 IPC 帧 `engine.run.decide`（`{run_id, call_id, approve, remember?}`）与
  可选接口 `types.ToolDecider`，两侧契约（`wire.go` / `types.ts`）同提交同步。
- 前端 preload / main / backend-client / frame / channels / store 全线打通。
- 需要授权的工具步骤**内联**「批准执行 / 拒绝」，并显示工具名与参数摘要。
  `remember: "session"` 时同一会话内同类工具直接放行（模型重发调用会带新 call ID，
  因此会话级放行按工具名匹配）。
- 拒绝不是失败：后端把「用户拒绝执行」作为工具结果喂回模型，让它另想办法。
- 事件 `tool_call.permission_required` 现在带 `callId / toolName / arguments / message`。

### 4. Synapse 图记忆后端（后端已实现，装配未接线）

按审核文档第 4 章实现：`mem_nodes` / `mem_edges` / `mem_fts` / `mem_recall_log`
独立库；写入流水线（脱敏 → 抽取 → 去重 → 实体链接 → 建边 → 矛盾检测 → 单事务）；
2 跳扩散激活召回（阈值 0.05、fanOut 8、hopDecay 0.6/0.35、按路径给出 `via`）；
赫布强化与惰性衰减（`used_with` 45 天 / `related` 90 天半衰期，`pinned` 不衰减）；
`Consolidate` 整理与旧 `memories` 幂等迁移。2 万节点召回基准 p95 ≈ 35–51ms
（预算 80ms）。配置已能识别 `memory.backend: "synapse"`（`EffectiveBackend` /
`Validate` / `Active` 三处），但 bootstrap 尚未按该值装配后端——默认行为与本次
改动前完全一致。详见 `internal/memory/README-synapse.md`。

### 5. 工具授权链路补全（F5 的生产路径）

审核报告确认 C4 时只看到「前端没有按钮」。把链路打通之后还发现两处后端断点，
本次一并修好，否则前端加了按钮也永远不会被触发：

1. `internal/tool` 的 `EffectAsk` 在没有交互式 Confirmer 时把「需要确认」报成
   `ErrPermissionDenied`——ask 与 deny 无法区分，Agent 循环因此永远不知道
   「人类还能说可以」。现在返回 `ErrNeedsConfirmation` 并回填
   `RequiresConfirmation` / `ConfirmationMessage`。
2. `bootstrap/tool_adapter.go` 没有把这两个字段透传进 `types.ToolResult.Metadata`，
   而引擎正是从 Metadata 里读它们；同时 `ports.ToolRequest` 缺少「已批准」通道，
   用户点了批准也没有任何东西能告诉权限层。现在适配器双向补齐（`Confirmed` /
   `ConfirmedForSession`），且**权限层的 deny 规则仍然优先**——批准只撤销「询问」
   这一步，不是提权。

### 6. 前端单测基础设施

新增 `vitest` + `jsdom` + `@testing-library/react`（`frontend/vitest.config.ts`），
并补上 `npm test`。覆盖模型选择器的定位 / 键盘 / 持久化，以及「事件 → 步骤」纯
函数折叠器。

---

## 三、修复

1. **C1｜待办全部完成后没有总结就显示「已完成」**：`runRound` 在 `stop` 时不再直接
   终止，而是返回 `WrapUp: true` 让 `Run` 花一轮**无工具**的总结轮；`forceWrapUp`
   按 reason 区分「待办完成」与「预算耗尽」，前者不再重复追加收尾提示。
   回归测试断言第 2 次 provider 请求 `Tools == nil` 且答案来自那一轮。
2. **C2｜预算耗尽被标成 completed**：状态仍是 `completed`（不动状态表），但
   `run.closure` 带 `verdict=partial`，`final_answer.Data.incomplete=true`，界面显示
   「部分完成」并提供「继续完成」——它会把未通过的检查项原文作为新任务提交。
3. **C3｜`max_tokens` 截断被当作完成**：拆开 `FinishStop` / `FinishLength`；截断时
   自动追加「请从中断处继续」并进入下一轮，最多 `MaxLengthContinues`（默认 3，进
   config 不写死）；超限才接受答案，且 `answer_not_truncated=false`。
4. **C4｜需要授权时用户没有任何按钮可点**：见上文「工具授权内联批准」。旧的那条
   没有按钮的「安全策略阻断」横幅已删除。
5. **C5｜工具成功结果与失败原因在界面上永远是空的**：后端 `tool_call.completed`
   现在带 `data.result`（脱敏 + 2 KiB 截断预览），`tool_call.failed` 带 `data.error`
   （脱敏）与 `data.result`；前端同时兼容读取 `ev.message` 作为兜底。
   完整输出仍然只进对话，事件体积有界。
6. **C6｜工具轨迹永远堆在对话最底部**：改为工作日志时间线。旧 `tools` 字典 +
   `loose` 渲染路径已从 `MessageList` 移除。
7. **C7｜复核 / 续段 / 压缩事件前端一律不处理**：三者都在时间线上有步骤，另有
   `run.closure`。
8. **C8｜最终答案没有任何校验 / 空答案也算完成**：空答案会追加一次「请给出最终
   答复」重试；仍为空则 `failed` 并带明确错误事件与 `verdict=failed`。
9. **模型选择器「点了没反应」（1A 节）**：面板改为 `createPortal(document.body)` +
   `fixed` 定位 + 上下自动翻转，彻底绕开输入框祖先的 `overflow-hidden` 裁剪；并在
   `resize` / `scroll` 时重算位置。附带搜索过滤、↑↓/Enter/Esc 键盘操作、
   `localStorage['ximo.model']` 记住上次选择、「该模型不在当前服务商列表中」警告态
   （保留选择不清空）。
10. **`app-store.submit()` 的合并漏洞（1A 第 4 条）**：旧写法 `let effective = opts;
    if (!effective) { …合并… }`——只要调用方传了 `opts`，模型 / 计划 / 专家开关会被
    整体绕过。现在**始终合并**，显式传入字段优先。
11. **`-ldflags -X main.version` 一直是空操作**：`cmd/ximo-agent/main.go` 的版本变量
    是未导出的，`-X` 无法覆盖，CI 注入版本号静默失败、二进制永远打印编译进去的字
    面量。已导出为 `main.Version`，并同步 `build.cmd`、`.github/workflows/release.yml`
    的 ldflags；`ximo-agent.exe --version` 现在与发布标签一致。
12. **`frontend` 的 `typecheck` 脚本依赖 `npm`**：改为直接调用 `tsc`，不再要求环境
    里有 npm。
13. **权限层的 ask 与 deny 无法区分、且「需要确认」标记在适配器处被丢弃**：见上文
    「工具授权链路补全」。`internal/tool` 的既有断言（无确认器必须 fail-closed）
    相应更新为「必须交回调用方等待人工确认且绝不执行」，不变量本身没有放松。

---

## 四、前后端契约变更（只追加，未改动或删除任何已有字段）

| 位置 | 变更 |
|---|---|
| `internal/types/t02_events.go` | 新增 durable 事件 `run.closure`、`memory.recalled` |
| `internal/types/t02_closure.go`（新） | `RunClosureReport` / `ClosureCheck` / `ClosureTracker` / `ToolCallKey` |
| `internal/types/t02_contract.go` | 新增可选接口 `ToolDecider` |
| `internal/types/t02_config.go` | `AgentConfig.MaxLengthContinues`（默认 3）、`EmptyAnswerRetries`（默认 1） |
| `internal/ipc/protocol.go` | 新增帧名 `engine.run.decide` |
| `internal/ipcapi/wire.go` | 新增 `DecidePayload` / `DecideResultPayload`（两侧同步） |
| `internal/ipcapi/service.go` | 注册 `engine.run.decide`（`Decide` 是可选能力，未实现则明确报错） |
| `internal/ports/ports.go` | `ProviderResponse.Model`（实际使用的模型名回显） |
| `internal/agent/loop.go` | `LoopInput.ToolDecisions`、`LoopResult.Closure` / `PendingToolCallIDs` / `PendingToolCallNames` |
| `frontend/src/shared/types.ts` | `ClosureReport` / `ClosureCheck` / `ClosureVerdict` / `ToolPermissionRequest` / `DecidePayload` / `DecideResultPayload`、两个新事件名、`XimoBridge.decide` |
| `frontend/src/shared/channels.ts` / `frame.ts` | `ximo:run:decide` / `engine.run.decide` |

注入位置与体积不变量都保持不变：记忆消息仍是独立 system 消息、插在稳定系统提示词
之后；「空块不插」不变；新增的「续写 / 空答案重试 / 授权恢复」提示只追加在消息列表
尾部，稳定前缀字节不变。

---

## 五、已知事项与后续（诚实清单）

1. **「批准后重放」的跨代际语义**：批准的解析发生在 `observe()` 内部（按 call ID，
   会话级按工具名）。工具调用 ID 属于产出它的那条 assistant 消息，而线协议只接受
   「紧接在前的 assistant 消息里的调用」的结果，所以 park 再 resume 之后无法直接重放
   原调用；会话级「记住」是让 run 真正继续下去的那条路径。跨进程崩溃后的待授权集合
   重建（`pendingToolCallsFromLog`）已在引擎侧实现并有端到端用例，但**在本次改动前
   的生产链路上根本没有 park 可恢复**——原因就是上文第 5 条的两个断点，现已修复。
2. **Synapse 后端只差 bootstrap 装配**：配置已能识别 `"synapse"`，后端实现、测试
   与基准都已完成，但 bootstrap 还没有按该值构造它（也还没有把 provider 适配成
   抽取器、没有发 `memory.recalled` 事件）。在装配完成前，启用记忆仍请使用 mem0 或
   内置 embedded 后端；`memory.recalled` 事件虽然前端已就绪，引擎侧还没有发出。
3. **工作日志是整轮时间线，不是逐条消息内联**：一次 run 的「第 N 轮回答」与「第 N 轮
   工具调用」是同一轮的两面，逐条插入需要把事件重新切回消息边界——而那正是旧实现
   算错的地方。时间线整块放在本次回答最上方，位置稳定、顺序正确。
4. **P1-c 记忆界面（力导向图 / 记忆页）未实现**：`d3-force` 与
   `components/memory/*` 不在本次范围。
5. **未做真机端到端验证**：本次验证是 `go build ./...` + `go vet ./...` +
   `go test -short ./...` + 前端 `tsc` + `vitest` + `electron-vite build`，
   没有起真实 Electron + 真实服务商密钥跑完整 GUI 流程。
6. **审核文档待验证项 V2 仍未决**：手选模型对专家 / 集群模式的语义（覆盖池分配
   还是仅主模型）需要产品决策，本版本未改。
7. **一个与本次改动无关的既有失败**：`internal/worker/office` 的
   `TestResolvePathOutsideRootsRejected` 在本机（Windows 8.3 短路径名 `ADMINI~1`）
   失败——`t.TempDir()` 返回短名而白名单用长名比对。已在 v2 分支**未做任何改动**
   的工作树上复现同样失败，确认是既有问题，未顺手修。

---

## 六、验证方式

```cmd
build.cmd verify                     REM build + vet + test-short
cd frontend && npm run typecheck      REM tsc（node + web 两个工程）
cd frontend && npm test               REM vitest
cd frontend && npm run build          REM electron-vite build
ximo-agent.exe --version              REM 应打印与发布标签一致的版本
```
