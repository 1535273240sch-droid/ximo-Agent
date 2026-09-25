package unit

import (
	"errors"
	"testing"
)

// Agent RunState 枚举定义（对照第4章）
type RunState string

const (
	StateCreated     RunState = "created"
	StateQueued      RunState = "queued"
	StatePlanning    RunState = "planning"
	StateThinking    RunState = "thinking"
	StateExecuting   RunState = "executing"
	StateCompacting  RunState = "compacting"
	StateWaitingUser RunState = "waiting_user"
	StateCompleted   RunState = "completed"
	StateCancelled   RunState = "cancelled"
	StateFailed      RunState = "failed"
	StateRecovering  RunState = "recovering"
)

// RunStateMachine 状态机模型
type RunStateMachine struct {
	Current RunState
	History []RunState
}

func NewRunStateMachine() *RunStateMachine {
	return &RunStateMachine{
		Current: StateCreated,
		History: []RunState{StateCreated},
	}
}

// Transition 状态流转规则校验
func (sm *RunStateMachine) Transition(next RunState) error {
	valid := false
	switch sm.Current {
	case StateCreated:
		valid = (next == StateQueued || next == StatePlanning || next == StateCancelled)
	case StateQueued:
		valid = (next == StatePlanning || next == StateThinking || next == StateCancelled)
	case StatePlanning:
		valid = (next == StateThinking || next == StateCancelled || next == StateFailed)
	case StateThinking:
		valid = (next == StateExecuting || next == StateCompacting || next == StateWaitingUser ||
			next == StateCompleted || next == StateCancelled || next == StateFailed || next == StateRecovering)
	case StateExecuting:
		valid = (next == StateThinking || next == StateWaitingUser || next == StateCancelled ||
			next == StateFailed || next == StateRecovering)
	case StateCompacting:
		valid = (next == StateThinking || next == StateFailed)
	case StateWaitingUser:
		valid = (next == StateThinking || next == StateCancelled)
	case StateRecovering:
		valid = (next == StateThinking || next == StateWaitingUser || next == StateFailed)
	case StateCompleted, StateCancelled, StateFailed:
		// 终态不可转移
		valid = false
	}

	if !valid {
		return errors.New("invalid state transition from " + string(sm.Current) + " to " + string(next))
	}

	sm.Current = next
	sm.History = append(sm.History, next)
	return nil
}

func TestRunStateMachineTransitions(t *testing.T) {
	sm := NewRunStateMachine()

	// 正常闭环：created -> queued -> planning -> thinking -> executing -> thinking -> completed
	steps := []RunState{
		StateQueued,
		StatePlanning,
		StateThinking,
		StateExecuting,
		StateThinking,
		StateCompleted,
	}

	for _, step := range steps {
		if err := sm.Transition(step); err != nil {
			t.Fatalf("unexpected transition error to %s: %v", step, err)
		}
	}

	// 终态不可继续转移
	if err := sm.Transition(StateThinking); err == nil {
		t.Fatalf("expected error transitioning from completed to thinking")
	}
}

func TestRunStateMachineRecoveryAndFailure(t *testing.T) {
	sm := NewRunStateMachine()
	_ = sm.Transition(StateQueued)
	_ = sm.Transition(StateThinking)
	_ = sm.Transition(StateExecuting)

	// 崩溃发生转移到 recovering
	if err := sm.Transition(StateRecovering); err != nil {
		t.Fatalf("transition to recovering failed: %v", err)
	}

	// 恢复后重新回到 thinking
	if err := sm.Transition(StateThinking); err != nil {
		t.Fatalf("transition from recovering to thinking failed: %v", err)
	}

	// 失败转移到 failed
	if err := sm.Transition(StateFailed); err != nil {
		t.Fatalf("transition to failed failed: %v", err)
	}
}
