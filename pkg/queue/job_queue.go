// Package queue provides Redis Streams operations for job queuing and
// per-job token streaming.
//
// Two stream patterns:
//
//  1. Job Queue — a single stream with a consumer group. API requests XADD jobs,
//     GPU workers XREADGROUP to claim them, XACK+XDEL when done.
//
//  2. Token Stream — per-job ephemeral streams. GPU workers XADD tokens as they
//     arrive from llama.cpp, API callers XREAD to stream them as SSE.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// JobQueue manages the main job stream with consumer groups.
type JobQueue struct {
	client     *redis.Client
	streamKey  string
	groupName  string
}

// JobPayload is what gets enqueued. The Data field carries the full job
// details as JSON — this keeps the stream entries simple (one field).
type JobPayload struct {
	JobID string `json:"job_id"`
	Data  []byte `json:"data"` // JSON-encoded job details
}

// StreamEntry is a message read from a Redis stream.
type StreamEntry struct {
	ID      string // Redis stream ID (e.g., "1234567890-0")
	Payload JobPayload
}

// NewJobQueue creates a JobQueue targeting the given stream and consumer group.
func NewJobQueue(client *redis.Client, streamKey, groupName string) *JobQueue {
	return &JobQueue{
		client:    client,
		streamKey: streamKey,
		groupName: groupName,
	}
}

// NewBlockingReader creates a new JobQueue backed by a dedicated Redis
// connection (pool size 1). Use this for XREADGROUP BLOCK calls so the
// blocking read doesn't hold a connection from the shared pool and starve
// heartbeats, enqueues, acks, and token stream operations.
//
// Each GPU worker should get its own blocking reader. The returned cleanup
// function closes the dedicated Redis client — call it when the worker
// disconnects.
//
// Usage:
//
//	reader, cleanup := sharedQueue.NewBlockingReader()
//	defer cleanup()
//	entries, err := reader.Read(ctx, workerID, 30*time.Second) // blocks on dedicated conn
//	_ = sharedQueue.Ack(ctx, entryID)                          // uses shared pool
func (q *JobQueue) NewBlockingReader() (*JobQueue, func()) {
	opts := *q.client.Options() // Copy all connection settings
	opts.PoolSize = 1           // Dedicated connection — one is all we need
	opts.MinIdleConns = 1       // Keep it warm
	dedicated := redis.NewClient(&opts)

	reader := &JobQueue{
		client:    dedicated,
		streamKey: q.streamKey,
		groupName: q.groupName,
	}

	cleanup := func() {
		dedicated.Close()
	}

	return reader, cleanup
}

// EnsureGroup creates the stream and consumer group if they don't exist.
// Safe to call multiple times (idempotent).
func (q *JobQueue) EnsureGroup(ctx context.Context) error {
	// XGROUP CREATE with MKSTREAM creates both the stream and group.
	// If the group already exists, Redis returns BUSYGROUP error — we ignore it.
	err := q.client.XGroupCreateMkStream(ctx, q.streamKey, q.groupName, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return fmt.Errorf("creating consumer group: %w", err)
	}
	return nil
}

// Enqueue adds a job to the stream. Returns the Redis stream entry ID.
func (q *JobQueue) Enqueue(ctx context.Context, jobID string, data []byte) (string, error) {
	id, err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.streamKey,
		Values: map[string]interface{}{
			"job_id": jobID,
			"data":   string(data),
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("enqueuing job %s: %w", jobID, err)
	}
	return id, nil
}

// Read blocks until a new message arrives or the context is cancelled.
// The consumer name identifies this specific worker for message assignment.
// blockTimeout of 0 means block indefinitely.
func (q *JobQueue) Read(ctx context.Context, consumerName string, blockTimeout time.Duration) ([]StreamEntry, error) {
	results, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.groupName,
		Consumer: consumerName,
		Streams:  []string{q.streamKey, ">"},
		Count:    1,
		Block:    blockTimeout,
	}).Result()

	if err == redis.Nil {
		return nil, nil // Timeout, no messages
	}
	if err != nil {
		return nil, fmt.Errorf("reading from stream: %w", err)
	}

	return parseStreamResults(results)
}

