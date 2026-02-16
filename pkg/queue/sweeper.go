package queue

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Sweeper periodically scans the PEL for dead consumers and reclaims their jobs.
//
// A consumer is "dead" if:
//  1. It has pending messages in the PEL
//  2. Its heartbeat key has expired
//
// Reclaimed messages are XCLAIMed to a live consumer so they stay at the front
// of the queue. The live consumer's ReadPending loop picks them up on its next
// iteration — no re-enqueue, no loss of position.
//
// If no live consumers exist, messages stay in the dead consumer's PEL until
// a worker connects. The sweeper will reclaim them on a future sweep.
type Sweeper struct {
	jobQueue  *JobQueue
	heartbeat *Heartbeat
	interval  time.Duration
	logger    *slog.Logger
}

// NewSweeper creates a dead-worker sweeper.
func NewSweeper(jobQueue *JobQueue, heartbeat *Heartbeat, interval time.Duration, logger *slog.Logger) *Sweeper {
	return &Sweeper{
		jobQueue:  jobQueue,
		heartbeat: heartbeat,
		interval:  interval,
		logger:    logger,
	}
}

// Run starts the sweep loop. Blocks until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	s.logger.Info("sweeper starting", "interval", s.interval.String())

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("sweeper stopping")
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep checks for dead consumers and reclaims their pending messages.
func (s *Sweeper) sweep(ctx context.Context) {
	// XPENDING summary: returns consumer list with pending counts
	pending, err := s.jobQueue.client.XPending(ctx, s.jobQueue.streamKey, s.jobQueue.groupName).Result()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Error("checking pending entries", "error", err)
		return
	}

	if pending.Count == 0 {
		return // No pending messages anywhere
	}

	// Separate alive and dead consumers from the pending summary.
	// In go-redis v9, Consumers is map[string]int64 (name → pending count).
	var deadConsumers []string
	for consumerName, pendingCount := range pending.Consumers {
		if pendingCount == 0 {
			continue
		}

		alive, err := s.heartbeat.IsAlive(ctx, consumerName)
		if err != nil {
			s.logger.Error("checking heartbeat", "consumer", consumerName, "error", err)
			continue
		}

		if alive {
			continue // Worker is alive, its pending messages are being processed
		}

		deadConsumers = append(deadConsumers, consumerName)
	}

	if len(deadConsumers) == 0 {
		return
	}

	// Find live consumers to transfer jobs to.
	// We scan heartbeat keys (not the XPENDING summary) because a live worker
	// with 0 pending messages wouldn't appear in the pending summary.
	liveWorkers, err := s.heartbeat.LiveWorkerIDs(ctx)
	if err != nil {
		s.logger.Error("discovering live workers", "error", err)
		return
	}

	if len(liveWorkers) == 0 {
		s.logger.Warn("dead consumers detected but no live workers to claim to",
			"dead_consumers", len(deadConsumers),
			"note", "messages stay pending until a worker connects",
		)
		return
	}

	// Reclaim from each dead consumer, distributing across live workers.
	for i, deadConsumer := range deadConsumers {
		s.logger.Warn("dead consumer detected",
			"consumer", deadConsumer,
			"pending", pending.Consumers[deadConsumer],
		)

		// Round-robin across live workers
		target := liveWorkers[i%len(liveWorkers)]
		s.reclaimFromConsumer(ctx, deadConsumer, target)
	}
}

// reclaimFromConsumer transfers all pending messages from a dead consumer
// to a live consumer via XCLAIM. The live consumer's ReadPending loop will
// find them on its next iteration and dispatch them immediately.
//
// This preserves queue position — jobs that were at the front stay at the front.
// Compare with the old approach of re-enqueueing (XADD), which sent them to
// the back of the queue.
func (s *Sweeper) reclaimFromConsumer(ctx context.Context, deadConsumer, targetConsumer string) {
	// Step 1: Get the pending message IDs for this specific consumer.
	entries, err := s.jobQueue.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream:   s.jobQueue.streamKey,
		Group:    s.jobQueue.groupName,
		Start:    "-",
		End:      "+",
		Count:    100,
		Consumer: deadConsumer,
	}).Result()
	if err != nil {
		s.logger.Error("listing pending messages for dead consumer",
			"consumer", deadConsumer,
			"error", err,
		)
		return
	}

	if len(entries) == 0 {
		// PEL is empty — safe to remove the dead consumer from the group.
		s.deleteConsumer(ctx, deadConsumer)
		return
	}

	// Step 2: XCLAIM those messages to the target live consumer.
	// This transfers PEL ownership — the messages are now "pending" for the
	// target consumer, and its ReadPending call will find them.
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}

	claimed, err := s.jobQueue.client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   s.jobQueue.streamKey,
		Group:    s.jobQueue.groupName,
		Consumer: targetConsumer,
		MinIdle:  0, // Claim regardless of idle time — we already verified the consumer is dead
		Messages: ids,
	}).Result()
	if err != nil {
		s.logger.Error("claiming messages from dead consumer",
			"dead_consumer", deadConsumer,
			"target_consumer", targetConsumer,
			"count", len(ids),
			"error", err,
		)
		return
	}

	s.logger.Info("reclaimed jobs from dead consumer",
		"dead_consumer", deadConsumer,
		"target_consumer", targetConsumer,
		"reclaimed", len(claimed),
	)

	// Step 3: Remove the dead consumer from the group.
	// Its PEL entries have been transferred to the target consumer.
	s.deleteConsumer(ctx, deadConsumer)
}

// deleteConsumer removes a consumer from the consumer group.
func (s *Sweeper) deleteConsumer(ctx context.Context, consumer string) {
	if err := s.jobQueue.client.XGroupDelConsumer(ctx,
		s.jobQueue.streamKey, s.jobQueue.groupName, consumer).Err(); err != nil {
		s.logger.Warn("removing dead consumer from group",
			"consumer", consumer,
			"error", err,
		)
	}
}
