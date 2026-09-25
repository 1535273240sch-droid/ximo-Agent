# XimoAgent v2 可观测性接入与埋点规范指南 (Observability Integration Guide)

本指南由 **07 号任务（可观测性、测试与发布）** 提供，为 01～06 号任务提供统一的指标采集（Metrics）、分布式链路追踪（Tracing）、结构化日志脱敏（Logging）以及健康检查（Health）接入标准。

---

## 一、通用接口契约与包引用

统一包路径：`internal/observability`

```go
import "ximo-agent/internal/observability"
```

### 1. 指标接口契约
```go
type Metrics interface {
    Inc(name string, labels ...Label)
    Observe(name string, value float64, labels ...Label)
    Gauge(name string, value float64, labels ...Label)
}
```

### 2. 链路追踪接口契约
```go
type Tracer interface {
    StartSpan(ctx context.Context, name string) (context.Context, Span)
}

type Span interface {
    End()
    SetTag(key string, val any) Span
    RecordError(err error) Span
    Context() TraceContext
}
```

---

## 二、各模块（01～06）埋点调用位置清单

### 1. 任务 01：Supervisor 与 IPC 基座
- **进程/Worker 重启事件**：
  在 Supervisor 监听到子进程/Worker 退出并执行退避重启时调用：
  ```go
  observability.WorkerRestart(workerKind, workerID)
  ```
- **进程/Worker 崩溃事件**：
  在捕捉到异常退出或段错误时调用：
  ```go
  observability.WorkerCrash(workerKind, workerID)
  ```
- **IPC 链路 Span 注入与提取**：
  在发送 IPC Frame 时，将 context 中的 TraceID/SpanID 携带在 Frame 标头中；接收端调用 `observability.WithTraceContext(ctx, tc)` 注入上下文。
- **系统健康暴露**：
  将 `observability.DefaultHealthManager().LivenessHandler()` 和 `ReadinessHandler()` 挂载在主进程的本地 HTTP/IPC 探针接口上。

---

### 2. 任务 02：Engine 核心与调度器
- **Run 生命周期指标（低基数聚合统计，严禁传入 run_id 标签以防内存泄露）**：
  - Run 创建/启动时：`observability.RunStarted()`（或传入固定低基数标签如 `status=started`）
  - Run 成功完成时：`observability.RunCompleted()`
  - Run 失败/异常时：`observability.RunFailed(reason)`（reason 必须为枚举值，如 `timeout`, `internal_error`, `user_cancelled`）
  - 崩溃重启后恢复成功：`observability.RunRecovered()`
- **调度器与排队指标**：
  - 入队/出队更新队列深度：`observability.SetQueueDepth(currentDepth)`
  - 任务在队列中等待完成时：`observability.RecordQueueWait(waitDurationMs)`
  - 活跃运行中 Run 计数：`observability.SetActiveRuns(activeCount)`
- **Trace 树树根与轮次构建（高基数个体追踪必须由 Tracing 承载）**：
  - 在 Run 开始时启动 Root Span：
    ```go
    runCtx, runSpan := observability.StartSpan(ctx, "run:"+runID)
    runSpan.SetTag("run_id", runID).SetTag("session_id", sessionID)
    defer runSpan.End()
    ```
  - 在进入每一轮 Agent Loop (Think) 时启动 Turn Span：
    ```go
    turnCtx, turnSpan := observability.StartSpan(runCtx, fmt.Sprintf("turn:%d", turnIndex))
    defer turnSpan.End()
    ```

---

### 3. 任务 03：存储层与 Checkpoint
- **SQLite 事务耗时**：
  在单 Writer 队列提交事务后调用：
  ```go
  observability.DBCommitLatency(commitDurationMs)
  ```
- **SQLITE_BUSY / 锁等待**：
  在捕获到 `sqlite3.ErrBusy` 或重试时调用：
  ```go
  observability.DBBusy()
  ```
- **CAS 与 Checkpoint 追踪**：
  在生成 Checkpoint 快照、原子落盘及恢复校验时注入 Span：
  ```go
  ctx, span := observability.StartSpan(ctx, "checkpoint:save")
  span.SetTag("manifest_id", manifestID)
  defer span.End()
  ```

---

### 4. 任务 04：工具运行时与权限安全
- **工具调用生命周期**：
  - 工具开始调用时：
    ```go
    observability.ToolStarted(toolName)
    observability.Default().Gauge(observability.MetricActiveTools, float64(activeToolCount))
    ```
  - 工具成功返回时：
    ```go
    observability.ToolCompleted(toolName)
    ```
  - 工具返回错误时：
    ```go
    observability.ToolFailed(toolName, err.Error())
    ```
  - 工具执行超时时：
    ```go
    observability.ToolTimeout(toolName)
    ```
- **结构化日志脱敏（Secrets 红线）**：
  在工具参数校验、审计日志记录处，使用统一脱敏工具：
  ```go
  sanitizedInput := observability.SanitizeMap(rawToolInput)
  ```
  该函数会自动将 `api_key`、`authorization`、`cookie`、`password`、`token` 以及匹配 `sk-...` 的内容屏蔽为 `[REDACTED]`。

---

### 5. 任务 05：Worker 池 (Browser / Terminal / MCP / DynamicJS)
- **Worker 崩溃与心跳丢失**：
  在 Worker Manager 发现 Worker 不响应或异常退出时调用：
  ```go
  observability.WorkerCrash(workerKind, workerID)
  ```
  在拉起新实例替代后调用：
  ```go
  observability.WorkerRestart(workerKind, workerID)
  ```
- **浏览器实例指标**：
  在 BrowserPool 申请与释放 Page/Worker 时更新仪表盘：
  ```go
  observability.Default().Gauge(observability.MetricBrowserCount, float64(activeBrowserCount))
  ```
- **工具在 Worker 端的执行追踪**：
  在 Worker 接收到 IPC 调用并派发给子进程时：
  ```go
  ctx, span := observability.StartSpan(ctx, "worker:"+workerKind+":exec")
  span.SetTag("worker_id", workerID)
  defer span.End()
  ```

---

### 6. 任务 06：Provider 层与上下文管理
- **请求计数与限频**：
  - 发起 LLM Completion/Stream 调用时：
    ```go
    observability.ProviderRequest(providerName, modelName)
    ```
  - 触发 429 限频错误时：
    ```go
    observability.Provider429(providerName)
    ```
  - 触发分类重试时（5xx / 网络超时 / 429 backoff）：
    ```go
    observability.ProviderRetry(providerName, reason)
    ```
  - 记录端到端延迟（毫秒）：
    ```go
    observability.ProviderLatency(providerName, modelName, latencyMs)
    ```
- **LLM 请求追踪**：
  在 Complete / Stream 核心方法内继承上游 Turn Span：
  ```go
  pCtx, pSpan := observability.StartSpan(turnCtx, "provider:complete")
  pSpan.SetTag("provider", providerName).SetTag("model", modelName)
  defer pSpan.End()
  ```
- **API Key 防泄露红线**：
  绝不可在日志中输出 Headers 或带 `sk-...` 的明文，统一使用 `observability.LogInfo` / `observability.LogError`，其内部自动执行正则与键名脱敏。
