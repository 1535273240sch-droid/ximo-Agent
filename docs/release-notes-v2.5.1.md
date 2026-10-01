# XimoAgent v2.5.1 更新说明

发布日期：2026-10-01 ｜ 版本跨度：v2.5.0 → v2.5.1

## 一、本次更新概述

v2.5.0 修掉了「未注册的工具被塞进子 Agent 的 function schema」这个缺陷。修完之后
顺着同一根线追问了一句：**过滤之后，专家到底还剩几个工具？**

实测（默认配置、真实装配注册表）：

- 生产注册表共 **25 个**工具可用：
  `file_read` / `file_list` / `file_search` / `file_write` / `file_delete`、
  `git_status` / `git_log` / `git_branch` / `git_diff`、
  `knowledge` / `memory` / `web_fetch`、
  `terminal_exec`、`browser_*`（7 个）、`mcp_*`（4 个）、`dynamic_eval`。
- 专家按部门过滤后，每位专家手里剩下 **2～5 个**真工具：

  | 部门 | 过滤后可用工具 |
  | --- | --- |
  | engineering | file_read, file_write, file_search, terminal_exec |
  | testing | file_read, file_search, terminal_exec |
  | security | file_read, file_search, terminal_exec, web_fetch |
  | gis / spatial-computing | file_read, file_write, terminal_exec |
  | project-management | file_read, file_write, web_fetch, terminal_exec |
  | marketing / paid-media | file_read, file_write, web_fetch, browser_navigate, browser_screenshot |
  | academic / finance / healthcare / product / sales / specialized / support | file_read, file_write, web_fetch |
  | design | file_read, file_write |

  → **结论：专家确实能调用工具**，但部门和关键词推荐表里那一长串名字
  （`code_execute` / `code_lint` / `git_operations` / `todo_write` / `web_search` /
  `ui_generate` / `design_*` / `computer_use` …）**在 v2 里根本没有实现**。

而**问题就在这里**：实施阶段的系统提示词此前直接照抄推荐表，于是模型被告知
「你已被配置以下工具：`code_execute`、`git_operations`、`todo_write`…」，而它真正能调的
只有 file/git/web/terminal 那几个。后果有两种，都很糟：

1. 模型发起一个未注册工具的调用 → 得到一次「工具未注册」的失败往返，白烧一轮；
2. 模型直接在回答里声称「我已用 code_execute 运行了测试」——用户看到一份自信的假执行报告。

v2.5.1 修的就是这件事：**提示词里说有的工具，schema 里就一定有；schema 里没有的，
提示词一个字都不提。**

---

## 二、修复内容

### 1. 系统提示词与 function schema 同源同集合（`internal/expert`）

- 新增 `BuildSystemPromptWithTools(e, tools)`：只把调用方**核实过**的工具名写进
  「可用工具」一节。`BuildSystemPrompt(e)` 保留原行为（展示完整推荐表），继续供
  「只看专家档案、不执行」的信息型返回使用。
- 新增 `SubAgentOptions.effectiveToolNames()`：复用 `resolveTools` 的同一套过滤
  （注册表 + 可用性 + 真实定义），保证提示词与 schema 不会各说一套。
- `runPhase` 改用 `BuildSystemPromptWithTools(e, runner.effectiveToolNames())`。
- **没有工具时不再留一段空列表**，而是明确告知并加上硬约束：
  「本次**没有任何可用工具**……不要声称调用了任何工具；不要输出工具调用、工具名或
  伪造的执行结果；如果任务必须依赖某个工具才能完成，直接说明缺少什么能力。」
  输出要求里的「主动使用可用工具」也在无工具时替换为「不要编造工具执行结果」。

### 2. 把「真实注册表」与「推荐表」的差距钉成测试（`internal/bootstrap`）

- `TestRegisteredToolNamesMatchProductionWiring`：默认配置下注册表的工具清单快照
  （25 项：12 个核心域工具 + 13 个 worker 工具）。注册表一变、快照没跟，测试就红。
