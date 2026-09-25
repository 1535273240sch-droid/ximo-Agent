package unit

import (
	"sort"
	"testing"
)

type UIEvent struct {
	Seq       uint64
	EventType string
	Payload   string
}

type UIRunSnapshot struct {
	RunID        string
	Status       string
	LastSequence uint64
	EventLog     []string
}

func ReplayEvents(snapshot *UIRunSnapshot, events []UIEvent) {
	// 按 Seq 严格升序排序
	sort.Slice(events, func(i, j int) bool {
		return events[i].Seq < events[j].Seq
	})

	for _, evt := range events {
		// 防倒退与重复：只有大于当前最新 sequence 的事件才应用
		if evt.Seq <= snapshot.LastSequence {
			continue
		}
		snapshot.LastSequence = evt.Seq
		snapshot.EventLog = append(snapshot.EventLog, evt.Payload)

		switch evt.EventType {
		case "thinking":
			snapshot.Status = "thinking"
		case "tool_call":
			snapshot.Status = "executing"
		case "completed":
			snapshot.Status = "completed"
		case "failed":
			snapshot.Status = "failed"
		}
	}
}

func TestUIReplayAndSequenceIntegrity(t *testing.T) {
	snapshot := &UIRunSnapshot{
		RunID:        "run-101",
		Status:       "thinking",
		LastSequence: 10,
		EventLog:     []string{"init"},
	}

	// 模拟断线后拉取到的一批包含乱序与重复 seq 的事件列表
	missedEvents := []UIEvent{
		{Seq: 12, EventType: "tool_call", Payload: "call_tool_file_read"},
		{Seq: 10, EventType: "duplicate", Payload: "should_ignore"}, // seq <= 10 应忽略
		{Seq: 11, EventType: "thinking", Payload: "turn_2_thinking"},
		{Seq: 13, EventType: "completed", Payload: "run_done"},
	}

	ReplayEvents(snapshot, missedEvents)

	if snapshot.LastSequence != 13 {
		t.Fatalf("expected lastSequence 13, got %d", snapshot.LastSequence)
	}
	if snapshot.Status != "completed" {
		t.Fatalf("expected status completed, got %s", snapshot.Status)
	}
	if len(snapshot.EventLog) != 4 { // init + seq11 + seq12 + seq13
		t.Fatalf("expected 4 event log items, got %d: %+v", len(snapshot.EventLog), snapshot.EventLog)
	}
	if snapshot.EventLog[1] != "turn_2_thinking" || snapshot.EventLog[2] != "call_tool_file_read" {
		t.Fatalf("events not replayed in sequence order: %+v", snapshot.EventLog)
	}
}
