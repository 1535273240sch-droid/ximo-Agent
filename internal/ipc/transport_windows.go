//go:build windows

package ipc

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateNamedPipeW    = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe    = kernel32.NewProc("ConnectNamedPipe")
	procDisconnectNamedP    = kernel32.NewProc("DisconnectNamedPipe")
	procWaitNamedPipeW      = kernel32.NewProc("WaitNamedPipeW")
	procReadFile            = kernel32.NewProc("ReadFile")
	procWriteFile           = kernel32.NewProc("WriteFile")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procResetEvent          = kernel32.NewProc("ResetEvent")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
)

const (
	pipeAccessDuplex       = 0x00000003
	fileFlagOverlapped     = 0x40000000
	pipeTypeByte           = 0x00000000
	pipeReadModeByte       = 0x00000000
	pipeWait               = 0x00000000
	pipeUnlimitedInstances = 255

	errorPipeConnected = 535
	errorPipeBusy      = 231
	errorIoPending     = 997
	errorBrokenPipe    = 109
	errorHandleEOF     = 38

	waitObject0 = 0
	waitTimeout = 258
)

// WindowsNamedPipeTransport 实现了基于 Windows Named Pipe 的全双工 IPC 传输
type WindowsNamedPipeTransport struct{}

func (t *WindowsNamedPipeTransport) Listen(endpoint string) (net.Listener, error) {
	return NewWindowsPipeListener(endpoint)
}

func (t *WindowsNamedPipeTransport) Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	return DialWindowsPipe(ctx, endpoint)
}

// WindowsPipeListener Named Pipe 监听器
type WindowsPipeListener struct {
	pipePath  string
	closed    bool
	mu        sync.Mutex
	closeChan chan struct{}
}

func NewWindowsPipeListener(path string) (*WindowsPipeListener, error) {
	return &WindowsPipeListener{
		pipePath:  path,
		closeChan: make(chan struct{}),
	}, nil
}

func (l *WindowsPipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	path := l.pipePath
	l.mu.Unlock()

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, net.ErrClosed
		}
		l.mu.Unlock()

		h, _, err := procCreateNamedPipeW.Call(
			uintptr(unsafe.Pointer(pathPtr)),
			uintptr(pipeAccessDuplex|fileFlagOverlapped),
			uintptr(pipeTypeByte|pipeReadModeByte|pipeWait),
			uintptr(pipeUnlimitedInstances),
			uintptr(65536),
			uintptr(65536),
			uintptr(0),
			uintptr(0),
		)

		handle := syscall.Handle(h)
		if handle == syscall.InvalidHandle {
			return nil, fmt.Errorf("CreateNamedPipe failed: %v", err)
		}

		connEvt, _, _ := procCreateEventW.Call(0, 1, 0, 0)
		var connOverlapped syscall.Overlapped
		connOverlapped.HEvent = syscall.Handle(connEvt)

		r1, _, errSys := procConnectNamedPipe.Call(uintptr(handle), uintptr(unsafe.Pointer(&connOverlapped)))
		if r1 == 0 {
			errno, _ := errSys.(syscall.Errno)
			if errno == errorPipeConnected {
				// 已经连接成功
			} else if errno == errorIoPending {
				// 等待连接或者退出
				for {
					l.mu.Lock()
					isClosed := l.closed
					l.mu.Unlock()
					if isClosed {
						procCancelIoEx.Call(uintptr(handle), uintptr(unsafe.Pointer(&connOverlapped)))
						syscall.CloseHandle(syscall.Handle(connEvt))
						syscall.CloseHandle(handle)
						return nil, net.ErrClosed
					}

					var transferred uint32
					res, _, _ := procGetOverlappedResult.Call(
						uintptr(handle),
						uintptr(unsafe.Pointer(&connOverlapped)),
						uintptr(unsafe.Pointer(&transferred)),
						0, // 不阻塞轮询
					)
					if res != 0 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			} else {
				syscall.CloseHandle(syscall.Handle(connEvt))
				syscall.CloseHandle(handle)
				select {
				case <-l.closeChan:
					return nil, net.ErrClosed
				default:
					time.Sleep(10 * time.Millisecond)
					continue
				}
			}
		}

		syscall.CloseHandle(syscall.Handle(connEvt))
		return newPipeConn(handle, path)
	}
}

func (l *WindowsPipeListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.closeChan)
	return nil
}

func (l *WindowsPipeListener) Addr() net.Addr {
	return IPCAddr{
		NetworkName: "named-pipe",
		Address:     l.pipePath,
	}
}

