package memory

import (
	"context"
	"time"
)

// Gate 判定一段工作是否已被认领过，用于保证「同一个 run 只回填抽取一次」。
//
// 为什么要它：一个 run 可能被收尾多次——用户取消、崩溃后恢复重放、重复的
// finish 调用。没有这道闸，同一次对话会被反复抽取，长期记忆里就会出现同一件
// 事的多个副本，而记忆的重复比遗漏更难清理。生产实现由 tool_idempotency 表
// 的原子 Claim 提供（见 bootstrap 的 memoryGate 适配）。
type Gate interface {
	// Claim 原子认领 key。key 不存在时返回 true；已被认领且未过期时返回 false。
	Claim(ctx context.Context, key string) (claimed bool, err error)
}

// Remember 投递一次回填。它必须不阻塞：调用点在 run 的收尾路径上，记忆回填
// 失败或服务慢都不该拖住 run 终态落盘。
//
// 队列满时丢弃并计数——丢一条记忆是可接受的，阻塞 run 收尾不是。
func (s *Service) Remember(turn Turn) {
	if s == nil || s.queue == nil || !s.cfg.Active() || !s.cfg.WriteBack {
		return
	}
	if turn.RunID == "" || turn.Prompt == "" {
		return
	}
	select {
	case s.queue <- turn:
		s.enqueued.Add(1)
	case <-s.closed:
		// 正在关停，丢弃是预期行为，不必报警。
	default:
		s.dropped.Add(1)
		s.logWarn(context.Background(), "长期记忆回填队列已满，本轮记忆被丢弃", nil)
	}
}

// runWriteBack 是回填 worker 的主循环。
func (s *Service) runWriteBack() {
	defer s.wg.Done()
	for {
		select {
		case <-s.closed:
			return
		case turn := <-s.queue:
			s.extract(turn)
		}
	}
}

// extract 认领并执行一次抽取。
func (s *Service) extract(turn Turn) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ExtractTimeout)
	defer cancel()

	if s.gate != nil {
		claimed, err := s.gate.Claim(ctx, ExtractionKey(turn.RunID))
		if err != nil {
			s.failed.Add(1)
			s.logWarn(ctx, "长期记忆回填去重失败，本轮跳过", err)
			return
		}
		if !claimed {
			s.skipped.Add(1)
			return
		}
	}

	msgs := turn.Messages()
	// 少于两条消息不算「一轮完成」：只有提问没有答复时，唯一能抽取出来的「事实」
	// 就是用户问过什么，那不是记忆，是噪声。收尾路径上传入的 turn 已经要求答复
	// 非空，这里是第二道闸——它守的是「这个特性无论被谁调用都不会写入半轮对话」。
	if len(msgs) < 2 {
		s.skipped.Add(1)
		return
	}
	metadata := map[string]any{"source": "ximo-agent"}
	if turn.SessionID != "" {
		metadata["session_id"] = turn.SessionID
	}
	if _, err := s.backend.Add(ctx, msgs, AddOptions{RunID: turn.RunID, Metadata: metadata}); err != nil {
		s.failed.Add(1)
		s.logWarn(ctx, "长期记忆回填失败，本轮记忆未落库", err)
		return
	}
	s.done.Add(1)
}

// extractTimeout 保证即使配置里把超时改得比召回还短，抽取也仍有一段合理的时间
// 去完成（mem0 侧要调一次 LLM 做抽取，与召回的「一次检索」不是一个量级）。
func (s *Service) extractTimeout() time.Duration {
	if s.cfg.ExtractTimeout <= 0 {
		return DefaultExtractTimeout
	}
	return s.cfg.ExtractTimeout
}
