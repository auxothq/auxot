package gpu

import (
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestPool_AddAndSize(t *testing.T) {
	p := NewPool(testLogger())

	if p.Size() != 0 {
		t.Fatalf("new pool should be empty, got size %d", p.Size())
	}

	err := p.Add("gpu-1", Capabilities{Model: "Qwen2.5-7B", Backend: "llama.cpp"}, 2)
	if err != nil {
		t.Fatalf("adding worker: %v", err)
	}
	if p.Size() != 1 {
		t.Errorf("pool size should be 1, got %d", p.Size())
	}

	err = p.Add("gpu-2", Capabilities{Model: "Qwen2.5-7B", Backend: "llama.cpp"}, 2)
	if err != nil {
		t.Fatalf("adding second worker: %v", err)
	}
	if p.Size() != 2 {
		t.Errorf("pool size should be 2, got %d", p.Size())
	}
}

func TestPool_AddDuplicate(t *testing.T) {
	p := NewPool(testLogger())

	if err := p.Add("gpu-1", Capabilities{}, 1); err != nil {
		t.Fatalf("first add: %v", err)
	}

	err := p.Add("gpu-1", Capabilities{}, 1)
	if err == nil {
		t.Fatal("adding duplicate worker should error")
	}
}

func TestPool_Remove(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("gpu-1", Capabilities{}, 2)

	// Assign a job so we can test orphan detection
	_ = p.AssignJob("gpu-1", "job-1")

	orphans := p.Remove("gpu-1")
	if len(orphans) != 1 {
		t.Fatalf("should have 1 orphaned job, got %d", len(orphans))
	}
	if orphans[0] != "job-1" {
		t.Errorf("orphaned job should be 'job-1', got %q", orphans[0])
	}
	if p.Size() != 0 {
		t.Errorf("pool should be empty after remove, got size %d", p.Size())
	}
}

func TestPool_RemoveNonexistent(t *testing.T) {
	p := NewPool(testLogger())
	orphans := p.Remove("ghost")
	if orphans != nil {
		t.Errorf("removing non-existent worker should return nil, got %v", orphans)
	}
}

func TestPool_Heartbeat(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("gpu-1", Capabilities{}, 1)

	if !p.Heartbeat("gpu-1") {
		t.Error("heartbeat for existing worker should return true")
	}
	if p.Heartbeat("ghost") {
		t.Error("heartbeat for non-existent worker should return false")
	}
}

func TestPool_AssignAndComplete(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("gpu-1", Capabilities{}, 2)

	// Assign first job
	if err := p.AssignJob("gpu-1", "job-1"); err != nil {
		t.Fatalf("assigning job-1: %v", err)
	}

	// Assign second job (should succeed, capacity is 2)
	if err := p.AssignJob("gpu-1", "job-2"); err != nil {
		t.Fatalf("assigning job-2: %v", err)
	}

	// Third job should fail (at capacity)
	if err := p.AssignJob("gpu-1", "job-3"); err == nil {
		t.Fatal("assigning job-3 should fail (at capacity)")
	}

	// Complete one job
	if !p.CompleteJob("gpu-1", "job-1") {
		t.Error("completing existing job should return true")
	}

	// Now third job should succeed
	if err := p.AssignJob("gpu-1", "job-3"); err != nil {
		t.Fatalf("assigning job-3 after completion: %v", err)
	}
}

func TestPool_CompleteNonexistent(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("gpu-1", Capabilities{}, 1)

	if p.CompleteJob("gpu-1", "no-such-job") {
		t.Error("completing non-existent job should return false")
	}
	if p.CompleteJob("ghost", "job-1") {
		t.Error("completing job on non-existent worker should return false")
	}
}

func TestPool_SelectWorker_LeastLoaded(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("busy", Capabilities{}, 2)
	_ = p.Add("idle", Capabilities{}, 2)

	// Load up the busy worker
	_ = p.AssignJob("busy", "job-a")

	// Selection should pick the idle one
	selected := p.SelectWorker()
	if selected != "idle" {
		t.Errorf("should select least loaded worker, got %q", selected)
	}
}

func TestPool_SelectWorker_NoCapacity(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("full", Capabilities{}, 1)
	_ = p.AssignJob("full", "job-1")

	selected := p.SelectWorker()
	if selected != "" {
		t.Errorf("should return empty when no capacity, got %q", selected)
	}
}

func TestPool_SelectWorker_EmptyPool(t *testing.T) {
	p := NewPool(testLogger())
	selected := p.SelectWorker()
	if selected != "" {
		t.Errorf("empty pool should return empty string, got %q", selected)
	}
}

func TestPool_TotalCapacity(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("gpu-1", Capabilities{}, 3)
	_ = p.Add("gpu-2", Capabilities{}, 2)
	_ = p.AssignJob("gpu-1", "job-1")

	// gpu-1: 3 slots - 1 active = 2 available
	// gpu-2: 2 slots - 0 active = 2 available
	// Total: 4
	cap := p.TotalCapacity()
	if cap != 4 {
		t.Errorf("total capacity should be 4, got %d", cap)
	}
}

func TestPool_WorkerIDs(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("alpha", Capabilities{}, 1)
	_ = p.Add("bravo", Capabilities{}, 1)

	ids := p.WorkerIDs()
	if len(ids) != 2 {
		t.Fatalf("should have 2 IDs, got %d", len(ids))
	}

	// Check both are present (order not guaranteed from map)
	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}
	if !idSet["alpha"] || !idSet["bravo"] {
		t.Errorf("missing expected IDs: %v", ids)
	}
}

