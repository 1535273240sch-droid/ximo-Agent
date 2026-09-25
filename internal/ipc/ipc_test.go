package ipc

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"testing"
	"time"
)

func TestFrameCodec(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	original := &Frame{
		Header: FrameHeader{
			Version:   1,
			RequestID: "req-123456",
			SessionID: "sess-abc-xyz",
			Sequence:  42,
			Type:      "test.action",
		},
		Payload: []byte("Hello XimoAgent IPC"),
	}
	original.Header.SetDeadline(now.Add(10 * time.Second))

	var buf bytes.Buffer
	if err := WriteFrame(&buf, original); err != nil {
		t.Fatalf("WriteFrame failed: %v", err)
	}

	decoded, err := ReadFrame(&buf, 1024*1024)
	if err != nil {
		t.Fatalf("ReadFrame failed: %v", err)
	}

	if decoded.Header.Version != original.Header.Version {
		t.Fatalf("version mismatch: got %d, want %d", decoded.Header.Version, original.Header.Version)
	}
	if decoded.Header.RequestID != original.Header.RequestID {
		t.Fatalf("request_id mismatch: got %s, want %s", decoded.Header.RequestID, original.Header.RequestID)
	}
	if decoded.Header.SessionID != original.Header.SessionID {
		t.Fatalf("session_id mismatch: got %s, want %s", decoded.Header.SessionID, original.Header.SessionID)
	}
	if decoded.Header.Sequence != original.Header.Sequence {
		t.Fatalf("sequence mismatch: got %d, want %d", decoded.Header.Sequence, original.Header.Sequence)
	}
	if decoded.Header.Type != original.Header.Type {
		t.Fatalf("type mismatch: got %s, want %s", decoded.Header.Type, original.Header.Type)
	}
	if decoded.Header.PayloadLength != uint32(len(original.Payload)) {
		t.Fatalf("payload length mismatch: got %d, want %d", decoded.Header.PayloadLength, len(original.Payload))
	}
	if !decoded.Header.Deadline().Equal(now.Add(10 * time.Second)) {
		t.Fatalf("deadline mismatch: got %v, want %v", decoded.Header.Deadline(), now.Add(10*time.Second))
	}
	if !bytes.Equal(decoded.Payload, original.Payload) {
		t.Fatalf("payload content mismatch: got %s, want %s", string(decoded.Payload), string(original.Payload))
	}
}

func TestPayloadLimitExceeded(t *testing.T) {
	hugePayload := make([]byte, 1024*1024) // 1MB
	frame := &Frame{
		Header: FrameHeader{
			Type: "test.huge",
		},
		Payload: hugePayload,
	}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame failed: %v", err)
	}

	// 限制只能读 512KB
	_, err := ReadFrame(&buf, 512*1024)
	if err == nil {
		t.Fatalf("expected ErrPayloadTooLarge error, got nil")
	}
}

