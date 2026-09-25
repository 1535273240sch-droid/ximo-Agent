package agent

import (
	"context"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// Compactor decides *when* to compact the context; the actual compaction
// mechanics live in task 06's ports.ContextManager.
//
// Splitting the decision from the mechanics is deliberate: the four-tier
// ladder (none/soft/snip/compact/force) is a policy that must be identical for
// every provider, whereas how a summary is produced is provider-specific.
type Compactor struct {
	cfg types.AgentConfig
	// manager performs the compaction. Nil disables compaction entirely, which
	// is how a test runs the loop without a context manager.
	manager ports.ContextManager
}

// NewCompactor builds a compactor from the agent configuration.
func NewCompactor(cfg types.AgentConfig, manager ports.ContextManager) *Compactor {
	return &Compactor{cfg: cfg, manager: manager}
}

// Decision is the outcome of the pre-round compaction check.
type Decision struct {
	// Tier is the level the usage ratio falls into.
	Tier types.CompactionTier
	// Ratio is prompt tokens over the context window.
	Ratio float64
	// ShouldRun reports whether the loop should actually invoke the manager.
	// The soft tier is advisory only: it notifies but never rewrites the
	// prefix, because rewriting destroys the provider's prompt cache.
	ShouldRun bool
	// NotifyOnly is true for the soft tier.
	NotifyOnly bool
	// Reason explains the decision for the event payload.
	Reason string
}

// Decide classifies current usage and decides whether to compact.
//
// Two v1 behaviours are reproduced deliberately:
//
//   - The soft tier only notifies. Modifying the prefix to save tokens would
//     cost more in cache misses than it saves.
//   - Once compaction has failed to reduce usage twice in a row, it latches
//     stuck and stops trying. Continuing to compact a window that cannot be
//     reduced would burn a model call every round for no benefit.
func (c *Compactor) Decide(conv *Conversation) Decision {
	if c == nil || conv == nil {
		return Decision{Tier: types.TierNone, Reason: "no conversation"}
	}
	ratio := conv.UsageRatio(c.cfg.ContextWindow)
	tier := c.cfg.TierFor(ratio)
	d := Decision{Tier: tier, Ratio: ratio}

	if conv.CompactionStuck {
		d.Reason = "compaction is stuck; the prefix is left to grow append-only"
		return d
	}
	if c.manager == nil {
		d.Reason = "no context manager configured"
		return d
	}

	switch tier {
	case types.TierNone:
		d.Reason = "usage below soft threshold"
	case types.TierSoft:
		d.NotifyOnly = true
		if conv.SoftNoticed {
			d.Reason = "soft threshold already reported"
			return d
		}
		d.Reason = "soft threshold crossed; notifying without rewriting the prefix"
	case types.TierSnip:
		d.ShouldRun = true
		d.Reason = "snipping old tool output"
	case types.TierCompact:
		d.ShouldRun = true
		d.Reason = "compacting the middle region with a summary"
	case types.TierForce:
		d.ShouldRun = true
		d.Reason = "forcing compaction past the economics check"
	}
	return d
}

// Apply runs the decision, updating the conversation in place. It reports
// whether it changed anything.
//
// The stuck latch is maintained here rather than in the engine because it is a
// property of the compaction policy: two consecutive ineffective compactions
// mean the window cannot be reduced, so further attempts are pointless.
func (c *Compactor) Apply(ctx context.Context, conv *Conversation) (bool, error) {
	d := c.Decide(conv)
	if d.NotifyOnly {
		conv.SoftNoticed = true
		return false, nil
	}
	if !d.ShouldRun {
		return false, nil
	}

	before := conv.Usage.PromptTokens
	res, err := c.manager.Compact(ctx, ports.ContextSession{
		RunID:         "",
		SessionID:     "",
		Messages:      append([]ports.Message(nil), conv.Messages...),
		Usage:         conv.Usage,
		Window:        c.cfg.ContextWindow,
		Tier:          d.Tier,
		ProtectRecent: c.cfg.RecentKeep,
	})
	if err != nil {
		return false, types.WrapError(types.CodeOf(err), err, "context compaction failed")
	}

	if len(res.Messages) > 0 {
		conv.Messages = res.Messages
	}
	if res.Usage.TotalTokens > 0 {
		conv.Usage = res.Usage
	}

	// Maintain the stuck latch: an ineffective compaction increments the
	// counter, an effective one resets it.
	if res.Stuck || conv.Usage.PromptTokens >= before {
		conv.ConsecutiveCompacts++
		if conv.ConsecutiveCompacts >= 2 {
			conv.CompactionStuck = true
		}
	} else {
		conv.ConsecutiveCompacts = 0
		// A successful compaction re-opens the soft-tier notification, since
		// usage will climb through the threshold again.
		conv.SoftNoticed = false
	}
	return true, nil
}

// Thresholds exposes the derived trigger ratios, for the event payload and for
// tests asserting the ladder matches the documentation.
func (c *Compactor) Thresholds() types.CompactionThresholds { return c.cfg.Thresholds() }
