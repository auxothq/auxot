package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/queue"
)

// enqueueJob serializes a job and adds it to the Redis job queue.
//
// ALL jobs go through Redis. Each connected GPU worker has its own consumer
// in the "workers" consumer group. Workers pull jobs via XREADGROUP in their
// own goroutine, so Redis handles FIFO distribution across consumers naturally.
//
// Flow: API handler → Redis XADD → worker's XREADGROUP goroutine → WebSocket → GPU
func enqueueJob(
	jobQueue *queue.JobQueue,
	jobMsg *protocol.JobMessage,
	logger *slog.Logger,
) error {
	data, err := json.Marshal(jobMsg)
	if err != nil {
		return fmt.Errorf("marshaling job for queue: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := jobQueue.Enqueue(ctx, jobMsg.JobID, data); err != nil {
		return fmt.Errorf("failed to enqueue job: %w", err)
	}

	logger.Info("job enqueued",
		"job_id", jobMsg.JobID,
	)
	return nil
}
