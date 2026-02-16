//go:build integration

// Package integration contains end-to-end tests that exercise the full router
// pipeline: HTTP API → Redis → mock GPU worker → token stream → HTTP response.
//
// Run:
//
//	go test -tags integration -v ./tests/integration/
//
// By default tests use embedded miniredis (no external dependencies).
// To test against real Redis, set AUXOT_TEST_REDIS_URL:
//
//	AUXOT_TEST_REDIS_URL=redis://localhost:6379 go test -tags integration -v ./tests/integration/
//
// The tests do NOT require a GPU or llama.cpp. A mock WebSocket worker
// connects to the router and responds with canned tokens.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/auxothq/auxot/internal/router"
	"github.com/auxothq/auxot/pkg/auth"
	"github.com/auxothq/auxot/pkg/gpu"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/queue"
)

// testEnv holds everything needed for an integration test.
type testEnv struct {
	server      *httptest.Server
	adminKey    string // plaintext admin key for workers
	apiKey      string // plaintext API key for callers
	redisClient *redis.Client
	jobQueue    *queue.JobQueue
	tokenStream *queue.TokenStream
	pool        *gpu.Pool
	wsHandler   *router.WSHandler
	cancelFunc  context.CancelFunc
	logger      *slog.Logger
}

// baseURL returns the test server's URL.
func (e *testEnv) baseURL() string {
	return e.server.URL
}

// wsURL returns the WebSocket URL for worker connections.
func (e *testEnv) wsURL() string {
	return "ws" + strings.TrimPrefix(e.server.URL, "http") + "/ws"
}

// setupTestEnv creates a fully wired test environment with router, Redis, and
// mock worker support. Call the returned cleanup function when done.
//
// Uses embedded miniredis by default. Set AUXOT_TEST_REDIS_URL to use external Redis.
func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	var redisURL string
	var mr *miniredis.Miniredis

	if envURL := os.Getenv("AUXOT_TEST_REDIS_URL"); envURL != "" {
		// External Redis explicitly requested
		redisURL = envURL
	} else {
		// Default: embedded miniredis (no external dependencies)
		var err error
		mr, err = miniredis.Run()
		if err != nil {
			t.Fatalf("starting embedded redis: %v", err)
		}
		redisURL = "redis://" + mr.Addr()
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Skipf("invalid Redis URL: %v", err)
	}

	redisClient := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		redisClient.Close()
		if mr != nil {
			mr.Close()
		}
		t.Fatalf("Redis not available at %s: %v", redisURL, err)
	}

	// Flush test keys (use a unique prefix per test to avoid collisions)
	testPrefix := fmt.Sprintf("test:%s:", t.Name())

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn, // Keep test output clean
	}))

	// Generate fresh keys
	adminKeyResult, err := auth.GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating admin key: %v", err)
	}
	apiKeyResult, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generating API key: %v", err)
	}

	// Build config manually (bypass LoadConfig which reads env vars)
	cfg := &router.Config{
		Port:              0, // unused with httptest
		Host:              "127.0.0.1",
		RedisURL:          redisURL,
		AdminKeyHash:      adminKeyResult.Hash,
		APIKeyHash:        apiKeyResult.Hash,
		ModelName:         "test-model",
		Quantization:      "Q4_K_S",
		ContextSize:       4096,
		MaxParallel:       1,
		HeartbeatInterval: 15 * time.Second,
		DeadWorkerTimeout: 45 * time.Second,
		JobTimeout:        30 * time.Second,
		AuthCacheTTL:      5 * time.Minute,
	}

	// Build subsystems
	verifier := auth.NewVerifier(cfg.AdminKeyHash, cfg.APIKeyHash, cfg.AuthCacheTTL)
	pool := gpu.NewPool(logger.With("component", "pool"))
	jobQueue := queue.NewJobQueue(redisClient, testPrefix+"jobs", "workers")
	tokenStream := queue.NewTokenStream(redisClient, testPrefix)
	heartbeat := queue.NewHeartbeat(redisClient, testPrefix, cfg.DeadWorkerTimeout)

	if err := jobQueue.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("creating consumer group: %v", err)
	}

	wsHandler := router.NewWSHandler(verifier, pool, jobQueue, tokenStream, heartbeat, cfg, logger.With("component", "ws"))
	apiHandler := router.NewAPIHandler(verifier, pool, jobQueue, tokenStream, cfg, logger.With("component", "api"))
	anthropicHandler := router.NewAnthropicHandler(verifier, jobQueue, tokenStream, cfg, logger.With("component", "anthropic"))

	// Sweeper for dead-worker detection (runs in background)
	sweeper := queue.NewSweeper(jobQueue, heartbeat, 5*time.Second, logger.With("component", "sweeper"))

	// Build HTTP mux (mirrors server.go)
	mux := http.NewServeMux()
	mux.Handle("/ws", wsHandler)
	mux.Handle("/api/openai/", apiHandler)
	mux.Handle("/api/anthropic/", http.StripPrefix("/api/anthropic", anthropicHandler))
	mux.Handle("/health", apiHandler)

	ts := httptest.NewServer(mux)

	// Start sweeper
	sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
	go sweeper.Run(sweeperCtx)

	// Miniredis TTLs don't decrease automatically — advance time so
	// heartbeat keys expire and the sweeper can detect dead workers.
	if mr != nil {
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-sweeperCtx.Done():
					return
				case <-ticker.C:
					mr.FastForward(1 * time.Second)
				}
			}
		}()
	}

	env := &testEnv{
		server:      ts,
		adminKey:    adminKeyResult.Key,
		apiKey:      apiKeyResult.Key,
		redisClient: redisClient,
		jobQueue:    jobQueue,
		tokenStream: tokenStream,
		pool:        pool,
		wsHandler:   wsHandler,
		cancelFunc:  sweeperCancel,
		logger:      logger,
	}

	t.Cleanup(func() {
		sweeperCancel()
		ts.Close()
		// Clean up Redis keys for this test
		keys, _ := redisClient.Keys(context.Background(), testPrefix+"*").Result()
		if len(keys) > 0 {
			redisClient.Del(context.Background(), keys...)
		}
		redisClient.Close()
		if mr != nil {
			mr.Close()
		}
	})

	return env
}

