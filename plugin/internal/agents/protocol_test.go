package agents

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestFrameGoldenBytes 用**手写的黄金字节**钉死线格式偏移。
//
// 这些期望值不是从本包实现里跑出来抄回去的，而是按主仓库
// internal/ipc/protocol.go 的注释布局（29 字节定长头 + type/reqid/sessid/payload）
// 手工算出来的；任何偏移、字节序、长度字段写错都会在这里红。
func TestFrameGoldenBytes(t *testing.T) {
	f := &Frame{
		Header: FrameHeader{
			Version:    1,
			Type:       "system.config.get",
			RequestID:  "r1",
			SessionID:  "s1",
			Sequence:   0x0102030405060708,
			DeadlineMs: 0x1122334455667788,
		},
		Payload: []byte(`{"ok":true}`),
	}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got := buf.Bytes()

	// 定长头 29 字节 + 元数据 17(type) + 2 + 2 + 载荷 11。
	wantLen := FixedHeaderSize + len("system.config.get") + 2 + 2 + len(`{"ok":true}`)
	if len(got) != wantLen {
		t.Fatalf("帧长度 = %d, want %d", len(got), wantLen)
	}

	checks := []struct {
		name string
		at   int
		want []byte
	}{
		{"magic", 0, []byte{0x58, 0x4D}},
		{"version", 2, []byte{0x01}},
		{"typeLen(be16)=17", 3, []byte{0x00, 0x11}},
		{"reqIDLen(be16)=2", 5, []byte{0x00, 0x02}},
		{"sessIDLen(be16)=2", 7, []byte{0x00, 0x02}},
		{"sequence(be64)", 9, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}},
		{"deadlineMs(be64)", 17, []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}},
		{"payloadLen(be32)=11", 25, []byte{0x00, 0x00, 0x00, 0x0B}},
	}
	for _, c := range checks {
		seg := got[c.at : c.at+len(c.want)]
		if !bytes.Equal(seg, c.want) {
			t.Errorf("%s @%d = % x, want % x", c.name, c.at, seg, c.want)
		}
	}

	// 可变段顺序必须是 type || requestID || sessionID，紧接着载荷。
	meta := got[FixedHeaderSize:]
	if string(meta[:17]) != "system.config.get" {
		t.Errorf("type 段 = %q", string(meta[:17]))
	}
	if string(meta[17:19]) != "r1" || string(meta[19:21]) != "s1" {
		t.Errorf("requestID/sessionID 段 = %q/%q", string(meta[17:19]), string(meta[19:21]))
	}
	if string(meta[21:]) != `{"ok":true}` {
		t.Errorf("载荷 = %q", string(meta[21:]))
	}

	// 载荷长度必须是**字节**长度，不是 rune 数（非 ASCII 时最容易写错）。
	u := &Frame{Header: FrameHeader{Type: "t", RequestID: "请求"}, Payload: []byte("载荷")}
	var ubuf bytes.Buffer
	if err := WriteFrame(&ubuf, u); err != nil {
		t.Fatalf("WriteFrame(utf8): %v", err)
	}
	ub := ubuf.Bytes()
	if got := binary.BigEndian.Uint16(ub[5:7]); got != 6 {
		t.Errorf("UTF-8 requestID 长度字段 = %d, want 6（字节数）", got)
	}
	if got := binary.BigEndian.Uint32(ub[25:29]); got != 6 {
		t.Errorf("UTF-8 载荷长度字段 = %d, want 6（字节数）", got)
	}
}

// TestFrameRoundTrip 走真实的「编码 → 字节流 → 解码」，并覆盖空字段边界。
func TestFrameRoundTrip(t *testing.T) {
	cases := []*Frame{
		{
			Header: FrameHeader{
				Version: 1, Type: TypeConfigSet, RequestID: "system.config.set-1",
				SessionID: "sess", Sequence: 42, DeadlineMs: 1700000000000,
			},
			Payload: []byte(`{"ok":true,"data":{"base_url":"http://127.0.0.1:8600"}}`),
		},
		{Header: FrameHeader{Type: "custom.no-payload"}, Payload: nil},
		{Header: FrameHeader{Type: "custom.empty-meta", RequestID: "", SessionID: ""}, Payload: []byte{}},
	}
	for i, in := range cases {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, in); err != nil {
			t.Fatalf("case %d WriteFrame: %v", i, err)
		}
		out, err := ReadFrame(&buf, 0)
		if err != nil {
			t.Fatalf("case %d ReadFrame: %v", i, err)
		}
		if out.Header.Type != in.Header.Type ||
			out.Header.RequestID != in.Header.RequestID ||
			out.Header.SessionID != in.Header.SessionID ||
			out.Header.Sequence != in.Header.Sequence ||
			out.Header.DeadlineMs != in.Header.DeadlineMs {
			t.Fatalf("case %d 头部不一致:\n got %+v\nwant %+v", i, out.Header, in.Header)
		}
		if !bytes.Equal(out.Payload, in.Payload) {
			t.Fatalf("case %d 载荷不一致: %q vs %q", i, out.Payload, in.Payload)
		}
		if out.Header.Version != CurrentVersion {
			t.Fatalf("case %d 版本 = %d, want %d（Version 为 0 时应填当前版本）", i, out.Header.Version, CurrentVersion)
		}
	}
}

