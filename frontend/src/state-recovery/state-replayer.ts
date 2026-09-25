import { DurableEvent, RunState, ToolCallState, UIStateSnapshot } from './types';

/**
 * StateReplayer 负责将断线期间缺失的 DurableEvent 序列按 sequence 单调严格重放，
 * 保证即使网络丢包、乱序到达，最终呈现的 UI State 与服务端 Engine 严格一致。
 */
export class StateReplayer {
  /**
   * 将增量事件应用到当前 RunState 快照上
   */
  public static applyEvent(current: RunState, event: DurableEvent): RunState {
    // 幂等防卫：若事件 sequence <= 已应用的 sequence，则忽略（防止重复消费）
    if (event.seq <= current.lastSequence) {
      return current;
    }

    const next: RunState = {
      ...current,
      lastSequence: event.seq,
      updatedAt: event.createdAt,
      activeTools: { ...current.activeTools },
      completedTools: [...current.completedTools],
    };

    const payload = event.payload || {};

    switch (event.eventType) {
      case 'run_started':
        next.status = 'thinking';
        break;

      case 'turn_started':
        next.currentTurn = (payload.turn as number) || (next.currentTurn + 1);
        next.status = 'thinking';
        break;

      case 'thinking_delta':
        if (typeof payload.text === 'string') {
          next.reasoningContent = (next.reasoningContent || '') + payload.text;
        }
        break;

      case 'content_delta':
        if (typeof payload.text === 'string') {
          next.content = (next.content || '') + payload.text;
        }
        break;

      case 'tool_call_started': {
        const toolCallId = payload.tool_call_id as string;
        const toolName = payload.tool_name as string;
        const args = (payload.args as Record<string, unknown>) || {};
        const toolState: ToolCallState = {
          toolCallId,
          toolName,
          args,
          status: 'started',
          startedAt: event.createdAt,
        };
        next.activeTools[toolCallId] = toolState;
        next.status = 'executing';
        break;
      }

      case 'tool_call_completed': {
        const toolCallId = payload.tool_call_id as string;
        const existing = next.activeTools[toolCallId];
        const completed: ToolCallState = {
          toolCallId,
          toolName: existing ? existing.toolName : (payload.tool_name as string) || 'unknown',
          args: existing ? existing.args : {},
          status: 'completed',
          result: payload.result,
          startedAt: existing ? existing.startedAt : event.createdAt,
          completedAt: event.createdAt,
        };
        delete next.activeTools[toolCallId];
        next.completedTools.push(completed);
        if (Object.keys(next.activeTools).length === 0) {
          next.status = 'thinking';
        }
        break;
      }

      case 'tool_call_failed': {
        const toolCallId = payload.tool_call_id as string;
        const existing = next.activeTools[toolCallId];
        const failed: ToolCallState = {
          toolCallId,
          toolName: existing ? existing.toolName : (payload.tool_name as string) || 'unknown',
          args: existing ? existing.args : {},
          status: 'failed',
          error: payload.error as string,
          startedAt: existing ? existing.startedAt : event.createdAt,
          completedAt: event.createdAt,
        };
        delete next.activeTools[toolCallId];
        next.completedTools.push(failed);
        if (Object.keys(next.activeTools).length === 0) {
          next.status = 'thinking';
        }
        break;
      }

      case 'run_completed':
        next.status = 'completed';
        break;

      case 'run_failed':
        next.status = 'failed';
        next.error = payload.error as string;
        break;

      case 'run_cancelled':
        next.status = 'cancelled';
        break;

      default:
        // 未知或扩展事件保持安全透传
        break;
    }

    return next;
  }

  /**
   * 将一批事件按单调递增 sequence 排序后逐一重放
   */
  public static replayBatch(initialState: RunState, events: DurableEvent[]): RunState {
    const sorted = [...events].sort((a, b) => a.seq - b.seq);
    let state = initialState;
    for (const evt of sorted) {
      state = StateReplayer.applyEvent(state, evt);
    }
    return state;
  }
}