// mockWorker simulates a GPU worker connecting via WebSocket.
type mockWorker struct {
	conn   *websocket.Conn
	gpuID  string
	mu     sync.Mutex // protects writes
	closed bool

	// Behavior controls
	responseDelay time.Duration // How long to wait before starting token output
	tokens        []string     // Tokens to return for each job
	inputTokens   int          // Reported input tokens
	outputTokens  int          // Reported output tokens (auto-calculated if 0)

	// State
	jobsCh chan protocol.JobMessage
	doneCh chan struct{} // closed when readLoop exits
}

// newMockWorker creates a mock worker with default behavior.
func newMockWorker() *mockWorker {
	return &mockWorker{
		tokens:      []string{"Hello", " from", " the", " GPU", "!"},
		inputTokens: 10,
		jobsCh:      make(chan protocol.JobMessage, 100),
		doneCh:      make(chan struct{}),
	}
}

// connect dials the router's WebSocket endpoint, authenticates, and starts
// processing jobs in the background.
func (mw *mockWorker) connect(t *testing.T, env *testEnv) {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("mock worker dial: %v", err)
	}
	mw.conn = conn

	// Send hello
	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: env.adminKey,
		Capabilities: protocol.Capabilities{
			Backend:    "mock",
			Model:      "test-model",
			CtxSize:    4096,
			TotalSlots: 1,
		},
	}
	mw.sendJSON(t, hello)

	// Read hello_ack
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading hello_ack: %v", err)
	}
	msg, err := protocol.ParseMessage(data)
	if err != nil {
		t.Fatalf("parsing hello_ack: %v", err)
	}
	ack, ok := msg.(protocol.HelloAckMessage)
	if !ok {
		t.Fatalf("expected hello_ack, got %T", msg)
	}
	if !ack.Success {
		t.Fatalf("hello_ack failed: %s", ack.Error)
	}
	mw.gpuID = ack.GPUID

	// Start background loops
	go mw.readLoop()
	go mw.processJobs()
}

