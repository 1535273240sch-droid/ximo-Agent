package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// Service 是长期记忆的门面：引擎拿它召回与回填，工具层拿它读写记忆。
//
// 它同时是引擎侧 MemoryPort 的实现：
//
//	Recall(ctx, query) string — 读，永不报错
//	Remember(turn)            — 写，永不阻塞
//
// 这两个签名是刻意设计的：调用点分别在 run 的入口与收尾路径上，那里没有
// 「记忆出问题该怎么办」的答案，所以答案必须是「什么也不做，继续跑」。
type Service struct {
	cfg    Config
	client *Client
	gate   Gate

	queue  chan Turn
	closed chan struct{}

	closeOnce sync.Once
	wg        sync.WaitGroup

	recallCalls  atomic.Uint64
	recallHits   atomic.Uint64
	recallErrors atomic.Uint64
	recallChars  atomic.Uint64

	enqueued atomic.Uint64
	done     atomic.Uint64
	failed   atomic.Uint64
	skipped  atomic.Uint64
	dropped  atomic.Uint64
}

// NewService 装配长期记忆服务。
//
// client 为 nil 或配置未启用时返回一个「空服务」而不是错误：调用方（bootstrap）
// 不需要为「用户没开记忆」写分支，服务自己会把所有调用变成 no-op。
func NewService(cfg Config, client *Client, gate Gate) *Service {
	cfg = cfg.WithDefaults()
	s := &Service{
		cfg:    cfg,
		client: client,
		gate:   gate,
		closed: make(chan struct{}),
	}
	if !cfg.Active() || client == nil {
		return s
	}
	if cfg.WriteBack {
		s.queue = make(chan Turn, cfg.QueueDepth)
		s.wg.Add(cfg.MaxInflight)
		for i := 0; i < cfg.MaxInflight; i++ {
			go s.runWriteBack()
		}
	}
	return s
}

// Enabled 报告召回是否可用。
func (s *Service) Enabled() bool { return s != nil && s.client != nil && s.cfg.Active() }

// Config 返回生效配置。
func (s *Service) Config() Config {
	if s == nil {
		return Config{}
	}
	return s.cfg
}

// Ping 对 mem0 服务做一次带鉴权的健康检查（供设置页「测试连接」使用）。
func (s *Service) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return ErrDisabled
	}
	return s.client.Ping(ctx)
}

// Close 停止回填 worker。可重复调用。
//
// 等待是有界的：正在进行的抽取最多占用 ExtractTimeout，进程退出不该被记忆回填
// 拖住。等待超时后仍有 goroutine 在跑是可接受的——它写的是外部服务的记忆，
// 不影响本进程的一致性。
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.queue == nil {
			return
		}
		waited := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(s.extractTimeout()):
		}
	})
}

// Stats 返回运行计数快照。
func (s *Service) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	stats := Stats{
		RecallCalls:      s.recallCalls.Load(),
		RecallHits:       s.recallHits.Load(),
		RecallErrors:     s.recallErrors.Load(),
		RecallChars:      s.recallChars.Load(),
		BackfillEnqueued: s.enqueued.Load(),
		BackfillDone:     s.done.Load(),
		BackfillFailed:   s.failed.Load(),
		BackfillSkipped:  s.skipped.Load(),
		BackfillDropped:  s.dropped.Load(),
		Enabled:          s.Enabled(),
		Endpoint:         s.cfg.Endpoint,
	}
	select {
	case <-s.closed:
		stats.Closed = true
	default:
	}
	if s.client != nil {
		stats.LastError, stats.LastErrorAt = s.client.LastError()
	}
	return stats
}

// ---------------------------------------------------------------------------
// 工具层用的读写门面（memory 工具的动作直接落到这几个方法上）
// ---------------------------------------------------------------------------

// Search 检索记忆。
func (s *Service) Search(ctx context.Context, query string, topK int) ([]Record, error) {
	if s == nil || s.client == nil {
		return nil, ErrDisabled
	}
	return s.client.Search(ctx, query, SearchOptions{TopK: topK})
}

// Add 直接写入一段文本（工具路径，不经引擎收尾）。
func (s *Service) Add(ctx context.Context, msgs []Message, opts AddOptions) ([]Record, error) {
	if s == nil || s.client == nil {
		return nil, ErrDisabled
	}
	return s.client.Add(ctx, msgs, opts)
}

// GetAll 列出记忆。
func (s *Service) GetAll(ctx context.Context, topK int) ([]Record, error) {
	if s == nil || s.client == nil {
		return nil, ErrDisabled
	}
	return s.client.GetAll(ctx, topK)
}

// Delete 删除一条记忆。
func (s *Service) Delete(ctx context.Context, id string) error {
	if s == nil || s.client == nil {
		return ErrDisabled
	}
	return s.client.Delete(ctx, id)
}

// logWarn 记录一条告警。错误细节在 Client 侧已经脱敏（密钥不会出现在这里）。
func (s *Service) logWarn(ctx context.Context, msg string, err error) {
	fields := map[string]any{"endpoint": s.cfg.Endpoint}
	if err != nil {
		fields["err"] = err.Error()
	}
	observability.LogWarn(ctx, msg, fields)
}
