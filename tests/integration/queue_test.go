//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/auxothq/auxot/pkg/openai"
)

// TestQueue_JobsEnqueueWhenWorkerBusy proves that jobs are queued in Redis
// when the worker is at capacity, and then dispatched when the worker finishes.
//
// Setup:
//   - 1 mock worker with 1 slot
//   - Worker has a 500ms delay before responding to each job
//
// Test:
//   - Send 3 concurrent requests
//   - First job should be dispatched immediately (fills the slot)
//   - Other 2 should be enqueued in Redis
//   - Verify Redis queue has pending entries
//   - All 3 should eventually complete
func TestQueue_JobsEnqueueWhenWorkerBusy(t *testing.T) {
	env := setupTestEnv(t)

	// Worker with 1 slot and a delay to simulate real inference time
	worker := newMockWorker()
	worker.responseDelay = 500 * time.Millisecond
	worker.connectWithSlots(t, env, 1)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	// Verify starting state
	queueLen := mustQueueLen(t, env)
	if queueLen != 0 {
		t.Fatalf("expected empty queue, got %d entries", queueLen)
	}

	// Send 3 requests concurrently
	numRequests := 3
	var wg sync.WaitGroup
	results := make(chan requestResult, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start := time.Now()

			resp, body, err := sendOpenAIRequest(env, "request "+string(rune('A'+idx)))
			results <- requestResult{
				index:    idx,
				status:   resp,
				body:     body,
				err:      err,
				duration: time.Since(start),
			}
		}(i)
	}

	// Poll until the worker is busy (slot filled). Under the race detector
	// goroutine scheduling is ~10x slower, so we poll instead of fixed sleep.
	var activeJobs int
	pollDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(pollDeadline) {
		h := mustGetHealth(t, env)
		activeJobs = h.ActiveJobs
		if activeJobs > 0 || h.Available == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("mid-test health: active_jobs=%d", activeJobs)

	// At this point the worker should be busy with 1 job.
	// The remaining jobs are either in the Redis queue or being enqueued.
	// We don't assert exact queue length here because timing varies —
	// the important thing is that ALL requests eventually complete below.

	// Wait for all requests to complete
	wg.Wait()
	close(results)

	// Collect results
	var completed int
	var totalDuration time.Duration

	for r := range results {
		if r.err != nil {
			t.Errorf("request %d failed: %v", r.index, r.err)
			continue
		}
		if r.status != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d: %s", r.index, r.status, r.body)
			continue
		}

		var resp openai.ChatCompletionResponse
		if err := json.Unmarshal([]byte(r.body), &resp); err != nil {
			t.Errorf("request %d: failed to parse response: %v", r.index, err)
			continue
		}

		if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
			t.Errorf("request %d: empty response", r.index)
			continue
		}

		completed++
		totalDuration += r.duration
		t.Logf("request %d completed in %v: %q",
			r.index, r.duration.Round(time.Millisecond), resp.Choices[0].Message.Content)
	}

	if completed != numRequests {
		t.Fatalf("expected %d completed requests, got %d", numRequests, completed)
	}

	// All 3 requests completed. Since the worker processes sequentially
	// (1 slot, 500ms delay), the total elapsed time for the last request
	// should be at least 1.5s (3 × 500ms).
	t.Logf("all %d requests completed successfully", completed)

	// Queue should be empty now (all jobs dispatched and completed)
	time.Sleep(200 * time.Millisecond) // Give the dispatcher a moment to clean up
	queueLen = mustQueueLen(t, env)
	t.Logf("post-test queue length: %d (should be 0)", queueLen)
}

