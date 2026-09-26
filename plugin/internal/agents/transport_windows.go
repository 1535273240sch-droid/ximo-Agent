//go:build windows

package agents

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 本文件是 Windows 命名管道客户端。主仓库 internal/ipc/transport_windows.go
// 里有一份等价的实现（它同时还要做服务端 Listen），这里只需要客户端方向：
// 插件永远是连过去的一方，不需要创建管道。
//
// 为什么要自己调 Win32 而不是用 net.Dial("npipe", ...)：标准库没有 npipe
// 传输层，而 ximo-agent 的默认端点就是 `\\.\pipe\ximo-agent-ipc`。

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procWaitNamedPipeW      = kernel32.NewProc("WaitNamedPipeW")
	procReadFile            = kernel32.NewProc("ReadFile")
	procWriteFile           = kernel32.NewProc("WriteFile")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procResetEvent          = kernel32.NewProc("ResetEvent")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procDisconnectNamedPipe = kernel32.NewProc("DisconnectNamedPipe")
)

const (
	fileFlagOverlapped = 0x40000000

	errorPipeBusy    = 231
	errorIoPending   = 997
	errorBrokenPipe  = 109
	errorHandleEOF   = 38
	waitTimeout      = 258
	waitInfinite     = 0xFFFFFFFF
	eventManualReset = 1
)

// dialPipe 连接命名管道，直到成功或 ctx 结束。
//
// WaitNamedPipeW 只用于「等管道出现」，不改变 CreateFile 的语义；管道还不存在
// 时 CreateFile 会立刻返回 ERROR_FILE_NOT_FOUND，所以要自己重试。
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return nil, lastErr
		default:
		}

		// 50ms 的等待让「管道存在但实例全忙」的情况不至于白转一圈。
		procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(pathPtr)), uintptr(50))

		handle, openErr := syscall.CreateFile(
			pathPtr,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0,
			nil,
			syscall.OPEN_EXISTING,
			fileFlagOverlapped,
			0,
		)
		if openErr == nil {
			return newPipeConn(handle, path)
		}
		lastErr = openErr

		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-ticker.C:
		}
	}
}

// pipeConn 把命名管道句柄包成 net.Conn（字节流）。
//
// 管道以 FILE_FLAG_OVERLAPPED 打开，因此每次读写都带 OVERLAPPED + 事件对象，
// 由 WaitForSingleObject 施加截止时间：没有这一层，读一个不再回话的对端会永久
// 阻塞。
type pipeConn struct {
	handle    syscall.Handle
	pipePath  string
	readEvent syscall.Handle
	wrEvent   syscall.Handle

	readMu  sync.Mutex
	writeMu sync.Mutex
	closeMu sync.Mutex
	closed  bool

	deadlineMu    sync.RWMutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func newPipeConn(handle syscall.Handle, path string) (*pipeConn, error) {
	rEvt, _, _ := procCreateEventW.Call(0, eventManualReset, 0, 0)
	wEvt, _, _ := procCreateEventW.Call(0, eventManualReset, 0, 0)
	if syscall.Handle(rEvt) == 0 || syscall.Handle(wEvt) == 0 {
		_ = syscall.CloseHandle(handle)
		_ = syscall.CloseHandle(syscall.Handle(rEvt))
		_ = syscall.CloseHandle(syscall.Handle(wEvt))
		return nil, errors.New("ipc: CreateEvent failed for named pipe I/O")
	}
	return &pipeConn{
		handle:    handle,
		pipePath:  path,
		readEvent: syscall.Handle(rEvt),
		wrEvent:   syscall.Handle(wEvt),
	}, nil
}

func (c *pipeConn) Read(b []byte) (int, error) {
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
	h, evt := c.handle, c.readEvent
	c.closeMu.Unlock()

	c.deadlineMu.RLock()
	dl := c.readDeadline
	c.deadlineMu.RUnlock()

	n, err := c.overlapped(h, evt, func(overlapped *syscall.Overlapped) (uint32, syscall.Errno) {
		var done uint32
		r1, _, errSys := procReadFile.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&b[0])),
			uintptr(len(b)),
			uintptr(unsafe.Pointer(&done)),
			uintptr(unsafe.Pointer(overlapped)),
		)
		if r1 != 0 {
			return done, 0
		}
		errno, _ := errSys.(syscall.Errno)
		return 0, errno
	}, dl)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (c *pipeConn) Write(b []byte) (int, error) {
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
	h, evt := c.handle, c.wrEvent
	c.closeMu.Unlock()

	c.deadlineMu.RLock()
	dl := c.writeDeadline
	c.deadlineMu.RUnlock()

	// 管道模式下 WriteFile 对 64KiB 以内的请求是原子的，但一旦返回的是
	// 「部分写入」或被中断，短写会让帧边界错位（对端会读到半个帧头）。所以
	// 这里循环补齐：帧协议建立在字节流之上，写满整段是调用方的责任。
	written := 0
	for written < len(b) {
		n, err := c.overlapped(h, evt, func(overlapped *syscall.Overlapped) (uint32, syscall.Errno) {
			var done uint32
			r1, _, errSys := procWriteFile.Call(
				uintptr(h),
				uintptr(unsafe.Pointer(&b[written])),
				uintptr(len(b)-written),
				uintptr(unsafe.Pointer(&done)),
				uintptr(unsafe.Pointer(overlapped)),
			)
			if r1 != 0 {
				return done, 0
			}
			errno, _ := errSys.(syscall.Errno)
			return 0, errno
		}, dl)
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
		written += int(n)
	}
	return written, nil
}

