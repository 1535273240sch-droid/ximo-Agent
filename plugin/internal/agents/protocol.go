// Package agents 实现「需要写代码而不是改配置文件」的 Agent 适配。目前只有
// ximo-agent：它的服务商配置不落明文文件，而是经本地 IPC 写进平台安全存储
// （密钥只以 secret_ref 形式进配置）。
//
// # 为什么是自包含的复刻
//
// 契约（recon/插件契约-冻结.md §0.2）禁止 plugin 模块 import 主仓库的
// internal/**，而 ximo-agent 只认它自己的帧协议。因此本包的帧编解码是主仓库
// internal/ipc/protocol.go 的**独立复刻**：主仓库那份是唯一规格来源，本包不
// 反向影响它，也不与它共享代码。两侧的线格式必须逐字节一致，线格式布局：
//
//	[0..1]   Magic (0x58, 0x4D)
//	[2]      Version (1)
//	[3..4]   TypeLen      (uint16, BigEndian)
//	[5..6]   ReqIDLen     (uint16, BigEndian)
//	[7..8]   SessIDLen    (uint16, BigEndian)
//	[9..16]  Sequence     (uint64, BigEndian)
//	[17..24] DeadlineMs   (int64,  BigEndian; 0 = 无截止时间)
//	[25..28] PayloadLength(uint32, BigEndian)
//	[29..]   Type || RequestID || SessionID || Payload
//
// 偏移一旦改动就会与主仓库失配，因此 protocol_test.go 里有一组手写的黄金字节
// 用例钉住偏移；跨实现互操作用例见 interop_live_test.go。
//
// 本包只实现客户端方向（我们是被适配的一方，一律由插件连过去）。测试里的假
// 服务端复用同一套 WriteFrame/ReadFrame —— 编解码是共享的，收发方向不同。
package agents

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 帧头魔数，用于帧边界识别（"XM"）。
const (
	FrameMagic0          = 0x58
	FrameMagic1          = 0x4D
	CurrentVersion uint8 = 1
	// FixedHeaderSize 是定长帧头的字节数：魔数 2 + 版本 1 + 三个长度 6 +
	// 序号 8 + 截止时间 8 + 载荷长度 4。
	FixedHeaderSize = 29
	// DefaultMaxPayload 是读取时的载荷上限，与主仓库默认值一致（16 MiB）。
	DefaultMaxPayload uint32 = 16 * 1024 * 1024
)

var (
	ErrInvalidMagic       = errors.New("ipc: invalid frame magic")
	ErrUnsupportedVersion = errors.New("ipc: unsupported frame version")
	ErrPayloadTooLarge    = errors.New("ipc: payload exceeds maximum allowed size")
	ErrInvalidFrameHeader = errors.New("ipc: invalid frame header lengths")
)

// 本包用到的帧类型（取值与主仓库 internal/ipc/protocol.go 完全一致）。
const (
	TypeHeartbeat    = "system.heartbeat"
	TypeHeartbeatAck = "system.heartbeat_ack"
	// TypeError 是服务端在「处理器返回错误」或「未知帧类型」时回的帧类型，
	// 其载荷是**裸文本**而不是 {ok,...} 信封。
	TypeError = "system.error"

	TypeSecretPut    = "system.secret.put"
	TypeSecretStatus = "system.secret.status"
	TypeConfigGet    = "system.config.get"
	TypeConfigSet    = "system.config.set"
	TypeModelList    = "system.model.list"
)

// FrameHeader 是定长帧头 + 可变长元数据。
type FrameHeader struct {
	Version       uint8
	RequestID     string
	SessionID     string
	Sequence      uint64
	Type          string
	PayloadLength uint32
	// DeadlineMs 是毫秒级截止时间戳（Unix Epoch Ms，0 表示无截止时间）。
	DeadlineMs int64
}

// Frame 是完整的一帧。
type Frame struct {
	Header  FrameHeader
	Payload []byte
}

