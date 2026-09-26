package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

// 本文件接线缺陷 D6：`quota.Service.ReapExpired` 此前没有任何调用方，
// 而网关的对账安全性依赖它——结算失败时**故意不释放预占**（避免双重退款，
// 见 api/openai/charge.go 的 settle 失败路径），那些 held 预占只能靠过期回收兜底。
// 没有回收协程时，它们会永久占住用户额度。

// reservationReaper 是回收协程需要的最小能力（消费方定义窄接口，契约 §10.2）：
// *quota.Service 的方法签名逐字一致，可直接传入；测试用 fake 替换。
type reservationReaper interface {
	// ReapExpired 回收超时未结算的 held 预占，返回回收条数。
	// now <= 0 取当前时钟，limit <= 0 取 quota.DefaultReapLimit。
	ReapExpired(ctx context.Context, now int64, limit int) (int, error)
}

// reaper 周期回收超时未结算的 held 预占。
type reaper struct {
	svc      reservationReaper
	interval time.Duration
	limit    int
	logger   *observability.Logger
}

// effectiveReapLimit 把「未配置」的 limit 归一成 quota 包的默认上限：
// 真实上限只由 quota 包决定，本包不另立一套数值。
func effectiveReapLimit(limit int) int {
	if limit <= 0 {
		return quota.DefaultReapLimit
	}
	return limit
}

// startReaper 启动后台回收协程，返回「停等」函数：它取消派生 context 并**等待**
// 协程真正退出。调用方必须在 runWithContext 返回前调用它（defer 即可）——协程
// 既不该在 Run 之后继续跑，更不该在 sqlite.DB 关闭之后才去动库。
// 停等函数可重复调用（sync.Once）。
//
// interval <= 0（--reap-interval 0）表示显式关闭回收：不启动协程，返回空操作。
// svc 为 nil 时同样不启动（装配错误不该在后台 nil 解引用）。
func startReaper(ctx context.Context, svc reservationReaper, interval time.Duration, limit int, logger *observability.Logger) func() {
	if svc == nil || interval <= 0 {
		return func() {}
	}
	r := &reaper{svc: svc, interval: interval, limit: effectiveReapLimit(limit), logger: logger}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	go func() {
		defer close(done)
		r.loop(runCtx)
	}()
	return stop
}

// loop 按 interval 周期回收，直到 ctx 取消（优雅停机路径）。
func (r *reaper) loop(ctx context.Context) {
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.reapOnce(ctx)
		}
	}
}

// reapOnce 回收一轮。回收是兜底路径，失败绝不能影响转发或让进程退出：
// 出错只记 Warn，下个周期自然重试（ReapExpired 幂等，重复回收同一行不会二次入账）。
func (r *reaper) reapOnce(ctx context.Context) {
	n, err := r.safeReap(ctx)
	if err != nil {
		r.warn(ctx, map[string]any{
			"err":      err.Error(),
			"interval": r.interval.String(),
		})
		return
	}
	if n > 0 {
		// 只有真回收到了才记 Info：空闲时每分钟一行会淹掉日志。
		r.info(ctx, map[string]any{"reaped": n, "limit": r.limit})
	}
}

// safeReap 把一次回收包成「不 panic」的调用：后台协程里未恢复的 panic 会直接
// 打死整个网关进程，而回收只是兜底任务，没有任何理由为此终止服务。
func (r *reaper) safeReap(ctx context.Context) (n int, err error) {
	defer func() {
		if p := recover(); p != nil {
			n = 0
			err = fmt.Errorf("回收协程 panic（已恢复，进程继续）: %v", p)
		}
	}()
	return r.svc.ReapExpired(ctx, 0, r.limit)
}

// 日志字段名避开 observability 的敏感键名，值里也不含任何凭据。
func (r *reaper) warn(ctx context.Context, fields map[string]any) {
	if r.logger == nil {
		observability.LogWarn(ctx, "ximo-gateway: 回收超时预占失败（下个周期重试）", fields)
		return
	}
	r.logger.Warn(ctx, "ximo-gateway: 回收超时预占失败（下个周期重试）", fields)
}

func (r *reaper) info(ctx context.Context, fields map[string]any) {
	if r.logger == nil {
		observability.LogInfo(ctx, "ximo-gateway: 已回收超时未结算的预占", fields)
		return
	}
	r.logger.Info(ctx, "ximo-gateway: 已回收超时未结算的预占", fields)
}
