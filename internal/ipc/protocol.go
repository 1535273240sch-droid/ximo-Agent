package ipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Magic 字节头，用于帧协议边界识别 ("XM")
const (
	FrameMagic0          = 0x58
	FrameMagic1          = 0x4D
	CurrentVersion uint8 = 1
)

var (
	ErrInvalidMagic       = errors.New("ipc: invalid frame magic")
	ErrUnsupportedVersion = errors.New("ipc: unsupported frame version")
	ErrPayloadTooLarge    = errors.New("ipc: payload exceeds maximum allowed size")
	ErrFrameTimeout       = errors.New("ipc: frame deadline exceeded")
	ErrInvalidFrameHeader = errors.New("ipc: invalid frame header lengths")
)

// 常用帧类型常量（对齐 v1 9个子模块与 v2 核心生命周期）
const (
	TypeHeartbeat    = "system.heartbeat"
	TypeHeartbeatAck = "system.heartbeat_ack"
	TypePing         = "system.ping"
	TypePong         = "system.pong"
	TypeError        = "system.error"
	TypeGracefulStop = "system.graceful_stop"
	TypeRunSubmit    = "engine.run.submit"
	TypeRunCancel    = "engine.run.cancel"
	TypeRunResume    = "engine.run.resume"
	TypeRunStatus    = "engine.run.status"
	TypeEventStream  = "engine.event.stream"
	// 计划模式（任务4）：用户对已生成计划的确认/否决。
	TypePlanConfirm   = "engine.plan.confirm"
	TypeWorkerHealth  = "worker.health"
	TypeWorkerStop    = "worker.stop"
	TypeWorkerExecute = "worker.execute"
	// 密钥与配置管理（第21章）：密钥只经此写入安全存储，读取接口只回是否已配置。
	TypeSecretPut    = "system.secret.put"
	TypeSecretStatus = "system.secret.status"
	TypeConfigGet    = "system.config.get"
	TypeConfigSet    = "system.config.set"
	// 模型列表：向服务商的 /models 端点查询可用模型，供界面下拉选择。
	TypeModelList = "system.model.list"
)

// FrameHeader 帧头结构体（完全对齐任务01规范中提供给 02 和 05 的契约）
// 第44-51行原文：
//
//	type FrameHeader struct {
//	    Version       uint8
//	    RequestID     string
//	    SessionID     string
//	    Sequence      uint64
//	    Type          string
//	    PayloadLength uint32
//	}
type FrameHeader struct {
	Version       uint8
	RequestID     string
	SessionID     string
	Sequence      uint64
	Type          string
	PayloadLength uint32
	// DeadlineMs 毫秒级截止时间戳（Unix Epoch Ms，0 表示无截止时间）
	DeadlineMs int64
}

// Frame 包含头部和载荷的完整帧
type Frame struct {
	Header  FrameHeader
	Payload []byte
}

// Deadline 返回时间形式的截止时间
func (h *FrameHeader) Deadline() time.Time {
	if h.DeadlineMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(h.DeadlineMs)
}

// SetDeadline 设置截止时间
func (h *FrameHeader) SetDeadline(t time.Time) {
	if t.IsZero() {
		h.DeadlineMs = 0
	} else {
		h.DeadlineMs = t.UnixMilli()
	}
}

// IsExpired 判断该帧是否已超过截止时间
func (h *FrameHeader) IsExpired() bool {
	if h.DeadlineMs <= 0 {
		return false
	}
	return time.Now().UnixMilli() > h.DeadlineMs
}

// SequenceGenerator 提供线程安全的单调递增序列号生成器
type SequenceGenerator struct {
	seq uint64
}

func NewSequenceGenerator(initial uint64) *SequenceGenerator {
	return &SequenceGenerator{seq: initial}
}

func (s *SequenceGenerator) Next() uint64 {
	return atomic.AddUint64(&s.seq, 1)
}

func (s *SequenceGenerator) Current() uint64 {
	return atomic.LoadUint64(&s.seq)
}