// DialWindowsPipe 连接 Windows 命名管道
func DialWindowsPipe(ctx context.Context, path string) (net.Conn, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		h, _, _ := procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(pathPtr)), uintptr(50))
		_ = h

		handle, err := syscall.CreateFile(
			pathPtr,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0,
			nil,
			syscall.OPEN_EXISTING,
			fileFlagOverlapped,
			0,
		)
		if err == nil {
			return newPipeConn(handle, path)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

type pipeConn struct {
	handle     syscall.Handle
	pipePath   string
	readEvent  syscall.Handle
	writeEvent syscall.Handle
	readMu     sync.Mutex
	writeMu    sync.Mutex
	closeMu    sync.Mutex
	closed     bool

	deadlineMu    sync.RWMutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func newPipeConn(handle syscall.Handle, path string) (*pipeConn, error) {
	rEvt, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	wEvt, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	return &pipeConn{
		handle:     handle,
		pipePath:   path,
		readEvent:  syscall.Handle(rEvt),
		writeEvent: syscall.Handle(wEvt),
	}, nil
}

func (c *pipeConn) Read(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}

	c.readMu.Lock()
	defer c.readMu.Unlock()

	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return 0, net.ErrClosed
	}
	h := c.handle
	rEvt := c.readEvent
	c.closeMu.Unlock()

	c.deadlineMu.RLock()
	dl := c.readDeadline
	c.deadlineMu.RUnlock()

	if !dl.IsZero() && time.Now().After(dl) {
		return 0, os.ErrDeadlineExceeded
	}

	procResetEvent.Call(uintptr(rEvt))

	var overlapped syscall.Overlapped
	overlapped.HEvent = rEvt

	var done uint32
	r1, _, errSys := procReadFile.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
		uintptr(unsafe.Pointer(&done)),
		uintptr(unsafe.Pointer(&overlapped)),
	)

	if r1 == 0 {
		errno, _ := errSys.(syscall.Errno)
		if errno == errorIoPending {
			var waitMillis uint32 = 0xFFFFFFFF // INFINITE
			if !dl.IsZero() {
				remain := time.Until(dl)
				if remain <= 0 {
					procCancelIoEx.Call(uintptr(h), uintptr(unsafe.Pointer(&overlapped)))
					return 0, os.ErrDeadlineExceeded
				}
				waitMillis = uint32(remain.Milliseconds())
				if waitMillis == 0 {
					waitMillis = 1
				}
			}

			waitRes, _, _ := procWaitForSingleObject.Call(uintptr(rEvt), uintptr(waitMillis))
			if waitRes == uintptr(waitTimeout) {
				procCancelIoEx.Call(uintptr(h), uintptr(unsafe.Pointer(&overlapped)))
				return 0, os.ErrDeadlineExceeded
			}

			var transferred uint32
			res, _, errWait := procGetOverlappedResult.Call(
				uintptr(h),
				uintptr(unsafe.Pointer(&overlapped)),
				uintptr(unsafe.Pointer(&transferred)),
				0, // 此时事件已通知，直接获取
			)
			if res == 0 {
				wErr, _ := errWait.(syscall.Errno)
				if wErr == errorBrokenPipe || wErr == errorHandleEOF {
					return 0, io.EOF
				}
				return 0, errWait
			}
			return int(transferred), nil
		}
		if errno == errorBrokenPipe || errno == errorHandleEOF {
			return 0, io.EOF
		}
		return 0, errSys
	}

	return int(done), nil
}

func (c *pipeConn) Write(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return 0, net.ErrClosed
	}
	h := c.handle
	wEvt := c.writeEvent
	c.closeMu.Unlock()

	c.deadlineMu.RLock()
	dl := c.writeDeadline
	c.deadlineMu.RUnlock()

	if !dl.IsZero() && time.Now().After(dl) {
		return 0, os.ErrDeadlineExceeded
	}

	procResetEvent.Call(uintptr(wEvt))

	var overlapped syscall.Overlapped
	overlapped.HEvent = wEvt

	var written uint32
	r1, _, errSys := procWriteFile.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
		uintptr(unsafe.Pointer(&written)),
		uintptr(unsafe.Pointer(&overlapped)),
	)

	if r1 == 0 {
		errno, _ := errSys.(syscall.Errno)
		if errno == errorIoPending {
			var waitMillis uint32 = 0xFFFFFFFF // INFINITE
			if !dl.IsZero() {
				remain := time.Until(dl)
				if remain <= 0 {
					procCancelIoEx.Call(uintptr(h), uintptr(unsafe.Pointer(&overlapped)))
					return 0, os.ErrDeadlineExceeded
				}
				waitMillis = uint32(remain.Milliseconds())
				if waitMillis == 0 {
					waitMillis = 1
				}
			}

			waitRes, _, _ := procWaitForSingleObject.Call(uintptr(wEvt), uintptr(waitMillis))
			if waitRes == uintptr(waitTimeout) {
				procCancelIoEx.Call(uintptr(h), uintptr(unsafe.Pointer(&overlapped)))
				return 0, os.ErrDeadlineExceeded
			}

			var transferred uint32
			res, _, errWait := procGetOverlappedResult.Call(
				uintptr(h),
				uintptr(unsafe.Pointer(&overlapped)),
				uintptr(unsafe.Pointer(&transferred)),
				0,
			)
			if res == 0 {
				wErr, _ := errWait.(syscall.Errno)
				if wErr == errorBrokenPipe {
					return 0, io.EOF
				}
				return 0, errWait
			}
			return int(transferred), nil
		}
		if errno == errorBrokenPipe {
			return 0, io.EOF
		}
		return 0, errSys
	}

	return int(written), nil
}

func (c *pipeConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true

	procCancelIoEx.Call(uintptr(c.handle), 0)
	procDisconnectNamedP.Call(uintptr(c.handle))
	_ = syscall.CloseHandle(c.handle)
	_ = syscall.CloseHandle(c.readEvent)
	_ = syscall.CloseHandle(c.writeEvent)
	return nil
}

func (c *pipeConn) LocalAddr() net.Addr {
	return IPCAddr{NetworkName: "named-pipe", Address: c.pipePath}
}

func (c *pipeConn) RemoteAddr() net.Addr {
	return IPCAddr{NetworkName: "named-pipe", Address: c.pipePath}
}

func (c *pipeConn) SetDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	return nil
}

func (c *pipeConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadline = t
	return nil
}

func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.writeDeadline = t
	return nil
}