func TestServerClientCommunication(t *testing.T) {
	// 使用 tcp 回环端点进行跨平台统一测试
	endpoint := fmt.Sprintf("tcp://127.0.0.1:%d", 20000+rand.Intn(10000))

	server := NewServer(endpoint, 1024*1024)
	server.RegisterHandler("calculator.add", func(ctx context.Context, req *Frame) (*Frame, error) {
		sum := fmt.Sprintf("Result-%s", string(req.Payload))
		return &Frame{
			Header: FrameHeader{
				Version:   req.Header.Version,
				RequestID: req.Header.RequestID,
				SessionID: req.Header.SessionID,
				Type:      "calculator.add.reply",
			},
			Payload: []byte(sum),
		}, nil
	})

	if err := server.Start(); err != nil {
		t.Fatalf("start server failed: %v", err)
	}
	defer server.Stop()

	client := NewClient(endpoint, 1024*1024, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("client connect failed: %v", err)
	}
	defer client.Close()

	// 1. 测试 Ping 心跳
	if err := client.Ping(2 * time.Second); err != nil {
		t.Fatalf("client ping failed: %v", err)
	}

	// 2. 测试 RPC 请求
	rpcReq := &Frame{
		Header: FrameHeader{
			Type: "calculator.add",
		},
		Payload: []byte("1+1=2"),
	}
	rpcResp, err := client.SendRequest(ctx, rpcReq)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	if string(rpcResp.Payload) != "Result-1+1=2" {
		t.Fatalf("unexpected RPC response: %s", string(rpcResp.Payload))
	}

	// 3. 测试广播与事件订阅
	eventReceived := make(chan *Frame, 1)
	client.Subscribe("engine.event.notify", func(f *Frame) {
		eventReceived <- f
	})

	broadcastFrame := &Frame{
		Header: FrameHeader{
			Type: "engine.event.notify",
		},
		Payload: []byte("Engine Started"),
	}
	server.Broadcast(broadcastFrame)

	select {
	case f := <-eventReceived:
		if string(f.Payload) != "Engine Started" {
			t.Fatalf("unexpected broadcast payload: %s", string(f.Payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("event subscriber timed out waiting for broadcast")
	}
}

func TestNamedPipeOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping named pipe test on non-windows platform")
	}

	pipeName := fmt.Sprintf(`\\.\pipe\test-ximo-pipe-%d`, time.Now().UnixNano())
	server := NewServer(pipeName, 512*1024)
	server.RegisterHandler("echo", func(ctx context.Context, req *Frame) (*Frame, error) {
		return &Frame{
			Header: FrameHeader{
				RequestID: req.Header.RequestID,
				Type:      "echo.ack",
			},
			Payload: req.Payload,
		}, nil
	})

	if err := server.Start(); err != nil {
		t.Fatalf("start named pipe server failed: %v", err)
	}
	defer server.Stop()

	client := NewClient(pipeName, 512*1024, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("connect named pipe failed: %v", err)
	}
	defer client.Close()

	if err := client.Ping(2 * time.Second); err != nil {
		t.Fatalf("ping named pipe failed: %v", err)
	}

	req := &Frame{
		Header: FrameHeader{
			Type: "echo",
		},
		Payload: []byte("named pipe data"),
	}
	resp, err := client.SendRequest(ctx, req)
	if err != nil {
		t.Fatalf("send request over named pipe failed: %v", err)
	}
		if string(resp.Payload) != "named pipe data" {
			t.Fatalf("payload over named pipe mismatch: %s", string(resp.Payload))
		}

		// 验证 B-1：测试 SetReadDeadline 超时保护
	rawConn, err := DialWindowsPipe(ctx, pipeName)
	if err != nil {
		t.Fatalf("dial for deadline test failed: %v", err)
	}
	defer rawConn.Close()

	_ = rawConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 16)
	_, err = rawConn.Read(buf)
	if err == nil {
		t.Fatalf("expected deadline exceeded error, got nil")
	}
	t.Logf("SetReadDeadline successfully aborted with error: %v", err)
}

func TestHandlerPanicRecovery(t *testing.T) {
	endpoint := fmt.Sprintf("tcp://127.0.0.1:%d", 28000+rand.Intn(2000))
	server := NewServer(endpoint, 1024*1024)
	server.RegisterHandler("crash.handler", func(ctx context.Context, req *Frame) (*Frame, error) {
		panic("deliberate panic inside rpc handler")
	})

	if err := server.Start(); err != nil {
		t.Fatalf("start server failed: %v", err)
	}
	defer server.Stop()

	client := NewClient(endpoint, 1024*1024, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer client.Close()

	req := &Frame{
		Header: FrameHeader{Type: "crash.handler"},
		Payload: []byte("test"),
	}

	_, err := client.SendRequest(ctx, req)
	if err == nil {
		t.Fatalf("expected error from panicked handler, got nil")
	}
}
