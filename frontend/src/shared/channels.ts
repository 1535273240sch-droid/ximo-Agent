/**
 * 主进程 ↔ 渲染进程的 IPC 频道名与共享类型。
 *
 * 单独成文件的原因：频道名是两边的契约，任何一侧拼错都会表现为"调用无响应"，
 * 用常量表可以避免手写字符串。
 */

export const IPC = {
  BackendStart: 'ximo:backend:start',
  BackendStop: 'ximo:backend:stop',
  BackendStatus: 'ximo:backend:status',
  BackendStatusChanged: 'ximo:backend:status-changed',

  RunSubmit: 'ximo:run:submit',
  RunCancel: 'ximo:run:cancel',
  RunStatus: 'ximo:run:status',
  RunEvents: 'ximo:run:events',
  RunResume: 'ximo:run:resume',

  /** 计划模式：确认/否决一个 run 已提出的执行计划（任务4）。 */
  PlanConfirm: 'ximo:plan:confirm',

  /** 工具授权：批准/拒绝一次待确认的工具调用（F5）。 */
  RunDecide: 'ximo:run:decide',

  EventPushed: 'ximo:event:pushed',

  /** 密钥管理（后端只写不读明文）。 */
  SecretStatus: 'ximo:secret:status',
  SecretPut: 'ximo:secret:put',
  SecretDelete: 'ximo:secret:delete',

  /** 运行时配置读写。 */
  ConfigGet: 'ximo:config:get',
  ConfigSet: 'ximo:config:set',

  /** 模型列表查询。 */
  ModelList: 'ximo:model:list',

  /**
   * 记忆图读写面（P1-c，审核文档 4.9）。
   *
   * 与 FrameType.Memory* 一一对应；主进程把每个频道转成同名帧发往后端。
   */
  MemoryGraph: 'ximo:memory:graph',
  MemoryNodeGet: 'ximo:memory:node:get',
  MemoryNodeUpdate: 'ximo:memory:node:update',
  MemoryNodeDelete: 'ximo:memory:node:delete',
  MemoryLink: 'ximo:memory:link',
  MemoryConsolidate: 'ximo:memory:consolidate',
  MemoryExport: 'ximo:memory:export',
  MemoryImport: 'ximo:memory:import',
  MemoryStats: 'ximo:memory:stats',
  MemoryClear: 'ximo:memory:clear',

  WindowMinimize: 'ximo:window:minimize',
  WindowMaximize: 'ximo:window:maximize',
  WindowClose: 'ximo:window:close'
} as const

export type IpcChannel = (typeof IPC)[keyof typeof IPC]