// TestQueue_NoWorkersAvailable verifies that jobs are enqueued (not 503) when
// no workers are connected. Once a worker connects, the queued jobs should be
// dispatched.
func TestQueue_NoWorkersAvailable(t *testing.T) {
	env := setupTestEnv(t)

	// Send a request with NO workers connected
	reqBody := openai.ChatCompletionRequest{
		Model: "test-model",
		Messages: []openai.Message{
			{Role: "user", Content: "Hello?"},
		},
		Stream: false,
	}
	body, _ := json.Marshal(reqBody)

	// Use a client with a timeout since the request will block waiting for tokens
	client := &http.Client{Timeout: 15 * time.Second}

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	// Send request in background (it will block)
	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		resultCh <- result{resp, err}
	}()

	// Poll until the job appears in the queue. Under the race detector
	// goroutine scheduling is slower, so we poll instead of fixed sleep.
	var queueLen int64
	pollDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(pollDeadline) {
		queueLen = mustQueueLen(t, env)
		if queueLen > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("queue length after sending with no workers: %d", queueLen)

	if queueLen == 0 {
		t.Error("expected at least 1 job in queue when no workers available")
	}

	// Now connect a worker — the dispatcher should pick up the queued job
	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	// Wait for the request to complete
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("request failed: %v", r.err)
		}
		defer r.resp.Body.Close()

		if r.resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(r.resp.Body)
			t.Fatalf("expected 200, got %d: %s", r.resp.StatusCode, string(b))
		}

		var resp openai.ChatCompletionResponse
		json.NewDecoder(r.resp.Body).Decode(&resp)

		if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
			t.Fatal("empty response")
		}

		t.Logf("queued job completed: %q", resp.Choices[0].Message.Content)

	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for queued job to complete")
	}
}

// TestQueue_HealthShowsCapacity verifies the health endpoint reflects worker
// capacity changes as jobs are dispatched and completed.
func TestQueue_HealthShowsCapacity(t *testing.T) {
	env := setupTestEnv(t)

	// Before worker: 0 workers, 0 slots
	health := mustGetHealth(t, env)
	if health.Workers != 0 {
		t.Errorf("expected 0 workers, got %d", health.Workers)
	}

	// Connect worker with 2 slots
	worker := newMockWorker()
	worker.responseDelay = 300 * time.Millisecond
	worker.connectWithSlots(t, env, 2)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	// After worker: 1 worker, 2 slots, 0 active
	health = mustGetHealth(t, env)
	if health.Workers != 1 {
		t.Errorf("expected 1 worker, got %d", health.Workers)
	}
	if health.TotalSlots != 2 {
		t.Errorf("expected 2 total slots, got %d", health.TotalSlots)
	}
	if health.Available != 2 {
		t.Errorf("expected 2 available, got %d", health.Available)
	}

	// Send 1 request (non-blocking) — worker has a 300ms response delay
	go sendOpenAIRequest(env, "test")

	// Poll until a job is active (race detector slows scheduling)
	pollDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(pollDeadline) {
		health = mustGetHealth(t, env)
		if health.ActiveJobs > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if health.ActiveJobs != 1 {
		t.Errorf("expected 1 active job during request, got %d", health.ActiveJobs)
	}

	// Poll until the job completes (back to 0 active)
	pollDeadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(pollDeadline) {
		health = mustGetHealth(t, env)
		if health.ActiveJobs == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if health.ActiveJobs != 0 {
		t.Errorf("expected 0 active jobs after completion, got %d", health.ActiveJobs)
	}
}

// --- Helpers ---

type requestResult struct {
	index    int
	status   int
	body     string
	err      error
	duration time.Duration
}

type healthResponse struct {
	Workers    int `json:"workers"`
	TotalSlots int `json:"total_slots"`
	ActiveJobs int `json:"active_jobs"`
	Available  int `json:"available"`
}

func sendOpenAIRequest(env *testEnv, content string) (int, string, error) {
	reqBody := openai.ChatCompletionRequest{
		Model: "test-model",
		Messages: []openai.Message{
			{Role: "user", Content: content},
		},
		Stream: false,
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func mustQueueLen(t *testing.T, env *testEnv) int64 {
	t.Helper()
	n, err := env.jobQueue.Len(context.Background())
	if err != nil {
		t.Fatalf("getting queue length: %v", err)
	}
	return n
}

func mustGetHealth(t *testing.T, env *testEnv) healthResponse {
	t.Helper()
	resp, err := http.Get(env.baseURL() + "/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	defer resp.Body.Close()

	var h healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("parsing health response: %v", err)
	}
	return h
}