// WriteFrame 将 Frame 编码并写入输出流
// Wire 编码布局:
// [0..1]   Magic (2 字节: 0x58, 0x4D)
// [2]      Version (1 字节)
// [3..4]   TypeLen (2 字节, BigEndian)
// [5..6]   ReqIDLen (2 字节, BigEndian)
// [7..8]   SessIDLen (2 字节, BigEndian)
// [9..16]  Sequence (8 字节, BigEndian)
// [17..24] DeadlineMs (8 字节, BigEndian)
// [25..28] PayloadLength (4 字节, BigEndian)
// [29..]   Type bytes + RequestID bytes + SessionID bytes + Payload bytes
func WriteFrame(w io.Writer, f *Frame) error {
	typeBytes := []byte(f.Header.Type)
	reqIDBytes := []byte(f.Header.RequestID)
	sessIDBytes := []byte(f.Header.SessionID)

	typeLen := len(typeBytes)
	reqLen := len(reqIDBytes)
	sessLen := len(sessIDBytes)

	if typeLen > 65535 || reqLen > 65535 || sessLen > 65535 {
		return ErrInvalidFrameHeader
	}

	f.Header.PayloadLength = uint32(len(f.Payload))
	version := f.Header.Version
	if version == 0 {
		version = CurrentVersion
	}

	const fixedHeaderSize = 29
	buf := make([]byte, fixedHeaderSize+typeLen+reqLen+sessLen)

	buf[0] = FrameMagic0
	buf[1] = FrameMagic1
	buf[2] = version
	binary.BigEndian.PutUint16(buf[3:5], uint16(typeLen))
	binary.BigEndian.PutUint16(buf[5:7], uint16(reqLen))
	binary.BigEndian.PutUint16(buf[7:9], uint16(sessLen))
	binary.BigEndian.PutUint64(buf[9:17], f.Header.Sequence)
	binary.BigEndian.PutUint64(buf[17:25], uint64(f.Header.DeadlineMs))
	binary.BigEndian.PutUint32(buf[25:29], f.Header.PayloadLength)

	offset := fixedHeaderSize
	copy(buf[offset:offset+typeLen], typeBytes)
	offset += typeLen
	copy(buf[offset:offset+reqLen], reqIDBytes)
	offset += reqLen
	copy(buf[offset:offset+sessLen], sessIDBytes)

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write frame header failed: %w", err)
	}

	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("write frame payload failed: %w", err)
		}
	}

	return nil
}

// ReadFrame 从输入流中读取并解码一个完整帧，限制最大载荷大小
func ReadFrame(r io.Reader, maxPayloadLength uint32) (*Frame, error) {
	const fixedHeaderSize = 29
	fixedBuf := make([]byte, fixedHeaderSize)

	if _, err := io.ReadFull(r, fixedBuf); err != nil {
		return nil, err
	}

	if fixedBuf[0] != FrameMagic0 || fixedBuf[1] != FrameMagic1 {
		return nil, ErrInvalidMagic
	}

	version := fixedBuf[2]
	if version != CurrentVersion {
		return nil, fmt.Errorf("%w: got %d, expected %d", ErrUnsupportedVersion, version, CurrentVersion)
	}

	typeLen := binary.BigEndian.Uint16(fixedBuf[3:5])
	reqLen := binary.BigEndian.Uint16(fixedBuf[5:7])
	sessLen := binary.BigEndian.Uint16(fixedBuf[7:9])
	seq := binary.BigEndian.Uint64(fixedBuf[9:17])
	deadlineMs := int64(binary.BigEndian.Uint64(fixedBuf[17:25]))
	payloadLen := binary.BigEndian.Uint32(fixedBuf[25:29])

	if maxPayloadLength > 0 && payloadLen > maxPayloadLength {
		return nil, fmt.Errorf("%w: size %d > max %d", ErrPayloadTooLarge, payloadLen, maxPayloadLength)
	}

	metaLen := int(typeLen) + int(reqLen) + int(sessLen)
	metaBuf := make([]byte, metaLen)
	if metaLen > 0 {
		if _, err := io.ReadFull(r, metaBuf); err != nil {
			return nil, fmt.Errorf("read frame meta failed: %w", err)
		}
	}

	offset := 0
	typeStr := string(metaBuf[offset : offset+int(typeLen)])
	offset += int(typeLen)
	reqIDStr := string(metaBuf[offset : offset+int(reqLen)])
	offset += int(reqLen)
	sessIDStr := string(metaBuf[offset : offset+int(sessLen)])

	var payload []byte
	if payloadLen > 0 {
		payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("read frame payload failed: %w", err)
		}
	}

	f := &Frame{
		Header: FrameHeader{
			Version:       version,
			RequestID:     reqIDStr,
			SessionID:     sessIDStr,
			Sequence:      seq,
			Type:          typeStr,
			PayloadLength: payloadLen,
			DeadlineMs:    deadlineMs,
		},
		Payload: payload,
	}

	return f, nil
}