// Ack acknowledges and deletes a message. Must be called after successful processing.
func (q *JobQueue) Ack(ctx context.Context, entryID string) error {
	// Step 1: XACK — mark as processed in consumer group
	if err := q.client.XAck(ctx, q.streamKey, q.groupName, entryID).Err(); err != nil {
		return fmt.Errorf("acknowledging message %s: %w", entryID, err)
	}

	// Step 2: XDEL — remove from stream to free memory
	if err := q.client.XDel(ctx, q.streamKey, entryID).Err(); err != nil {
		return fmt.Errorf("deleting message %s: %w", entryID, err)
	}

	return nil
}

// ClaimPending reclaims messages that have been idle for longer than minIdle.
// Used on worker reconnect to recover jobs that were in-flight when the previous
// connection dropped.
func (q *JobQueue) ClaimPending(ctx context.Context, consumerName string, minIdle time.Duration) ([]StreamEntry, error) {
	messages, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   q.streamKey,
		Group:    q.groupName,
		Consumer: consumerName,
		MinIdle:  minIdle,
		Start:    "0-0",
		Count:    100,
	}).Result()

	if err != nil {
		// NOGROUP means the group doesn't exist yet — first connection, nothing to claim
		if err.Error() == "NOGROUP No such consumer group '"+q.groupName+"' for key name '"+q.streamKey+"'" {
			return nil, nil
		}
		return nil, fmt.Errorf("claiming pending messages: %w", err)
	}

	return parseXMessages(messages)
}

// ReadPending returns entries that were previously delivered to this consumer
// but not yet acknowledged. These are entries stuck in the Pending Entries List
// (PEL) — for example, from a previous dispatch attempt that failed because
// no workers had capacity.
//
// Unlike Read (which uses ">" for new-only), this uses "0" which returns
// pending entries for the consumer. Returns immediately (non-blocking).
func (q *JobQueue) ReadPending(ctx context.Context, consumerName string) ([]StreamEntry, error) {
	results, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.groupName,
		Consumer: consumerName,
		Streams:  []string{q.streamKey, "0"},
		Count:    10,
		Block:    -1, // go-redis: -1 means "don't send BLOCK at all" → non-blocking
	}).Result()

	if err == redis.Nil {
		return nil, nil // No pending entries
	}
	if err != nil {
		return nil, fmt.Errorf("reading pending entries: %w", err)
	}

	return parseStreamResults(results)
}

// Len returns the number of entries in the stream.
func (q *JobQueue) Len(ctx context.Context) (int64, error) {
	n, err := q.client.XLen(ctx, q.streamKey).Result()
	if err != nil {
		return 0, fmt.Errorf("getting stream length: %w", err)
	}
	return n, nil
}

// parseStreamResults converts go-redis XStream results to our StreamEntry type.
func parseStreamResults(results []redis.XStream) ([]StreamEntry, error) {
	var entries []StreamEntry
	for _, stream := range results {
		parsed, err := parseXMessages(stream.Messages)
		if err != nil {
			return nil, err
		}
		entries = append(entries, parsed...)
	}
	return entries, nil
}

// parseXMessages converts go-redis XMessage slice to our StreamEntry type.
func parseXMessages(messages []redis.XMessage) ([]StreamEntry, error) {
	var entries []StreamEntry
	for _, msg := range messages {
		jobID, _ := msg.Values["job_id"].(string)
		data, _ := msg.Values["data"].(string)

		entries = append(entries, StreamEntry{
			ID: msg.ID,
			Payload: JobPayload{
				JobID: jobID,
				Data:  []byte(data),
			},
		})
	}
	return entries, nil
}

// TokenStream manages per-job ephemeral streams for token-by-token delivery.
type TokenStream struct {
	client *redis.Client
	prefix string // Key prefix (e.g., "auxot:" or "")
}

// TokenEvent is a single event in a job's token stream.
type TokenEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// NewTokenStream creates a TokenStream with the given key prefix.
func NewTokenStream(client *redis.Client, prefix string) *TokenStream {
	return &TokenStream{client: client, prefix: prefix}
}

