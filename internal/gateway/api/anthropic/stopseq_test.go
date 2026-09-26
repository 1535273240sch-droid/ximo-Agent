package anthropic

import "testing"

func TestFindStop(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		seqs      []string
		wantIdx   int
		wantMatch string
	}{
		{name: "无序列", text: "abc", seqs: nil, wantIdx: -1},
		{name: "命中", text: "abcENDdef", seqs: []string{"END"}, wantIdx: 3, wantMatch: "END"},
		{name: "取最先出现", text: "xxBxxAxx", seqs: []string{"A", "B"}, wantIdx: 2, wantMatch: "B"},
		{name: "同位置按入参顺序", text: "xxAB", seqs: []string{"AB", "A"}, wantIdx: 2, wantMatch: "AB"},
		{name: "空序列忽略", text: "abc", seqs: []string{""}, wantIdx: -1},
		{name: "多字节", text: "你好世界", seqs: []string{"世界"}, wantIdx: 6, wantMatch: "世界"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, match := findStop(tc.text, tc.seqs)
			if idx != tc.wantIdx || match != tc.wantMatch {
				t.Fatalf("期望 (%d,%q)，实际 (%d,%q)", tc.wantIdx, tc.wantMatch, idx, match)
			}
		})
	}
}

func TestStopFilterIncremental(t *testing.T) {
	type step struct {
		in          string
		wantEmit    string
		wantStopped bool
	}
	cases := []struct {
		name        string
		seqs        []string
		steps       []step
		wantFlush   string
		wantMatched string
	}{
		{
			name: "无停止序列时直通",
			steps: []step{
				{in: "hello", wantEmit: "hello"},
				{in: " world", wantEmit: " world"},
			},
		},
		{
			name: "单分片内命中",
			seqs: []string{"END"},
			steps: []step{
				{in: "headENDtail", wantStopped: true},
			},
			wantMatched: "END",
		},
		{
			name: "停止序列跨分片（滞留尾部）",
			seqs: []string{"END"},
			steps: []step{
				{in: "xxE", wantEmit: "x"},
				{in: "NDyy", wantStopped: true},
			},
			wantMatched: "END",
		},
		{
			name: "无命中时补齐滞留尾部",
			seqs: []string{"END"},
			steps: []step{
				{in: "hello", wantEmit: "hel"},
				{in: "lo", wantEmit: "lo"},
			},
			wantFlush: "lo",
		},
		{
			name: "多序列取最早命中",
			seqs: []string{"FFFF", "DD"},
			steps: []step{
				{in: "aDDb", wantStopped: true},
			},
			wantMatched: "DD",
		},
		{
			name: "空序列被忽略（不会立刻截断）",
			seqs: []string{"", "ZZ"},
			steps: []step{
				{in: "abc", wantEmit: "ab"},
			},
			wantFlush: "c",
		},
		{
			name: "多字节序列在字符边界处切分",
			seqs: []string{"世界"},
			steps: []step{
				{in: "你好"},
			},
			wantFlush: "你好",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopFilter(tc.seqs)
			for i, s := range tc.steps {
				emit, stopped := f.feed(s.in)
				if emit != s.wantEmit || stopped != s.wantStopped {
					t.Fatalf("第 %d 步期望 (%q,%v)，实际 (%q,%v)", i, s.wantEmit, s.wantStopped, emit, stopped)
				}
			}
			if got := f.flush(); got != tc.wantFlush {
				t.Fatalf("flush 期望 %q，实际 %q", tc.wantFlush, got)
			}
			if got := f.matchedSequence(); got != tc.wantMatched {
				t.Fatalf("matched 期望 %q，实际 %q", tc.wantMatched, got)
			}
			if f.stopped() != (tc.wantMatched != "") {
				t.Fatalf("stopped 期望 %v，实际 %v", tc.wantMatched != "", f.stopped())
			}
		})
	}
}

func TestStopFilterNilIsPassthrough(t *testing.T) {
	var f *stopFilter
	emit, stopped := f.feed("abc")
	if emit != "abc" || stopped {
		t.Fatalf("nil 过滤器应直通，实际 (%q,%v)", emit, stopped)
	}
	if f.flush() != "" || f.stopped() || f.matchedSequence() != "" {
		t.Fatalf("nil 过滤器的只读方法应返回零值")
	}
}

func TestRuneSafeCut(t *testing.T) {
	s := "你好" // 每个汉字 3 字节
	if got := runeSafeCut(s, 4); got != 3 {
		t.Fatalf("应回退到字符边界 3，实际 %d", got)
	}
	if got := runeSafeCut(s, 0); got != 0 {
		t.Fatalf("n=0 应返回 0，实际 %d", got)
	}
	if got := runeSafeCut(s, len(s)); got != len(s) {
		t.Fatalf("n=len 应原样返回，实际 %d", got)
	}
}
