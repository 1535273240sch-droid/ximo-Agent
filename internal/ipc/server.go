package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// HandlerFunc 处理接收到的特定类型帧
type HandlerFunc func(ctx context.Context, req *Frame) (*Frame, error)

// Server 统一的 IPC 服务端基座
type Server struct {
	endpoint         string
	maxPayloadLength uint32
	listener         net.Listener

	handlersMu sync.RWMutex
	handlers   map[string]HandlerFunc

	// forwarder 把本服务端未注册的帧代理给真正的业务进程（通常是 Engine）。
	// 为 nil 时未注册帧仍旧回 "unknown method"。
	forwarder FrameForwarder

	connsMu sync.Mutex
	conns   map[uint64]*serverConn
	connSeq uint64

	seqGen *SequenceGenerator
	sem    chan struct{} // 并发信号量限流，防止无界 goroutine 堆积 (S-2)

	ctx        context.Context
	cancel     context.CancelFunc
	closedChan chan struct{}
}

// FrameForwarder 把一帧转发给下游服务端并取回响应。
type FrameForwarder interface {
	Forward(ctx context.Context, frame *Frame) (*Frame, error)
}

// SetForwarder 注册未命中帧的转发器。必须在 Start 之前调用。
func (s *Server) SetForwarder(f FrameForwarder) {
	s.handlersMu.Lock()
	defer s.handlersMu.Unlock()
	s.forwarder = f
}

type serverConn struct {
	id     uint64
	conn   net.Conn
	sendMu sync.Mutex
	server *Server
}

// NewServer 创建 IPC 服务端实例
func NewServer(endpoint string, maxPayloadLength uint32) *Server {
	if maxPayloadLength == 0 {
		maxPayloadLength = 16 * 1024 * 1024 // 16MB
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		endpoint:         endpoint,
		maxPayloadLength: maxPayloadLength,
		handlers:         make(map[string]HandlerFunc),
		conns:            make(map[uint64]*serverConn),
		seqGen:           NewSequenceGenerator(0),
		sem:              make(chan struct{}, 1024),
		ctx:              ctx,
		cancel:           cancel,
		closedChan:       make(chan struct{}),
	}
}

// RegisterHandler 注册特定消息类型的处理函数
func (s *Server) RegisterHandler(msgType string, handler HandlerFunc) {
	s.handlersMu.Lock()
	defer s.handlersMu.Unlock()
	s.handlers[msgType] = handler
}

// Start 启动监听并开始接受客户端连接
func (s *Server) Start() error {
	l, err := ListenIPC(s.endpoint)
	if err != nil {
		return fmt.Errorf("ipc server listen on %s failed: %w", s.endpoint, err)
	}
	s.listener = l

	// 注册默认心跳处理
	s.RegisterHandler(TypeHeartbeat, func(ctx context.Context, req *Frame) (*Frame, error) {
		return &Frame{
			Header: FrameHeader{
				Version:   req.Header.Version,
				RequestID: req.Header.RequestID,
				SessionID: req.Header.SessionID,
				Sequence:  s.seqGen.Next(),
				Type:      TypeHeartbeatAck,
			},
			Payload: []byte("OK"),
		}, nil
	})

	// 探活是本服务端自己的职责，绝不转发。
	//
	// 客户端（UI）用它判断"当前连的这条链路还活着"。若把它转发给下游，
	// 一旦下游未响应就会把一次普通的探活变成错误，界面会误报"后端异常"，
	// 而实际上本地链路完全健康。
	s.RegisterHandler(TypePing, func(ctx context.Context, req *Frame) (*Frame, error) {
		return &Frame{
			Header: FrameHeader{
				Version:   req.Header.Version,
				RequestID: req.Header.RequestID,
				SessionID: req.Header.SessionID,
				Sequence:  s.seqGen.Next(),
				Type:      TypePong,
			},
			Payload: []byte("PONG"),
		}, nil
	})

	go s.acceptLoop()
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closedChan:
				return
			default:
				time.Sleep(20 * time.Millisecond)
				continue
			}
		}

		connID := atomic.AddUint64(&s.connSeq, 1)
		sc := &serverConn{
			id:     connID,
			conn:   conn,
			server: s,
		}

		s.connsMu.Lock()
		s.conns[connID] = sc
		s.connsMu.Unlock()

		go s.handleConn(sc)
	}
}