// connectWithSlots is like connect but sets a custom slot count.
func (mw *mockWorker) connectWithSlots(t *testing.T, env *testEnv, slots int) {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("mock worker dial: %v", err)
	}
	mw.conn = conn

	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: env.adminKey,
		Capabilities: protocol.Capabilities{
			Backend:    "mock",
			Model:      "test-model",
			CtxSize:    4096,
			TotalSlots: slots,
		},
	}
	mw.sendJSON(t, hello)

	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading hello_ack: %v", err)
	}
	msg, err := protocol.ParseMessage(data)
	if err != nil {
		t.Fatalf("parsing hello_ack: %v", err)
	}
	ack, ok := msg.(protocol.HelloAckMessage)
	if !ok {
		t.Fatalf("expected hello_ack, got %T", msg)
	}
	if !ack.Success {
		t.Fatalf("hello_ack failed: %s", ack.Error)
	}
	mw.gpuID = ack.GPUID

	go mw.readLoop()
	go mw.processJobs()
}

// readLoop reads messages from the router and dispatches them.
func (mw *mockWorker) readLoop() {
	defer close(mw.doneCh)

	for {
		_, data, err := mw.conn.ReadMessage()
		if err != nil {
			return
		}

		msg, err := protocol.ParseMessage(data)
		if err != nil {
			continue
		}

		switch m := msg.(type) {
		case protocol.JobMessage:
			// Guard against send-on-closed-channel race: close() may have
			// already closed jobsCh while we were blocked on ReadMessage.
			mw.mu.Lock()
			closed := mw.closed
			mw.mu.Unlock()
			if closed {
				return
			}
			mw.jobsCh <- m
		case protocol.CancelMessage:
			// Could implement cancellation if needed
		}
	}
}

// processJobs handles jobs from the channel — sends tokens and completion.
func (mw *mockWorker) processJobs() {
	for job := range mw.jobsCh {
		if mw.responseDelay > 0 {
			time.Sleep(mw.responseDelay)
		}

		// Send tokens
		for _, token := range mw.tokens {
			tokenMsg := protocol.TokenMessage{
				Type:  protocol.TypeToken,
				JobID: job.JobID,
				Token: token,
			}
			data, _ := json.Marshal(tokenMsg)
			mw.mu.Lock()
			if !mw.closed {
				mw.conn.WriteMessage(websocket.TextMessage, data)
			}
			mw.mu.Unlock()
		}

		// Send complete
		outputCount := mw.outputTokens
		if outputCount == 0 {
			outputCount = len(mw.tokens)
		}
		completeMsg := protocol.CompleteMessage{
			Type:         protocol.TypeComplete,
			JobID:        job.JobID,
			FullResponse: strings.Join(mw.tokens, ""),
			InputTokens:  mw.inputTokens,
			OutputTokens: outputCount,
			DurationMS:   100,
		}
		data, _ := json.Marshal(completeMsg)
		mw.mu.Lock()
		if !mw.closed {
			mw.conn.WriteMessage(websocket.TextMessage, data)
		}
		mw.mu.Unlock()
	}
}

// close disconnects the mock worker.
func (mw *mockWorker) close() {
	mw.mu.Lock()
	mw.closed = true
	mw.mu.Unlock()

	// Close the WebSocket first — this unblocks readLoop's ReadMessage call
	// so it exits before we close the jobs channel.
	if mw.conn != nil {
		mw.conn.Close()
	}
	<-mw.doneCh // Wait for readLoop to exit (it returns on read error)

	// Close the jobs channel — processJobs will drain remaining items and exit.
	close(mw.jobsCh)
}

// sendJSON marshals and sends a message to the router.
func (mw *mockWorker) sendJSON(t *testing.T, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling message: %v", err)
	}
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if err := mw.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("sending message: %v", err)
	}
}

// waitForWorker polls the health endpoint until a worker appears (or timeout).
func waitForWorker(t *testing.T, env *testEnv, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(env.baseURL() + "/health")
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()

		var health map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&health)

		if workers, ok := health["workers"].(float64); ok && workers > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for worker to connect")
}