- `TestExpertRecommendationsExistInRegistry`：推荐表（17 个部门 + 12 条关键词规则 +
  默认工具集）里的每个名字，要么真的注册了，要么必须在
  `knownMissingRecommendedTools` 里明确登记。新增工具却忘了同步时同样会红。
- `TestEveryDivisionKeepsAtLeastOneRealTool`：过滤后每个部门必须至少剩 1 个真工具，
  并把每部门的可用清单打进测试日志（上面那张表的来源）。

### 3. 专家的提示词一致性测试（`internal/expert`）

- `TestExecutePromptOnlyAdvertisesExecutableTools`：工程类专家的实施阶段提示词必须
  列出 `file_read/file_write/file_search/terminal_exec`，且**不得**出现
  `code_execute`/`code_lint`/`code_format`/`git_operations`/`todo_write`/`multi_edit`/
  `project_index`/`dependency_check`；同时校验提示词集合与 `Tools` schema 完全一致。
- `TestExecutePromptDeclaresNoToolsWhenNoneAvailable`：一个工具都不可用时，提示词必须
  明确说「没有任何可用工具」、schema 为空、且带上禁止虚构的约束。
- `TestInfoPathKeepsRecommendationList`：信息型返回（无 task）仍展示完整推荐表。

---

## 三、测试

```
go build ./...        → 通过
go vet ./...          → 通过
go test -short ./...  → 仅 internal/worker/office 的 TestResolvePathOutsideRootsRejected 失败
                        （既有问题：t.TempDir() 返回 Windows 8.3 短名，改动前同样失败）
gofmt（5 个改动/新增文件）→ 干净
```

一次观察到的偶发失败：全量并行 `go test ./...` 时
`internal/bootstrap` 的 `TestAssemble_RunsAgainstRealSQLite` 曾报
`answer = "", want "装配成功"`（run 状态是 completed 但日志读回的 answer 为空，
WAL 读连接可见性/负载相关）。单独重跑 3/3 通过，随后全量重跑也通过。它走的是主
Agent 循环，与本次改动无关，未在本次修复范围内，已记录待观察。

---

## 四、新发现（需要产品决策，未在本轮修）

1. **`todo_write` 在 v2 根本不存在**，但三处都在引用它：
   - 每个部门的推荐工具表；
   - F1 闭环（`ClosureReasonTodosDone` + `todos_done` 检查）；
   - 权限与幂等分类表（`permission.go` / `idempotency.go` 里为它预置了规则）。
   结论：**「待办全部完成」这个闭环判据在生产里不可达** —— 模型没有这个工具可调，
   `TodoSeen` 永远为 false，该检查项按「不适用」被省略。要么实现 `todo_write`，
   要么把这条闭环与其配套配置摘掉，不能继续挂着。
2. 推荐表里 20+ 个名字（`web_search` / `web_research` / `code_execute` /
   `file_edit` / `multi_edit` / `git_operations` / `design_*` / `ui_generate` /
   `computer_use` / `network_*` …）在 v2 未实现。本轮只让提示词不再谎报，**没有去实现它们**；
   是否补齐属能力规划问题。
3. 注册表里 `knowledge`、`memory`、`git_*` 这些工具**任何部门/关键词都不会推荐**，
   因此专家永远拿不到它们（只有主 Agent 能用）。若希望专家会用知识库/记忆/Git，
   需要在推荐表里补上——现在这个缺口是静默的。
4. `internal/tool/dynamic.go` 的 `create_tool` 工具没有在 bootstrap 注册
   （dynamic-js worker 只暴露 `dynamic_eval`）。

---

## 五、升级说明

- 无数据库迁移、无配置变更。
- 行为变化：专家（单专家直连与 Agent 集群）的实施阶段提示词只列**真实可用**的工具；
  无可用工具时明确说明并禁止虚构工具调用。此前提示词会列出推荐表里的全部名字。