func TestPool_DeadWorkers(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("alive", Capabilities{}, 1)
	_ = p.Add("dead", Capabilities{}, 1)

	// Manually set dead worker's heartbeat to the past
	p.mu.Lock()
	p.workers["dead"].LastHeartbeat = time.Now().Add(-5 * time.Minute)
	p.mu.Unlock()

	dead := p.DeadWorkers(1 * time.Minute)
	if len(dead) != 1 {
		t.Fatalf("should have 1 dead worker, got %d", len(dead))
	}
	if dead[0] != "dead" {
		t.Errorf("dead worker should be 'dead', got %q", dead[0])
	}
}

func TestPool_Snapshot(t *testing.T) {
	p := NewPool(testLogger())
	_ = p.Add("gpu-1", Capabilities{Model: "TestModel", Backend: "llama.cpp", VRAMGB: 24}, 2)
	_ = p.AssignJob("gpu-1", "job-1")

	snaps := p.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("should have 1 snapshot, got %d", len(snaps))
	}

	snap := snaps[0]
	if snap.ID != "gpu-1" {
		t.Errorf("snapshot ID: got %q, want %q", snap.ID, "gpu-1")
	}
	if snap.Capabilities.Model != "TestModel" {
		t.Errorf("snapshot model: got %q, want %q", snap.Capabilities.Model, "TestModel")
	}
	if snap.MaxSlots != 2 {
		t.Errorf("snapshot max slots: got %d, want %d", snap.MaxSlots, 2)
	}
	if len(snap.ActiveJobIDs) != 1 {
		t.Errorf("snapshot active jobs: got %d, want 1", len(snap.ActiveJobIDs))
	}
}

// TestPool_ConcurrentAccess verifies the pool is safe for concurrent use.
// Run with -race flag.
func TestPool_ConcurrentAccess(t *testing.T) {
	p := NewPool(testLogger())

	// Add workers from multiple goroutines
	done := make(chan struct{})

	for i := 0; i < 10; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			id := fmt.Sprintf("worker-%d", n)
			_ = p.Add(id, Capabilities{}, 2)
			p.Heartbeat(id)
			_ = p.AssignJob(id, fmt.Sprintf("job-%d", n))
			p.SelectWorker()
			p.CompleteJob(id, fmt.Sprintf("job-%d", n))
			p.Snapshot()
			p.Remove(id)
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}
