package queue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis starts a miniredis server and returns a connected client.
// The server is automatically shut down when the test ends.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	return redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
}

// --- JobQueue tests ---

func TestJobQueue_EnqueueAndRead(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	q := NewJobQueue(client, "test:jobs", "workers")

	// Create the consumer group first
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensuring group: %v", err)
	}

	// Enqueue a job
	jobData := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)
	entryID, err := q.Enqueue(ctx, "job-001", jobData)
	if err != nil {
		t.Fatalf("enqueuing: %v", err)
	}
	if entryID == "" {
		t.Fatal("entry ID should not be empty")
	}

	// Read it back (non-blocking since miniredis doesn't support BLOCK)
	entries, err := q.Read(ctx, "worker-1", 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	entry := entries[0]
	if entry.Payload.JobID != "job-001" {
		t.Errorf("job ID: got %q, want %q", entry.Payload.JobID, "job-001")
	}
	if string(entry.Payload.Data) != string(jobData) {
		t.Errorf("data mismatch: got %q, want %q", entry.Payload.Data, jobData)
	}
}

func TestJobQueue_EnsureGroupIdempotent(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	q := NewJobQueue(client, "test:jobs", "workers")

	// Calling EnsureGroup twice should not error
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("second call (should be idempotent): %v", err)
	}
}

func TestJobQueue_AckAndDelete(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	q := NewJobQueue(client, "test:jobs", "workers")
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensuring group: %v", err)
	}

	// Enqueue and read
	_, err := q.Enqueue(ctx, "job-002", []byte(`{}`))
	if err != nil {
		t.Fatalf("enqueuing: %v", err)
	}

	entries, err := q.Read(ctx, "worker-1", 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// Ack + delete
	if err := q.Ack(ctx, entries[0].ID); err != nil {
		t.Fatalf("acknowledging: %v", err)
	}

	// Stream should now be empty
	length, err := q.Len(ctx)
	if err != nil {
		t.Fatalf("getting length: %v", err)
	}
	if length != 0 {
		t.Errorf("stream should be empty after ack+delete, got length %d", length)
	}
}

func TestJobQueue_Len(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	q := NewJobQueue(client, "test:jobs", "workers")
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensuring group: %v", err)
	}

	// Start empty
	length, err := q.Len(ctx)
	if err != nil {
		t.Fatalf("getting initial length: %v", err)
	}
	if length != 0 {
		t.Errorf("initial length should be 0, got %d", length)
	}

	// Add 3 jobs
	for i := 0; i < 3; i++ {
		if _, err := q.Enqueue(ctx, "job-len", []byte(`{}`)); err != nil {
			t.Fatalf("enqueuing job %d: %v", i, err)
		}
	}

	length, err = q.Len(ctx)
	if err != nil {
		t.Fatalf("getting length: %v", err)
	}
	if length != 3 {
		t.Errorf("length should be 3, got %d", length)
	}
}

func TestJobQueue_MultipleConsumers(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	q := NewJobQueue(client, "test:jobs", "workers")
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensuring group: %v", err)
	}

	// Enqueue 2 jobs
	if _, err := q.Enqueue(ctx, "job-a", []byte(`{"job":"a"}`)); err != nil {
		t.Fatalf("enqueuing job-a: %v", err)
	}
	if _, err := q.Enqueue(ctx, "job-b", []byte(`{"job":"b"}`)); err != nil {
		t.Fatalf("enqueuing job-b: %v", err)
	}

	// Two workers each read one job
	entries1, err := q.Read(ctx, "worker-1", 0)
	if err != nil {
		t.Fatalf("worker-1 reading: %v", err)
	}
	entries2, err := q.Read(ctx, "worker-2", 0)
	if err != nil {
		t.Fatalf("worker-2 reading: %v", err)
	}

	if len(entries1) != 1 {
		t.Fatalf("worker-1 should get 1 job, got %d", len(entries1))
	}
	if len(entries2) != 1 {
		t.Fatalf("worker-2 should get 1 job, got %d", len(entries2))
	}

	// Different jobs
	if entries1[0].Payload.JobID == entries2[0].Payload.JobID {
		t.Error("workers should receive different jobs")
	}
}

