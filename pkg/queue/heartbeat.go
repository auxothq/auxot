// Heartbeat provides Redis-backed worker liveness tracking.
//
// Each connected worker has a heartbeat key with a TTL. The router refreshes
// the key every tick. If the key expires, the worker is considered dead and
// a sweeper can reclaim its pending jobs.
//
// Key format:  {prefix}heartbeat:{workerID}
// Value:       "1" (existence is what matters)
// TTL:         configurable, typically 10s
package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Heartbeat manages per-worker heartbeat keys in Redis.
type Heartbeat struct {
	client *redis.Client
	prefix string // Key prefix (e.g., "auxot:")
	ttl    time.Duration
}

// NewHeartbeat creates a heartbeat manager.
// ttl is the key expiry — if a heartbeat isn't refreshed within this window,
// the worker is considered dead.
func NewHeartbeat(client *redis.Client, prefix string, ttl time.Duration) *Heartbeat {
	return &Heartbeat{
		client: client,
		prefix: prefix,
		ttl:    ttl,
	}
}

func (h *Heartbeat) key(workerID string) string {
	return h.prefix + "heartbeat:" + workerID
}

// Ping refreshes a worker's heartbeat. Call this on a ticker (e.g., every 5s).
func (h *Heartbeat) Ping(ctx context.Context, workerID string) error {
	if err := h.client.Set(ctx, h.key(workerID), "1", h.ttl).Err(); err != nil {
		return fmt.Errorf("refreshing heartbeat for %s: %w", workerID, err)
	}
	return nil
}

// IsAlive checks if a worker's heartbeat key exists.
func (h *Heartbeat) IsAlive(ctx context.Context, workerID string) (bool, error) {
	n, err := h.client.Exists(ctx, h.key(workerID)).Result()
	if err != nil {
		return false, fmt.Errorf("checking heartbeat for %s: %w", workerID, err)
	}
	return n > 0, nil
}

// Remove deletes a worker's heartbeat key (called on clean disconnect).
func (h *Heartbeat) Remove(ctx context.Context, workerID string) error {
	if err := h.client.Del(ctx, h.key(workerID)).Err(); err != nil {
		return fmt.Errorf("removing heartbeat for %s: %w", workerID, err)
	}
	return nil
}

// LiveWorkerIDs returns the IDs of all workers with active heartbeat keys.
//
// Uses SCAN (not KEYS) to avoid blocking Redis on large keyspaces.
// The returned IDs are workers that are definitely alive right now — their
// heartbeat TTL hasn't expired.
func (h *Heartbeat) LiveWorkerIDs(ctx context.Context) ([]string, error) {
	pattern := h.prefix + "heartbeat:*"
	prefixLen := len(h.prefix + "heartbeat:")
	var ids []string

	var cursor uint64
	for {
		keys, nextCursor, err := h.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scanning heartbeat keys: %w", err)
		}
		for _, key := range keys {
			if len(key) > prefixLen {
				ids = append(ids, key[prefixLen:])
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return ids, nil
}
