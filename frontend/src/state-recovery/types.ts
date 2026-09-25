/**
 * UI 状态恢复与事件重放核心类型定义
 * 对应第 26 章：GET session state → GET run state → GET events since sequence → snapshot+event replay
 */

export interface SessionState {
  sessionId: string;
  title: string;
  activeRunId?: string;
  createdAt: number;
  updatedAt: number;
}

export type RunStatus =
  | 'created'
  | 'queued'
  | 'planning'
  | 'thinking'
  | 'executing'
  | 'compacting'
  | 'waiting_user'
  | 'completed'
  | 'cancelled'
  | 'failed'
  | 'recovering';

export interface ToolCallState {
  toolCallId: string;
  toolName: string;
  args: Record<string, unknown>;
  status: 'started' | 'completed' | 'failed' | 'timeout';
  result?: unknown;
  error?: string;
  startedAt: number;
  completedAt?: number;
}

export interface RunState {
  runId: string;
  sessionId: string;
  status: RunStatus;
  currentTurn: number;
  maxTurns: number;
  lastSequence: number;
  content: string;
  reasoningContent: string;
  activeTools: Record<string, ToolCallState>;
  completedTools: ToolCallState[];
  error?: string;
  createdAt: number;
  updatedAt: number;
}

export interface DurableEvent {
  runId: string;
  seq: number;
  eventId: string;
  eventType: string;
  payload: Record<string, unknown>;
  createdAt: number;
}

export interface UIStateSnapshot {
  session: SessionState;
  run?: RunState;
  isReplaying: boolean;
  replayedEventsCount: number;
  lastAppliedSequence: number;
}