// TestReadFrameRejectsMalformed 是反向控制：把帧头改坏后必须报错，而不是
// 解出一堆垃圾继续跑。
func TestReadFrameRejectsMalformed(t *testing.T) {
	base := func() []byte {
		var buf bytes.Buffer
		_ = WriteFrame(&buf, &Frame{
			Header:  FrameHeader{Type: TypeConfigGet, RequestID: "r", Sequence: 1},
			Payload: []byte(`{"ok":true}`),
		})
		return buf.Bytes()
	}

	t.Run("magic", func(t *testing.T) {
		b := base()
		b[0] = 0x00
		if _, err := ReadFrame(bytes.NewReader(b), 0); !errors.Is(err, ErrInvalidMagic) {
			t.Fatalf("err = %v, want ErrInvalidMagic", err)
		}
	})

	t.Run("version", func(t *testing.T) {
		b := base()
		b[2] = 2
		if _, err := ReadFrame(bytes.NewReader(b), 0); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
		}
	})

	t.Run("payload-too-large", func(t *testing.T) {
		b := base()
		binary.BigEndian.PutUint32(b[25:29], 1<<20)
		if _, err := ReadFrame(bytes.NewReader(b), 1024); !errors.Is(err, ErrPayloadTooLarge) {
			t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
		}
	})

	t.Run("truncated-payload", func(t *testing.T) {
		b := base()
		if _, err := ReadFrame(bytes.NewReader(b[:len(b)-3]), 0); err == nil {
			t.Fatal("载荷被截断却解析成功")
		}
	})

	t.Run("deadline-is-expired", func(t *testing.T) {
		var buf bytes.Buffer
		h := FrameHeader{Type: TypeConfigGet, RequestID: "r"}
		h.DeadlineMs = time.Now().Add(-time.Second).UnixMilli()
		_ = WriteFrame(&buf, &Frame{Header: h, Payload: []byte(`{}`)})
		f, err := ReadFrame(&buf, 0)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if f.Header.DeadlineMs <= 0 {
			t.Fatal("截止时间字段丢失")
		}
		if time.UnixMilli(f.Header.DeadlineMs).After(time.Now()) {
			t.Fatal("截止时间未被解成过去时刻")
		}
	})

	t.Run("empty-stream", func(t *testing.T) {
		if _, err := ReadFrame(bytes.NewReader(nil), 0); !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	})
}

// TestRedactMasksCredentials 锁死「凭据不进输出」这条红线里最基础的一环。
func TestRedactMasksCredentials(t *testing.T) {
	RegisterSecret("sk-live-abcdef123456")

	cases := []struct{ in, wantNotContain string }{
		{"connect failed: key sk-live-abcdef123456 rejected", "sk-live-abcdef123456"},
		{"Authorization: Bearer abcdef1234567890xyz rejected", "abcdef1234567890xyz"},
		{"api_key=sk-other-999999 invalid", "sk-other-999999"},
		{"password=hunter2secret rejected", "hunter2secret"},
		{"token: abcdef.ghijkl rejected", "abcdef.ghijkl"},
	}
	for _, c := range cases {
		got := redact(c.in)
		if strings.Contains(got, c.wantNotContain) {
			t.Errorf("redact(%q) = %q，仍包含凭据 %q", c.in, got, c.wantNotContain)
		}
		if !strings.Contains(got, "***") {
			t.Errorf("redact(%q) = %q，没有留下掩码标记", c.in, got)
		}
	}

	// 反向控制：不含凭据的普通错误必须原样保留（否则脱敏会把诊断信息吃光）。
	plain := "connect to \\\\.\\pipe\\ximo-agent-ipc failed: file not found"
	if got := redact(plain); got != plain {
		t.Errorf("普通错误被改写: %q", got)
	}
}

// TestEnvelopeDecode 覆盖面：ok=false 要变成错误，data 要能解出来。
func TestEnvelopeDecode(t *testing.T) {
	ok := `{"ok":true,"data":{"ref":"secretref:v1:00"}}`
	var env Envelope
	if err := json.Unmarshal([]byte(ok), &env); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Ref string `json:"ref"`
	}
	if err := env.Decode(&out); err != nil || out.Ref != "secretref:v1:00" {
		t.Fatalf("Decode = %v, ref = %q", err, out.Ref)
	}

	var bad Envelope
	if err := json.Unmarshal([]byte(`{"ok":false,"error":"安全存储写入失败"}`), &bad); err != nil {
		t.Fatal(err)
	}
	if err := bad.Decode(&out); err == nil || !strings.Contains(err.Error(), "安全存储写入失败") {
		t.Fatalf("ok=false 未变成错误: %v", err)
	}
}