// NewBlockingReader creates a new TokenStream backed by a dedicated Redis
// connection (pool size 1). Use this for XREAD BLOCK calls when reading
// tokens so the blocking read doesn't hold a connection from the shared pool.
//
// Each API request that waits for a streaming or blocking response should
// create its own reader. The connection lifecycle matches the request:
// request arrives → create reader → stream tokens → request ends → cleanup.
//
// This prevents 100 concurrent API callers from starving the shared pool
// that heartbeats, enqueues, acks, and token publishes depend on.
func (ts *TokenStream) NewBlockingReader() (*TokenStream, func()) {
	opts := *ts.client.Options()
	opts.PoolSize = 1
	opts.MinIdleConns = 1
	dedicated := redis.NewClient(&opts)

	reader := &TokenStream{
		client: dedicated,
		prefix: ts.prefix,
	}

	cleanup := func() {
		dedicated.Close()
	}

	return reader, cleanup
}

// streamKey builds the Redis key for a job's token stream.
func (ts *TokenStream) streamKey(jobID string) string {
	return ts.prefix + "tokens:" + jobID
}

// Publish adds a token event to a job's stream.
func (ts *TokenStream) Publish(ctx context.Context, jobID string, event TokenEvent) (string, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("marshaling token event: %w", err)
	}

	id, err := ts.client.XAdd(ctx, &redis.XAddArgs{
		Stream: ts.streamKey(jobID),
		Values: map[string]interface{}{
			"event": string(data),
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("publishing token event for job %s: %w", jobID, err)
	}
	return id, nil
}

// Read retrieves token events from a job's stream starting after lastID.
// If block > 0, it waits for new events up to the timeout.
// If block <= 0, it returns immediately with whatever is available.
// Use lastID "0-0" to read from the beginning, or the last seen ID for incremental reads.
func (ts *TokenStream) Read(ctx context.Context, jobID string, lastID string, count int64, block time.Duration) ([]TokenStreamEntry, error) {
	args := &redis.XReadArgs{
		Streams: []string{ts.streamKey(jobID), lastID},
		Count:   count,
		// go-redis sends BLOCK when Block >= 0. BLOCK 0 means "block forever" in Redis.
		// Setting Block to -1 means "don't include BLOCK at all" → non-blocking read.
		Block: -1,
	}
	if block > 0 {
		args.Block = block
	}

	results, err := ts.client.XRead(ctx, args).Result()
	if err == redis.Nil {
		return nil, nil // Timeout, no new events
	}
	if err != nil {
		return nil, fmt.Errorf("reading token stream for job %s: %w", jobID, err)
	}

	var entries []TokenStreamEntry
	for _, stream := range results {
		for _, msg := range stream.Messages {
			eventJSON, _ := msg.Values["event"].(string)
			var event TokenEvent
			if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
				return nil, fmt.Errorf("parsing token event: %w", err)
			}
			entries = append(entries, TokenStreamEntry{
				ID:    msg.ID,
				Event: event,
			})
		}
	}
	return entries, nil
}

// SetTTL sets an expiry on a job's token stream. Call after the job completes
// to auto-cleanup the ephemeral stream.
func (ts *TokenStream) SetTTL(ctx context.Context, jobID string, ttl time.Duration) error {
	ok, err := ts.client.Expire(ctx, ts.streamKey(jobID), ttl).Result()
	if err != nil {
		return fmt.Errorf("setting TTL for job %s token stream: %w", jobID, err)
	}
	if !ok {
		// Key doesn't exist — this is fine, the stream may have already expired or never existed
		return nil
	}
	return nil
}

// Delete removes a job's token stream immediately.
func (ts *TokenStream) Delete(ctx context.Context, jobID string) error {
	if err := ts.client.Del(ctx, ts.streamKey(jobID)).Err(); err != nil {
		return fmt.Errorf("deleting token stream for job %s: %w", jobID, err)
	}
	return nil
}

// TokenStreamEntry is a single entry read from a token stream.
type TokenStreamEntry struct {
	ID    string
	Event TokenEvent
}
