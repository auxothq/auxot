// Package gpu manages connected GPU worker connections for the router.
//
// The Pool tracks which workers are online, their capabilities, how many
// active jobs each has, and which worker to assign new jobs to.
//
// Thread-safe — all operations are protected by a sync.RWMutex because
// WebSocket handlers run concurrently.
package gpu

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Worker represents a connected GPU worker.
type Worker struct {
	ID            string
	Capabilities  Capabilities
	ActiveJobs    map[string]time.Time // jobID → assigned time
	MaxSlots      int
	ConnectedAt   time.Time
	LastHeartbeat time.Time
}

// Capabilities describes what a GPU worker can do.
type Capabilities struct {
	Backend    string  `json:"backend"`    // e.g., "llama.cpp"
	Model      string  `json:"model"`
	CtxSize    int     `json:"ctx_size"`
	VRAMGB     float64 `json:"vram_gb,omitempty"`
	Parameters string  `json:"parameters,omitempty"`
	TotalSlots int     `json:"total_slots,omitempty"`
}

// HasCapacity returns true if the worker can accept another job.
func (w *Worker) HasCapacity() bool {
	return len(w.ActiveJobs) < w.MaxSlots
}

// ActiveJobCount returns the number of jobs currently assigned to this worker.
func (w *Worker) ActiveJobCount() int {
	return len(w.ActiveJobs)
}

// Pool tracks all connected GPU workers and provides worker selection.
type Pool struct {
	workers map[string]*Worker // workerID → Worker
	mu      sync.RWMutex
	logger  *slog.Logger
}

// NewPool creates an empty GPU pool.
func NewPool(logger *slog.Logger) *Pool {
	return &Pool{
		workers: make(map[string]*Worker),
		logger:  logger,
	}
}

// Add registers a new worker in the pool.
// Returns an error if a worker with the same ID already exists.
func (p *Pool) Add(id string, caps Capabilities, maxSlots int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.workers[id]; exists {
		return fmt.Errorf("worker %q already in pool", id)
	}

	now := time.Now()
	p.workers[id] = &Worker{
		ID:            id,
		Capabilities:  caps,
		ActiveJobs:    make(map[string]time.Time),
		MaxSlots:      maxSlots,
		ConnectedAt:   now,
		LastHeartbeat: now,
	}

	p.logger.Info("worker added to pool",
		"worker_id", id,
		"model", caps.Model,
		"max_slots", maxSlots,
		"pool_size", len(p.workers),
	)
	return nil
}

// Remove disconnects a worker from the pool.
// Returns the list of job IDs that were active on this worker (for requeue).
func (p *Pool) Remove(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	worker, exists := p.workers[id]
	if !exists {
		return nil
	}

	// Collect active job IDs before removing
	var orphanedJobs []string
	for jobID := range worker.ActiveJobs {
		orphanedJobs = append(orphanedJobs, jobID)
	}

	delete(p.workers, id)

	p.logger.Info("worker removed from pool",
		"worker_id", id,
		"orphaned_jobs", len(orphanedJobs),
		"pool_size", len(p.workers),
	)
	return orphanedJobs
}

// Heartbeat updates a worker's last heartbeat timestamp.
// Returns false if the worker is not in the pool.
func (p *Pool) Heartbeat(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	worker, exists := p.workers[id]
	if !exists {
		return false
	}
	worker.LastHeartbeat = time.Now()
	return true
}

// UpdateCapabilities updates a worker's capabilities (called when worker sends config).
func (p *Pool) UpdateCapabilities(id string, caps Capabilities) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	worker, exists := p.workers[id]
	if !exists {
		return false
	}

	worker.Capabilities = caps
	if caps.TotalSlots > 0 {
		worker.MaxSlots = caps.TotalSlots
	}

	p.logger.Info("worker capabilities updated",
		"worker_id", id,
		"model", caps.Model,
		"backend", caps.Backend,
		"ctx_size", caps.CtxSize,
		"max_slots", worker.MaxSlots,
	)
	return true
}

// AssignJob marks a job as active on a specific worker.
// Returns an error if the worker doesn't exist or has no capacity.
func (p *Pool) AssignJob(workerID, jobID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	worker, exists := p.workers[workerID]
	if !exists {
		return fmt.Errorf("worker %q not in pool", workerID)
	}
	if !worker.HasCapacity() {
		return fmt.Errorf("worker %q at capacity (%d/%d)", workerID, len(worker.ActiveJobs), worker.MaxSlots)
	}

	worker.ActiveJobs[jobID] = time.Now()
	return nil
}

// CompleteJob removes a job from a worker's active list.
// Returns false if the worker or job was not found.
func (p *Pool) CompleteJob(workerID, jobID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	worker, exists := p.workers[workerID]
	if !exists {
		return false
	}

	_, hadJob := worker.ActiveJobs[jobID]
	delete(worker.ActiveJobs, jobID)
	return hadJob
}

// SelectWorker picks the best available worker for a new job.
// Strategy: worker with the fewest active jobs (least loaded).
// Returns the worker ID, or empty string if no workers have capacity.
func (p *Pool) SelectWorker() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestID string
	bestLoad := -1

	for id, w := range p.workers {
		if !w.HasCapacity() {
			continue
		}
		load := len(w.ActiveJobs)
		if bestLoad == -1 || load < bestLoad {
			bestID = id
			bestLoad = load
		}
	}

	return bestID
}

// Size returns the number of workers in the pool.
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.workers)
}

// TotalCapacity returns the total number of available job slots across all workers.
func (p *Pool) TotalCapacity() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total := 0
	for _, w := range p.workers {
		total += w.MaxSlots - len(w.ActiveJobs)
	}
	return total
}

// WorkerIDs returns the IDs of all workers in the pool.
func (p *Pool) WorkerIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ids := make([]string, 0, len(p.workers))
	for id := range p.workers {
		ids = append(ids, id)
	}
	return ids
}

// DeadWorkers returns the IDs of workers whose last heartbeat is older than maxAge.
func (p *Pool) DeadWorkers(maxAge time.Duration) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cutoff := time.Now().Add(-maxAge)
	var dead []string
	for id, w := range p.workers {
		if w.LastHeartbeat.Before(cutoff) {
			dead = append(dead, id)
		}
	}
	return dead
}

// Snapshot returns a read-only copy of all worker states.
// Used for admin/status endpoints.
func (p *Pool) Snapshot() []WorkerSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snaps := make([]WorkerSnapshot, 0, len(p.workers))
	for _, w := range p.workers {
		jobIDs := make([]string, 0, len(w.ActiveJobs))
		for jid := range w.ActiveJobs {
			jobIDs = append(jobIDs, jid)
		}

		snaps = append(snaps, WorkerSnapshot{
			ID:            w.ID,
			Capabilities:  w.Capabilities,
			ActiveJobIDs:  jobIDs,
			MaxSlots:      w.MaxSlots,
			ConnectedAt:   w.ConnectedAt,
			LastHeartbeat: w.LastHeartbeat,
		})
	}
	return snaps
}

// WorkerSnapshot is a read-only copy of a worker's state.
type WorkerSnapshot struct {
	ID            string       `json:"id"`
	Capabilities  Capabilities `json:"capabilities"`
	ActiveJobIDs  []string     `json:"active_jobs"`
	MaxSlots      int          `json:"max_slots"`
	ConnectedAt   time.Time    `json:"connected_at"`
	LastHeartbeat time.Time    `json:"last_heartbeat"`
}
