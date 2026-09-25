package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

var (
	ErrClientClosed    = errors.New("ipc: client is closed")
	ErrRequestTimeout  = errors.New("ipc: request timeout")
	ErrConnectionLost  = errors.New("ipc: connection lost")
	ErrDuplicateReqID  = errors.New("ipc: duplicate request id")
)

// Client 统一的 IPC 客户端基座，支持自动重连与请求/响应配对
type Client struct {
	endpoint         string
	maxPayloadLength uint32
	autoReconnect    bool

	connMu sync.Mutex
	conn   net.Conn

	sendMu sync.Mutex
	seqGen *SequenceGenerator

	reqMu   sync.Mutex
	pending map[string]chan *Frame

	subMu       sync.RWMutex
	subscribers map[string][]func(f *Frame)

	ctx        context.Context
	cancel     context.CancelFunc
	closedChan chan struct{}

	reconnectChan chan struct{}
}

// NewClient 创建 IPC 客户端
func NewClient(endpoint string, maxPayloadLength uint32, autoReconnect bool) *Client {
	if maxPayloadLength == 0 {
		maxPayloadLength = 16 * 1024 * 1024
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		endpoint:         endpoint,
		maxPayloadLength: maxPayloadLength,
		autoReconnect:    autoReconnect,
		seqGen:           NewSequenceGenerator(0),
		pending:          make(map[string]chan *Frame),
		subscribers:      make(map[string][]func(f *Frame)),
		ctx:              ctx,
		cancel:           cancel,
		closedChan:       make(chan struct{}),
		reconnectChan:    make(chan struct{}, 1),
	}
	return c
}

// Connect 建立连接并启动读循环与重连监控
func (c *Client) Connect(ctx context.Context) error {
	conn, err := DialIPC(ctx, c.endpoint, 5*time.Second)
	if err != nil {
		return fmt.Errorf("ipc client connect to %s failed: %w", c.endpoint, err)
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	go c.readLoop(conn)

	if c.autoReconnect {
		go c.reconnectLoop()
	}

	return nil
}

func (c *Client) readLoop(activeConn net.Conn) {
	defer func() {
		_ = activeConn.Close()
		c.connMu.Lock()
		if c.conn == activeConn {
			c.conn = nil
		}
		c.connMu.Unlock()

		// 触发重连
		if c.autoReconnect {
			select {
			case c.reconnectChan <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		frame, err := ReadFrame(activeConn, c.maxPayloadLength)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}

		// 优先匹配等待响应的 Pending Request
		if frame.Header.RequestID != "" {
			c.reqMu.Lock()
			ch, exists := c.pending[frame.Header.RequestID]
			if exists {
				delete(c.pending, frame.Header.RequestID)
			}
			c.reqMu.Unlock()

			if exists {
				select {
				case ch <- frame:
				default:
				}
				continue
			}
		}

		// 分发至对应类型的订阅者
		c.subMu.RLock()
		subs := c.subscribers[frame.Header.Type]
		subsAll := c.subscribers["*"]
		handlers := make([]func(f *Frame), 0, len(subs)+len(subsAll))
		handlers = append(handlers, subs...)
		handlers = append(handlers, subsAll...)
		c.subMu.RUnlock()

			for _, h := range handlers {
				handlerFunc := h
				go func() {
					defer func() {
						_ = recover() // 隔离单个 subscriber panic，避免影响整个客户端读循环
					}()
					handlerFunc(frame)
				}()
			}
	}
}

func (c *Client) reconnectLoop() {
	backoffSteps := []time.Duration{
		50 * time.Millisecond,
		200 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
	}
	stepIdx := 0

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.reconnectChan:
		}

		for {
			select {
			case <-c.ctx.Done():
				return
			default:
			}

			c.connMu.Lock()
			alreadyConnected := c.conn != nil
			c.connMu.Unlock()
			if alreadyConnected {
				stepIdx = 0
				break
			}

			delay := backoffSteps[stepIdx]
			timer := time.NewTimer(delay)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			if stepIdx < len(backoffSteps)-1 {
				stepIdx++
			}

			dialCtx, dialCancel := context.WithTimeout(c.ctx, 2*time.Second)
			conn, err := DialIPC(dialCtx, c.endpoint, 2*time.Second)
			dialCancel()

			if err == nil {
				c.connMu.Lock()
				c.conn = conn
				c.connMu.Unlock()
				stepIdx = 0
				go c.readLoop(conn)
				break
			}
		}
	}
}

// SendRequest 发送请求并阻塞等待响应（支持超时与 Context 取消）
func (c *Client) SendRequest(ctx context.Context, f *Frame) (*Frame, error) {
	select {
	case <-c.ctx.Done():
		return nil, ErrClientClosed
	default:
	}

	if f.Header.RequestID == "" {
		f.Header.RequestID = fmt.Sprintf("req-%d", c.seqGen.Next())
	}
	if f.Header.Sequence == 0 {
		f.Header.Sequence = c.seqGen.Next()
	}

	respChan := make(chan *Frame, 1)

	c.reqMu.Lock()
	if _, exists := c.pending[f.Header.RequestID]; exists {
		c.reqMu.Unlock()
		return nil, ErrDuplicateReqID
	}
	c.pending[f.Header.RequestID] = respChan
	c.reqMu.Unlock()

	defer func() {
		c.reqMu.Lock()
		delete(c.pending, f.Header.RequestID)
		c.reqMu.Unlock()
	}()

	if err := c.Send(f); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, ErrClientClosed
	case resp := <-respChan:
		if resp.Header.Type == TypeError {
			return resp, errors.New(string(resp.Payload))
		}
		return resp, nil
	}
}

// Send 单向发送 Frame
func (c *Client) Send(f *Frame) error {
	select {
	case <-c.ctx.Done():
		return ErrClientClosed
	default:
	}

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return ErrConnectionLost
	}

	if f.Header.Sequence == 0 {
		f.Header.Sequence = c.seqGen.Next()
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	return WriteFrame(conn, f)
}

// Subscribe 注册对特定消息类型的监听函数
func (c *Client) Subscribe(msgType string, handler func(f *Frame)) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.subscribers[msgType] = append(c.subscribers[msgType], handler)
}

// Ping 发送心跳探针并校验应答
func (c *Client) Ping(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req := &Frame{
		Header: FrameHeader{
			Type: TypeHeartbeat,
		},
		Payload: []byte("PING"),
	}

	resp, err := c.SendRequest(ctx, req)
	if err != nil {
		return err
	}
	if resp.Header.Type != TypeHeartbeatAck {
		return fmt.Errorf("unexpected heartbeat ack type: %s", resp.Header.Type)
	}
	return nil
}

// Close 优雅关闭客户端
func (c *Client) Close() error {
	c.cancel()

	select {
	case <-c.closedChan:
		return nil
	default:
		close(c.closedChan)
	}

	c.connMu.Lock()
	var err error
	if c.conn != nil {
		err = c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()

	c.reqMu.Lock()
	for reqID, ch := range c.pending {
		close(ch)
		delete(c.pending, reqID)
	}
	c.reqMu.Unlock()

	return err
}
