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

  WindowMinimize: 'ximo:window:minimize',
  WindowMaximize: 'ximo:window:maximize',
  WindowClose: 'ximo:window:close'
} as const

export type IpcChannel = (typeof IPC)[keyof typeof IPC]
