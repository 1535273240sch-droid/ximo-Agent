import { DurableEvent, RunState, SessionState, UIStateSnapshot } from './types';
import { StateReplayer } from './state-replayer';

export interface TransportClient {
  getSessionState(sessionId: string): Promise<SessionState>;
  getRunState(runId: string): Promise<RunState>;
  getEventsSince(runId: string, afterSeq: number): Promise<DurableEvent[]>;
  subscribeEvents(runId: string, onEvent: (evt: DurableEvent) => void): () => void;
}

export type UIStateListener = (snapshot: UIStateSnapshot) => void;

/**
 * ReconnectManager 严格落地第26章UI状态恢复规范：
 * 启动或断线重连流程固定为：
 * 1. GET session state
 * 2. GET run state
 * 3. GET events since sequence (拉取缺失的增量事件)
 * 4. snapshot + event replay 重建 UI
 */
export class ReconnectManager {
  private transport: TransportClient;
  private currentSessionId: string | null = null;
  private currentSnapshot: UIStateSnapshot | null = null;
  private listeners: Set<UIStateListener> = new Set();
  private eventBuffer: DurableEvent[] = [];
  private unsubscribeStream: (() => void) | null = null;
  private isRecovering = false;
  private backoffMs = 500;
  private readonly maxBackoffMs = 10000;

  constructor(transport: TransportClient) {
    this.transport = transport;
  }

  public subscribe(listener: UIStateListener): () => void {
    this.listeners.add(listener);
    if (this.currentSnapshot) {
      listener(this.currentSnapshot);
    }
    return () => {
      this.listeners.delete(listener);
    };
  }

  private notify() {
    if (!this.currentSnapshot) return;
    for (const l of this.listeners) {
      l(this.currentSnapshot);
    }
  }

  /**
   * 初始化或断线重连入口
   */
  public async recoverState(sessionId: string): Promise<void> {
    if (this.isRecovering) {
      return;
    }
    this.isRecovering = true;
    this.currentSessionId = sessionId;

    try {
      // Step 1: GET session state
      const session = await this.transport.getSessionState(sessionId);

      let run: RunState | undefined = undefined;
      let lastSeq = 0;

      if (session.activeRunId) {
        // Step 2: GET run state
        run = await this.transport.getRunState(session.activeRunId);
        lastSeq = run.lastSequence || 0;

        // Step 3: GET events since sequence
        const missedEvents = await this.transport.getEventsSince(session.activeRunId, lastSeq);

        // Step 4: snapshot + event replay 重建 UI
        if (missedEvents.length > 0) {
          run = StateReplayer.replayBatch(run, missedEvents);
        }

        // 处理在恢复拉取期间可能收到的实时推送缓冲
        if (this.eventBuffer.length > 0) {
          run = StateReplayer.replayBatch(run, this.eventBuffer);
          this.eventBuffer = [];
        }

        lastSeq = run.lastSequence;

        // 重新建立实时流订阅
        if (this.unsubscribeStream) {
          this.unsubscribeStream();
        }
        this.unsubscribeStream = this.transport.subscribeEvents(session.activeRunId, (evt) => {
          this.handleLiveEvent(evt);
        });
      }

      this.currentSnapshot = {
        session,
        run,
        isReplaying: false,
        replayedEventsCount: run ? run.completedTools.length : 0,
        lastAppliedSequence: lastSeq,
      };

      this.backoffMs = 500; // 重置重试退避
      this.notify();
    } catch (err) {
      console.error('[ReconnectManager] State recovery failed, scheduling retry...', err);
      this.scheduleRetry(sessionId);
    } finally {
      this.isRecovering = false;
    }
  }

  /**
   * 处理实时流到达的事件
   */
  private handleLiveEvent(evt: DurableEvent) {
    if (!this.currentSnapshot || !this.currentSnapshot.run) {
      this.eventBuffer.push(evt);
      return;
    }

    if (this.isRecovering) {
      // 正在全量重构中，暂存进缓冲区
      this.eventBuffer.push(evt);
      return;
    }

    const currentRun = this.currentSnapshot.run;

    // 检查是否存在 sequence 空洞 (断线或漏包)
    if (evt.seq > currentRun.lastSequence + 1) {
      console.warn(
        `[ReconnectManager] Sequence gap detected: expected ${currentRun.lastSequence + 1}, got ${evt.seq}. Triggering recovery.`
      );
      this.eventBuffer.push(evt);
      if (this.currentSessionId) {
        this.recoverState(this.currentSessionId);
      }
      return;
    }

    const updatedRun = StateReplayer.applyEvent(currentRun, evt);
    this.currentSnapshot = {
      ...this.currentSnapshot,
      run: updatedRun,
      lastAppliedSequence: updatedRun.lastSequence,
    };
    this.notify();
  }

  /**
   * 指数退避重连
   */
  private scheduleRetry(sessionId: string) {
    setTimeout(() => {
      this.recoverState(sessionId);
    }, this.backoffMs);

    this.backoffMs = Math.min(this.backoffMs * 2, this.maxBackoffMs);
  }

  public getSnapshot(): UIStateSnapshot | null {
    return this.currentSnapshot;
  }

  public destroy() {
    if (this.unsubscribeStream) {
      this.unsubscribeStream();
      this.unsubscribeStream = null;
    }
    this.listeners.clear();
    this.eventBuffer = [];
  }
}
