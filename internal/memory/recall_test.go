package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlockEmptyInputsReturnEmpty(t *testing.T) {
	if got := Block(nil, 500); got != "" {
		t.Fatalf("没有记录时应返回空串，实际 %q", got)
	}
	// 内容全是空白的记录等同于没有内容：不能因为「有条目」就往上下文里塞一个空标题。
	if got := Block([]Record{{ID: "a", Memory: "   \n  "}}, 500); got != "" {
		t.Fatalf("空白记忆应被跳过，实际 %q", got)
	}
}

func TestBlockRendersHeaderAndNormalizesWhitespace(t *testing.T) {
	got := Block([]Record{{ID: "a", Memory: "用户偏好  暗色\n主题"}}, 500)
	if !strings.HasPrefix(got, BlockHeader) {
		t.Fatalf("缺少固定首行: %q", got)
	}
	if !strings.Contains(got, "- 用户偏好 暗色 主题") {
		t.Fatalf("应把换行折成单行并保留词间距: %q", got)
	}
}

func TestBlockDedupesAndSortsByScore(t *testing.T) {
	got := Block([]Record{
		{Memory: "低分", Score: 0.1},
		{Memory: "高分", Score: 0.9},
		{Memory: "高分", Score: 0.9},
	}, 500)
	if strings.Count(got, "高分") != 1 {
		t.Fatalf("重复记忆只应出现一次: %q", got)
	}
	if strings.Index(got, "高分") > strings.Index(got, "低分") {
		t.Fatalf("应按相关度降序: %q", got)
	}
}

func TestBlockRespectsBudget(t *testing.T) {
	// 填充词必须与固定首行（"--- 长期记忆 (mem0) ---"）里的字符不同，否则
	// "超预算条目被跳过" 的断言会被首行自己的字面量满足，测试永远为真。
	long := strings.Repeat("z", 400)
	// 单条超预算被跳过，后面的短条目仍应注入：预算不足时丢的是「最长的」，
	// 而不是「剩下的全部」。
	got := Block([]Record{
		{Memory: long, Score: 0.9},
		{Memory: "短", Score: 0.5},
	}, 80)
	if !strings.Contains(got, "短") {
		t.Fatalf("预算内的小条目应被保留: %q", got)
	}
	if strings.Contains(got, "zzz") {
		t.Fatalf("超预算条目应被跳过: %q", got)
	}
	// 一条都放不下时必须返回空串，让调用方完全不注入这条消息。
	if got := Block([]Record{{Memory: long}}, 80); got != "" {
		t.Fatalf("全部超预算时应返回空串，实际 %q", got)
	}
}

func TestServiceRecallDegradesInsteadOfFailing(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := NewService(Config{Enabled: true, Endpoint: srv.URL, Timeout: time.Second, WriteBack: false},
		NewClient(Config{Enabled: true, Endpoint: srv.URL, UserID: "u", Timeout: time.Second},
			ClientOptions{HTTPClient: srv.Client()}), nil)

	if got := svc.Recall(context.Background(), "随便问点什么"); got != "" {
		t.Fatalf("服务出错时应降级为空串，实际 %q", got)
	}
	stats := svc.Stats()
	if stats.RecallErrors != 1 || stats.RecallHits != 0 {
		t.Fatalf("计数不符: %+v", stats)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("应只请求一次，实际 %d", hits)
	}
}

func TestServiceRecallSkipsWithoutQueryOrWhenDisabled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"results":[{"id":"m","memory":"x"}]}`))
	}))
	defer srv.Close()

	enabled := Config{Enabled: true, Endpoint: srv.URL, UserID: "u", Timeout: time.Second}
	svc := NewService(enabled, NewClient(enabled, ClientOptions{HTTPClient: srv.Client()}), nil)

	if got := svc.Recall(context.Background(), "   "); got != "" {
		t.Fatalf("空查询不应注入任何内容，实际 %q", got)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("空查询不应发出请求")
	}

	// 未启用：连请求都不该发。
	off := NewService(Config{}, NewClient(Config{}, ClientOptions{HTTPClient: srv.Client()}), nil)
	if got := off.Recall(context.Background(), "q"); got != "" {
		t.Fatalf("未启用时应返回空串，实际 %q", got)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("未启用时不应发出请求")
	}
}

func TestServiceRecallInjectsBlockAndCountsChars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"m","memory":"用户偏好暗色主题","score":0.9}]}`))
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, Endpoint: srv.URL, UserID: "u", Timeout: time.Second}
	svc := NewService(cfg, NewClient(cfg, ClientOptions{HTTPClient: srv.Client()}), nil)

	got := svc.Recall(context.Background(), "主题偏好")
	if !strings.Contains(got, "用户偏好暗色主题") || !strings.HasPrefix(got, BlockHeader) {
		t.Fatalf("注入内容不符: %q", got)
	}
	stats := svc.Stats()
	if stats.RecallHits != 1 || stats.RecallChars != uint64(len(got)) || stats.RecallCalls != 1 {
		t.Fatalf("计数不符: %+v", stats)
	}
}