// --- TokenStream tests ---

func TestTokenStream_PublishAndRead(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	ts := NewTokenStream(client, "test:")

	// Publish a few token events
	events := []TokenEvent{
		{Type: "token", Data: json.RawMessage(`"Hello"`)},
		{Type: "token", Data: json.RawMessage(`" world"`)},
		{Type: "done"},
	}

	for _, ev := range events {
		if _, err := ts.Publish(ctx, "job-100", ev); err != nil {
			t.Fatalf("publishing: %v", err)
		}
	}

	// Read all events from the beginning
	entries, err := ts.Read(ctx, "job-100", "0-0", 10, 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if entries[0].Event.Type != "token" {
		t.Errorf("first event type: got %q, want %q", entries[0].Event.Type, "token")
	}
	if entries[2].Event.Type != "done" {
		t.Errorf("last event type: got %q, want %q", entries[2].Event.Type, "done")
	}
}

func TestTokenStream_IncrementalRead(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	ts := NewTokenStream(client, "")

	// Publish 2 events
	if _, err := ts.Publish(ctx, "job-200", TokenEvent{Type: "token", Data: json.RawMessage(`"A"`)}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if _, err := ts.Publish(ctx, "job-200", TokenEvent{Type: "token", Data: json.RawMessage(`"B"`)}); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	// Read first batch
	entries, err := ts.Read(ctx, "job-200", "0-0", 10, 0)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	lastID := entries[len(entries)-1].ID

	// Publish one more
	if _, err := ts.Publish(ctx, "job-200", TokenEvent{Type: "done"}); err != nil {
		t.Fatalf("publishing done: %v", err)
	}

	// Read incrementally from last seen ID
	newEntries, err := ts.Read(ctx, "job-200", lastID, 10, 0)
	if err != nil {
		t.Fatalf("incremental read: %v", err)
	}
	if len(newEntries) != 1 {
		t.Fatalf("expected 1 new entry, got %d", len(newEntries))
	}
	if newEntries[0].Event.Type != "done" {
		t.Errorf("new event type: got %q, want %q", newEntries[0].Event.Type, "done")
	}
}

func TestTokenStream_SetTTL(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	ts := NewTokenStream(client, "test:")

	// Publish something so the key exists
	if _, err := ts.Publish(ctx, "job-300", TokenEvent{Type: "token"}); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	// Set TTL
	if err := ts.SetTTL(ctx, "job-300", 60*time.Second); err != nil {
		t.Fatalf("setting TTL: %v", err)
	}

	// Verify TTL was set
	ttl, err := client.TTL(ctx, "test:tokens:job-300").Result()
	if err != nil {
		t.Fatalf("getting TTL: %v", err)
	}
	if ttl <= 0 {
		t.Errorf("TTL should be positive, got %v", ttl)
	}
}

func TestTokenStream_Delete(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	ts := NewTokenStream(client, "")

	// Publish and verify it exists
	if _, err := ts.Publish(ctx, "job-400", TokenEvent{Type: "token"}); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	entries, err := ts.Read(ctx, "job-400", "0-0", 10, 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("should have 1 entry, got %d", len(entries))
	}

	// Delete
	if err := ts.Delete(ctx, "job-400"); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	// Should be empty now
	entries, err = ts.Read(ctx, "job-400", "0-0", 10, 0)
	if err != nil {
		t.Fatalf("reading after delete: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("should have 0 entries after delete, got %d", len(entries))
	}
}

func TestTokenStream_KeyPrefix(t *testing.T) {
	ts := NewTokenStream(nil, "auxot:")
	key := ts.streamKey("job-123")
	expected := "auxot:tokens:job-123"
	if key != expected {
		t.Errorf("stream key: got %q, want %q", key, expected)
	}
}

func TestTokenStream_NoPrefix(t *testing.T) {
	ts := NewTokenStream(nil, "")
	key := ts.streamKey("job-456")
	expected := "tokens:job-456"
	if key != expected {
		t.Errorf("stream key: got %q, want %q", key, expected)
	}
}