func (s *Server) handleConn(sc *serverConn) {
	defer func() {
		_ = sc.conn.Close()
		s.connsMu.Lock()
		delete(s.conns, sc.id)
		s.connsMu.Unlock()
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		req, err := ReadFrame(sc.conn, s.maxPayloadLength)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}

		// 检查帧 Deadline
		if req.Header.IsExpired() {
			errResp := &Frame{
				Header: FrameHeader{
					Version:   req.Header.Version,
					RequestID: req.Header.RequestID,
					SessionID: req.Header.SessionID,
					Sequence:  s.seqGen.Next(),
					Type:      TypeError,
				},
				Payload: []byte(ErrFrameTimeout.Error()),
			}
			_ = sc.writeFrame(errResp)
			continue
		}

		s.handlersMu.RLock()
		handler, exists := s.handlers[req.Header.Type]
		fwd := s.forwarder
		s.handlersMu.RUnlock()

		// 未注册的帧交给可选的转发器处理。
		//
		// 为什么需要这一层：业务真相在 Engine 进程里（数据库、密钥、模型调用），
		// 而 UI 连的是 Supervisor 的 IPC 端点。Supervisor 不该自己实现业务，
		// 而应把这类帧代理给 Engine。之前的实现直接回 "unknown method"，
		// 导致界面上所有依赖 Engine 的能力（例如保存 API 密钥）都不可用。
		if !exists {
			if fwd != nil {
				select {
				case s.sem <- struct{}{}:
				case <-s.ctx.Done():
					return
				}
				go func(frame *Frame) {
					defer func() {
						<-s.sem
						_ = recover()
					}()
					resp, ferr := fwd.Forward(s.ctx, frame)
					if ferr != nil || resp == nil {
						msg := "forward failed"
						if ferr != nil {
							msg = ferr.Error()
						}
						resp = &Frame{
							Header: FrameHeader{
								Version:   frame.Header.Version,
								RequestID: frame.Header.RequestID,
								SessionID: frame.Header.SessionID,
								Sequence:  s.seqGen.Next(),
								Type:      TypeError,
							},
							Payload: []byte(msg),
						}
					}
					_ = sc.writeFrame(resp)
				}(req)
				continue
			}

			errResp := &Frame{
				Header: FrameHeader{
					Version:   req.Header.Version,
					RequestID: req.Header.RequestID,
					SessionID: req.Header.SessionID,
					Sequence:  s.seqGen.Next(),
					Type:      TypeError,
				},
				Payload: []byte(fmt.Sprintf("unknown message type: %s", req.Header.Type)),
			}
			_ = sc.writeFrame(errResp)
			continue
		}

		select {
		case s.sem <- struct{}{}:
		case <-s.ctx.Done():
			return
		}

		go func(frame *Frame) {
			defer func() {
				<-s.sem
				if r := recover(); r != nil {
					errResp := &Frame{
						Header: FrameHeader{
							Version:   frame.Header.Version,
							RequestID: frame.Header.RequestID,
							SessionID: frame.Header.SessionID,
							Sequence:  s.seqGen.Next(),
							Type:      TypeError,
						},
						Payload: []byte(fmt.Sprintf("internal handler panic: %v", r)),
					}
					_ = sc.writeFrame(errResp)
				}
			}()

			resp, err := handler(s.ctx, frame)
			if err != nil {
				errResp := &Frame{
					Header: FrameHeader{
						Version:   frame.Header.Version,
						RequestID: frame.Header.RequestID,
						SessionID: frame.Header.SessionID,
						Sequence:  s.seqGen.Next(),
						Type:      TypeError,
					},
					Payload: []byte(err.Error()),
				}
				_ = sc.writeFrame(errResp)
				return
			}
			if resp != nil {
				if resp.Header.Sequence == 0 {
					resp.Header.Sequence = s.seqGen.Next()
				}
				_ = sc.writeFrame(resp)
			}
		}(req)
	}
}

func (sc *serverConn) writeFrame(f *Frame) error {
	sc.sendMu.Lock()
	defer sc.sendMu.Unlock()
	return WriteFrame(sc.conn, f)
}

// Broadcast 向所有在线客户端广播消息（对每个连接分配专属副本与序号，S-4）
func (s *Server) Broadcast(f *Frame) {
	s.connsMu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.connsMu.Unlock()

	for _, c := range conns {
		frameCopy := &Frame{
			Header:  f.Header,
			Payload: f.Payload,
		}
		frameCopy.Header.Sequence = s.seqGen.Next()
		_ = c.writeFrame(frameCopy)
	}
}

// Stop 优雅关闭 IPC 服务端
func (s *Server) Stop() error {
	s.cancel()
	select {
	case <-s.closedChan:
		return nil
	default:
		close(s.closedChan)
	}

	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}

	s.connsMu.Lock()
	for _, c := range s.conns {
		_ = c.conn.Close()
	}
	s.conns = make(map[uint64]*serverConn)
	s.connsMu.Unlock()

	return err
}
