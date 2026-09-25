import { ReconnectManager, TransportClient } from './reconnect-manager';
import { DurableEvent, RunState, SessionState, UIStateSnapshot } from './types';

describe('UI State Recovery and Replay (Chapter 26)', () => {
  it('should recover state by GET session -> GET run -> GET missed events -> replay', async () => {
    const mockSession: SessionState = {
      sessionId: 'sess-100',
      title: 'Test Session',
      activeRunId: 'run-500',
      createdAt: 1000,
      updatedAt: 1000,
    };

    const mockInitialRun: RunState = {
      runId: 'run-500',
      sessionId: 'sess-100',
      status: 'thinking',
      currentTurn: 1,
      maxTurns: 30,
      lastSequence: 2,
      content: 'Initial thinking...',
      reasoningContent: '',
      activeTools: {},
      completedTools: [],
      createdAt: 1000,
      updatedAt: 1000,
    };

    const missedEvents: DurableEvent[] = [
      {
        runId: 'run-500',
        seq: 3,
        eventId: 'evt-3',
        eventType: 'tool_call_started',
        payload: { tool_call_id: 'call-1', tool_name: 'file_read', args: { path: 'main.go' } },
        createdAt: 1050,
      },
      {
        runId: 'run-500',
        seq: 4,
        eventId: 'evt-4',
        eventType: 'tool_call_completed',
        payload: { tool_call_id: 'call-1', tool_name: 'file_read', result: 'package main' },
        createdAt: 1100,
      },
      {
        runId: 'run-500',
        seq: 5,
        eventId: 'evt-5',
        eventType: 'run_completed',
        payload: {},
        createdAt: 1200,
      },
    ];

    const mockTransport: TransportClient = {
      getSessionState: jest.fn().mockResolvedValue(mockSession),
      getRunState: jest.fn().mockResolvedValue(mockInitialRun),
      getEventsSince: jest.fn().mockResolvedValue(missedEvents),
      subscribeEvents: jest.fn().mockReturnValue(() => {}),
    };

    const manager = new ReconnectManager(mockTransport);

    let lastSnapshot: UIStateSnapshot | null = null;
    manager.subscribe((s) => {
      lastSnapshot = s;
    });

    await manager.recoverState('sess-100');

    expect(mockTransport.getSessionState).toHaveBeenCalledWith('sess-100');
    expect(mockTransport.getRunState).toHaveBeenCalledWith('run-500');
    expect(mockTransport.getEventsSince).toHaveBeenCalledWith('run-500', 2);

    expect(lastSnapshot).toBeDefined();
    expect(lastSnapshot?.run?.status).toBe('completed');
    expect(lastSnapshot?.run?.lastSequence).toBe(5);
    expect(lastSnapshot?.run?.completedTools.length).toBe(1);
    expect(lastSnapshot?.run?.completedTools[0].toolName).toBe('file_read');
    expect(lastSnapshot?.run?.completedTools[0].result).toBe('package main');
  });
});
