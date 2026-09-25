package procguard

import (
	"bytes"
	"sync"
)

// limitBuffer 是一个有上限的字节缓冲，用来替代 v1 里"把全部 stdout 追加进字符串"的写法。
//
// 为什么必须限流：v1 的 TerminalExecTool 只在进程退出后才截断，运行期间无限累积，
// 一条失控日志（几 GB）就能把宿主打爆。这里在写入路径上就设上界，
// 并且**读满上限后仍然继续读**（丢弃多余数据），保证子进程不会因管道写满而死锁。
//
// 保留策略：
//   - HeadBytes<=0 且 TailBytes<=0：只保留前 limit 字节（默认）。
//   - HeadBytes>0 且 TailBytes>0：保留前 Head 字节 + 后 Tail 字节，中间插省略标记。
type limitBuffer struct {
	mu       sync.Mutex
	limit    int64
	head     int64
	tail     int64
	headBuf  bytes.Buffer
	tailBuf  *ringBuffer
	total    int64
	trunc    bool
	onWrite  func([]byte)
	overflow bool // 已进入"丢弃中段"状态
}

// 省略标记，让调用方（以及 LLM）明确知道输出被截断。
const truncationMarker = "\n...(输出被截断)\n"

func newLimitBuffer(limit, head, tail int64, onWrite func([]byte)) *limitBuffer {
	if limit <= 0 {
		limit = DefaultMaxOutputBytes
	}
	b := &limitBuffer{limit: limit, onWrite: onWrite}
	if head > 0 && tail > 0 {
		b.head = head
		b.tail = tail
		b.tailBuf = newRingBuffer(int(tail))
	} else {
		b.head = limit
	}
	return b
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)

	if b.onWrite != nil {
		// 回调也只在未超限时触发，避免把海量数据推给上层。
		if b.total <= b.limit {
			b.onWrite(p)
		}
	}

	if b.tailBuf != nil {
		// head + tail 模式。
		headLeft := b.head - int64(b.headBuf.Len())
		if headLeft > 0 {
			if headLeft >= int64(n) {
				b.headBuf.Write(p)
				return n, nil
			}
			b.headBuf.Write(p[:headLeft])
			p = p[headLeft:]
		}
		b.trunc = true
		b.tailBuf.Write(p)
		return n, nil
	}

	// 只保留头部模式。
	left := b.limit - int64(b.headBuf.Len())
	if left <= 0 {
		b.trunc = true
		return n, nil
	}
	if int64(n) <= left {
		b.headBuf.Write(p)
		return n, nil
	}
	b.headBuf.Write(p[:left])
	b.trunc = true
	return n, nil
}

// Bytes 返回受限于上限的内容（head+tail 模式会插入截断标记）。
func (b *limitBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tailBuf == nil {
		return append([]byte(nil), b.headBuf.Bytes()...)
	}
	out := make([]byte, 0, b.headBuf.Len()+b.tailBuf.Len()+len(truncationMarker))
	out = append(out, b.headBuf.Bytes()...)
	if b.tailBuf.Len() > 0 {
		out = append(out, truncationMarker...)
		out = append(out, b.tailBuf.Bytes()...)
	}
	return out
}

// Truncated 报告是否发生了截断。
func (b *limitBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trunc
}

// Total 返回累计写入字节数（未截断前的真实总量）。
func (b *limitBuffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// ringBuffer 是固定容量的字节缓冲（只保留最后 size 字节）。
//
// 实现选择"追加 + 裁剪"而不是真正的模运算环：写入量远大于容量时，
// 一次裁剪就能丢掉整段旧数据，摊还成本低，且代码不会在边界条件下出错。
type ringBuffer struct {
	buf  []byte
	size int
}

func newRingBuffer(size int) *ringBuffer {
	if size < 0 {
		size = 0
	}
	return &ringBuffer{size: size}
}

func (r *ringBuffer) Write(p []byte) {
	if r.size == 0 {
		return
	}
	if len(p) >= r.size {
		// 新数据本身已超过容量：只留它的最后 size 字节。
		r.buf = append(r.buf[:0], p[len(p)-r.size:]...)
		return
	}
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.size {
		r.buf = r.buf[len(r.buf)-r.size:]
	}
}

// Len 返回已缓存字节数。
func (r *ringBuffer) Len() int { return len(r.buf) }

// Bytes 按时间顺序返回缓存内容。
func (r *ringBuffer) Bytes() []byte { return append([]byte(nil), r.buf...) }