// overlapped 执行一次带 OVERLAPPED 的同步式调用：立即完成就直接返回，
// ERROR_IO_PENDING 则等事件（或截止时间），再取结果。
func (c *pipeConn) overlapped(
	h syscall.Handle,
	evt syscall.Handle,
	call func(*syscall.Overlapped) (uint32, syscall.Errno),
	dl time.Time,
) (uint32, error) {
	if !dl.IsZero() && time.Now().After(dl) {
		return 0, os.ErrDeadlineExceeded
	}

	procResetEvent.Call(uintptr(evt))

	var overlapped syscall.Overlapped
	overlapped.HEvent = evt

	done, errno := call(&overlapped)
	if errno == 0 {
		return done, nil
	}
	if errno != errorIoPending {
		return 0, pipeIOError(errno)
	}

	waitMillis := uint32(waitInfinite)
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

	waitRes, _, _ := procWaitForSingleObject.Call(uintptr(evt), uintptr(waitMillis))
	if waitRes == uintptr(waitTimeout) {
		procCancelIoEx.Call(uintptr(h), uintptr(unsafe.Pointer(&overlapped)))
		return 0, os.ErrDeadlineExceeded
	}

	var transferred uint32
	res, _, errWait := procGetOverlappedResult.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&overlapped)),
		uintptr(unsafe.Pointer(&transferred)),
		0, // 事件已通知，不阻塞
	)
	if res == 0 {
		werr, _ := errWait.(syscall.Errno)
		return 0, pipeIOError(werr)
	}
	return transferred, nil
}

// pipeIOError 把 Win32 错误码翻译成 Go 语义的错误。
func pipeIOError(errno syscall.Errno) error {
	switch errno {
	case errorBrokenPipe, errorHandleEOF:
		return io.EOF
	case errorPipeBusy:
		return errors.New("ipc: named pipe is busy (another instance is handling a client)")
	case 0:
		return nil
	}
	return errno
}

func (c *pipeConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true

	procCancelIoEx.Call(uintptr(c.handle), 0)
	procDisconnectNamedPipe.Call(uintptr(c.handle))
	_ = syscall.CloseHandle(c.handle)
	_ = syscall.CloseHandle(c.readEvent)
	_ = syscall.CloseHandle(c.wrEvent)
	return nil
}

func (c *pipeConn) LocalAddr() net.Addr  { return pipeAddr{c.pipePath} }
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr{c.pipePath} }

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

type pipeAddr struct{ path string }

func (a pipeAddr) Network() string { return "named-pipe" }
func (a pipeAddr) String() string  { return a.path }