// WriteFrame 把 Frame 编码成线格式并写入 w。
//
// 与主仓库 WriteFrame 的差异只有一处：这里**不**回写 Header.PayloadLength
// （主仓库会顺手改调用方的结构体）。线格式完全相同。
func WriteFrame(w io.Writer, f *Frame) error {
	if f == nil {
		return errors.New("ipc: nil frame")
	}
	typeBytes := []byte(f.Header.Type)
	reqIDBytes := []byte(f.Header.RequestID)
	sessIDBytes := []byte(f.Header.SessionID)

	typeLen, reqLen, sessLen := len(typeBytes), len(reqIDBytes), len(sessIDBytes)
	if typeLen > 65535 || reqLen > 65535 || sessLen > 65535 {
		return ErrInvalidFrameHeader
	}

	version := f.Header.Version
	if version == 0 {
		version = CurrentVersion
	}

	payloadLen := uint32(len(f.Payload))
	buf := make([]byte, FixedHeaderSize+typeLen+reqLen+sessLen)

	buf[0] = FrameMagic0
	buf[1] = FrameMagic1
	buf[2] = version
	binary.BigEndian.PutUint16(buf[3:5], uint16(typeLen))
	binary.BigEndian.PutUint16(buf[5:7], uint16(reqLen))
	binary.BigEndian.PutUint16(buf[7:9], uint16(sessLen))
	binary.BigEndian.PutUint64(buf[9:17], f.Header.Sequence)
	binary.BigEndian.PutUint64(buf[17:25], uint64(f.Header.DeadlineMs))
	binary.BigEndian.PutUint32(buf[25:29], payloadLen)

	offset := FixedHeaderSize
	copy(buf[offset:offset+typeLen], typeBytes)
	offset += typeLen
	copy(buf[offset:offset+reqLen], reqIDBytes)
	offset += reqLen
	copy(buf[offset:offset+sessLen], sessIDBytes)

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("ipc: write frame header: %w", err)
	}
	if payloadLen > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("ipc: write frame payload: %w", err)
		}
	}
	return nil
}

// ReadFrame 从 r 读取并解码一个完整帧。maxPayloadLength 为 0 时用 DefaultMaxPayload。
func ReadFrame(r io.Reader, maxPayloadLength uint32) (*Frame, error) {
	if maxPayloadLength == 0 {
		maxPayloadLength = DefaultMaxPayload
	}

	fixed := make([]byte, FixedHeaderSize)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return nil, err
	}
	if fixed[0] != FrameMagic0 || fixed[1] != FrameMagic1 {
		return nil, ErrInvalidMagic
	}
	version := fixed[2]
	if version != CurrentVersion {
		return nil, fmt.Errorf("%w: got %d, expected %d", ErrUnsupportedVersion, version, CurrentVersion)
	}

	typeLen := int(binary.BigEndian.Uint16(fixed[3:5]))
	reqLen := int(binary.BigEndian.Uint16(fixed[5:7]))
	sessLen := int(binary.BigEndian.Uint16(fixed[7:9]))
	seq := binary.BigEndian.Uint64(fixed[9:17])
	deadlineMs := int64(binary.BigEndian.Uint64(fixed[17:25]))
	payloadLen := binary.BigEndian.Uint32(fixed[25:29])

	if payloadLen > maxPayloadLength {
		return nil, fmt.Errorf("%w: size %d > max %d", ErrPayloadTooLarge, payloadLen, maxPayloadLength)
	}

	meta := make([]byte, typeLen+reqLen+sessLen)
	if len(meta) > 0 {
		if _, err := io.ReadFull(r, meta); err != nil {
			return nil, fmt.Errorf("ipc: read frame meta: %w", err)
		}
	}
	msgType := string(meta[:typeLen])
	reqID := string(meta[typeLen : typeLen+reqLen])
	sessID := string(meta[typeLen+reqLen:])

	var payload []byte
	if payloadLen > 0 {
		payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("ipc: read frame payload: %w", err)
		}
	}

	return &Frame{
		Header: FrameHeader{
			Version:       version,
			RequestID:     reqID,
			SessionID:     sessID,
			Sequence:      seq,
			Type:          msgType,
			PayloadLength: payloadLen,
			DeadlineMs:    deadlineMs,
		},
		Payload: payload,
	}, nil
}
