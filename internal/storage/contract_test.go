package storage

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// TestEventMatchesRunEventsColumns 是 A1/D-4 裁决的可执行防线：
// Event 的字段与 JSON tag 必须与 run_events 列一一对应，且与 08 权威
// internal/types/event.go 的 Event 逐字一致（拼装时本地定义换成
// `type Event = types.Event` 是零字段改动的机械替换，任何一侧漂移都会
// 在这里失败，而不是在集成后静默丢字段）。
func TestEventMatchesRunEventsColumns(t *testing.T) {
	want := map[string]string{
		"RunID":       "run_id",
		"Seq":         "seq",
		"EventID":     "event_id",
		"Type":        "event_type",
		"PayloadJSON": "payload_json",
		"CreatedAt":   "created_at",
	}
	typ := reflect.TypeOf(Event{})
	if typ.NumField() != len(want) {
		t.Fatalf("Event has %d fields, want %d (must match run_events columns exactly)", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected field %q (not a run_events column)", f.Name)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("field %s json tag = %q, want %q", f.Name, got, tag)
		}
		if f.Type.Kind() != reflect.Uint64 && f.Type.Kind() != reflect.Int64 && f.Type.Kind() != reflect.String && f.Type.Kind() != reflect.Slice {
			t.Errorf("field %s has unexpected kind %s", f.Name, f.Type.Kind())
		}
	}
	// PayloadJSON 必须是 json.RawMessage（与权威类型一致，便于直接嵌进 JSON）。
	// 注意 Go 1.27 起 json.RawMessage 是 jsontext.Value 的别名，因此按
	// reflect.TypeOf 比较而不是按类型字符串。
	f, _ := typ.FieldByName("PayloadJSON")
	if f.Type != reflect.TypeOf(json.RawMessage{}) {
		t.Errorf("PayloadJSON type = %s, want json.RawMessage", f.Type)
	}
}

// TestEventRoundTripsThroughJSON 验证同一 struct 能同时服务 append 与
// replay 路径（裁决 A1 的理由）：JSON tag 与列名相同意味着不需要转换层。
func TestEventRoundTripsThroughJSON(t *testing.T) {
	ev := Event{
		EventID:     "e1",
		RunID:       "r1",
		Seq:         7,
		Type:        "tool.completed",
		PayloadJSON: json.RawMessage(`{"ok":true}`),
		CreatedAt:   1700000000000,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, ev) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", back, ev)
	}
	// tag 与列名一致：序列化后的键就是 run_events 的列名
	for _, key := range []string{"run_id", "seq", "event_id", "event_type", "payload_json", "created_at"} {
		if !containsKey(raw, key) {
			t.Errorf("serialized event missing key %q (must equal the column name)", key)
		}
	}
}

func containsKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// TestSinceNHasExplicitDefaultLimit 对应审核建议 S-1：SinceN 不允许
// "无限拉取"，limit<=0 必须解析到显式上限。
func TestSinceNHasExplicitDefaultLimit(t *testing.T) {
	if DefaultSinceLimit <= 0 {
		t.Fatal("DefaultSinceLimit must be positive")
	}
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	log := NewEventLog(store.DB())
	for i := 0; i < 5; i++ {
		if _, err := log.Append(ctx, runID, "t", []byte("{}")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// limit=0 → 默认上限（>0），不是无限
	events, err := log.SinceN(ctx, runID, 0, 0)
	if err != nil {
		t.Fatalf("SinceN: %v", err)
	}
	if len(events) != 5 {
		t.Errorf("SinceN(0) = %d events, want 5", len(events))
	}
	// 超过上限的 limit 被钳制
	if _, err := log.SinceN(ctx, runID, 0, DefaultSinceLimit+1); err != nil {
		t.Fatalf("SinceN(huge): %v", err)
	}
	// 契约方法 Since 保持不截断
	all, err := log.Since(ctx, runID, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("Since = %d events, want 5", len(all))
	}
}
