package router

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/auth"
	"github.com/auxothq/auxot/pkg/gpu"
	"github.com/auxothq/auxot/pkg/queue"
	"github.com/redis/go-redis/v9"
)

// Server is the top-level router server that owns all subsystems.
type Server struct {
	config      *Config
	httpServer  *http.Server
	sweeper     *queue.Sweeper
	wsHandler   *WSHandler
	apiHandler  *APIHandler
	pool        *gpu.Pool
	redisClient *redis.Client
	logger      *slog.Logger
}

// NewServer creates a fully wired router server from configuration.
//
// Architecture:
//   - Each GPU worker is a Redis consumer in the "workers" consumer group
//   - The WSHandler starts a job reader goroutine per worker (XREADGROUP)
//   - The Sweeper detects dead workers via heartbeat TTL and reclaims their jobs
//   - No centralized dispatcher — Redis consumer groups handle FIFO distribution
func NewServer(cfg *Config, logger *slog.Logger) (*Server, error) {
	// --- Redis ---
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parsing REDIS_URL: %w", err)
	}
	redisClient := redis.NewClient(opts)

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("connecting to Redis: %w", err)
	}

	// --- Auth ---
	verifier := auth.NewVerifier(cfg.AdminKeyHash, cfg.APIKeyHash, 5*time.Minute)

	// --- GPU pool (connection tracking + health endpoint) ---
	pool := gpu.NewPool(logger.With("component", "gpu_pool"))

	// --- Redis Streams ---
	// Consumer group is "workers" — each GPU worker is a consumer with its workerID
	jobQueue := queue.NewJobQueue(redisClient, "auxot:jobs", "workers")
	tokenStream := queue.NewTokenStream(redisClient, "auxot:")

	// Ensure the job queue consumer group exists
	if err := jobQueue.EnsureGroup(context.Background()); err != nil {
		return nil, fmt.Errorf("creating job queue consumer group: %w", err)
	}

	// --- Heartbeat ---
	// TTL is DeadWorkerTimeout — if a heartbeat isn't refreshed within this
	// window, the worker is considered dead by the sweeper.
	heartbeat := queue.NewHeartbeat(redisClient, "auxot:", cfg.DeadWorkerTimeout)

	// --- Handlers ---
	wsHandler := NewWSHandler(
		verifier,
		pool,
		jobQueue,
		tokenStream,
		heartbeat,
		cfg,
		logger.With("component", "ws"),
	)

	apiHandler := NewAPIHandler(
		verifier,
		pool,
		jobQueue,
		tokenStream,
		cfg,
		logger.With("component", "api"),
	)

	anthropicHandler := NewAnthropicHandler(
		verifier,
		jobQueue,
		tokenStream,
		cfg,
		logger.With("component", "anthropic"),
	)

	// --- Sweeper ---
	// Periodically scans for dead consumers (heartbeat expired) and
	// re-enqueues their pending jobs for live workers.
	sweeper := queue.NewSweeper(
		jobQueue,
		heartbeat,
		15*time.Second, // Sweep interval
		logger.With("component", "sweeper"),
	)

	// --- HTTP mux ---
	mux := http.NewServeMux()

	// WebSocket endpoint for GPU workers
	mux.Handle("/ws", wsHandler)

	// OpenAI-compatible API — all routes under /api/openai/
	mux.Handle("/api/openai/", apiHandler)

	// Anthropic-compatible API — all routes under /api/anthropic/
	// StripPrefix removes /api/anthropic so the handler sees /v1/messages, etc.
	mux.Handle("/api/anthropic/", http.StripPrefix("/api/anthropic", anthropicHandler))

	// Health check (no auth, no prefix — infrastructure endpoint)
	mux.Handle("/health", apiHandler)

	// Catch-all
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErrorJSON(w, http.StatusNotFound, "not_found", "endpoint not found")
	})

	// Wrap the mux with v1-tolerance middleware.
	// Many LLMs reflexively add /v1 to base URLs. This absorbs that:
	//   /api/openai/v1/chat/completions  → /api/openai/chat/completions
	//   /api/anthropic/v1/v1/messages    → /api/anthropic/v1/messages
	handler := v1Tolerant(mux)

	listenAddr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return &Server{
		config:      cfg,
		httpServer:  httpServer,
		sweeper:     sweeper,
		wsHandler:   wsHandler,
		apiHandler:  apiHandler,
		pool:        pool,
		redisClient: redisClient,
		logger:      logger,
	}, nil
}

// Start begins serving HTTP connections and starts the background sweeper.
// It blocks until the context is cancelled or the server encounters an error.
func (s *Server) Start(ctx context.Context) error {
	// Start the dead-worker sweeper in a background goroutine
	sweeperCtx, sweeperCancel := context.WithCancel(ctx)
	defer sweeperCancel()

	go s.sweeper.Run(sweeperCtx)

	listenAddr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	s.logger.Info("auxot-router starting",
		"addr", listenAddr,
		"redis", s.config.RedisURL,
	)

	// Start HTTP server in a goroutine
	errCh := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal or error
	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("HTTP server error: %w", err)
		}
	}

	return s.Shutdown()
}

// Shutdown gracefully stops the server and cleans up resources.
func (s *Server) Shutdown() error {
	s.logger.Info("shutting down...")

	// Graceful HTTP shutdown with timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("HTTP shutdown error", "error", err)
	}

	// Close Redis connection
	if err := s.redisClient.Close(); err != nil {
		s.logger.Error("Redis close error", "error", err)
	}

	s.logger.Info("shutdown complete")
	return nil
}

// v1Tolerant is middleware that absorbs accidental /v1 in base URLs.
//
// LLMs and SDK defaults reflexively add /v1 to base URLs, producing paths like:
//
//	/api/openai/v1/chat/completions   (should be /api/openai/chat/completions)
//	/api/anthropic/v1/v1/messages     (should be /api/anthropic/v1/messages)
//
// This middleware rewrites the path before it reaches the mux so both forms work.
func v1Tolerant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		// /api/openai/v1/... → /api/openai/...
		// The canonical routes don't have /v1, so strip the extra one.
		if strings.HasPrefix(p, "/api/openai/v1/") {
			r.URL.Path = "/api/openai/" + strings.TrimPrefix(p, "/api/openai/v1/")
			if r.URL.RawPath != "" {
				r.URL.RawPath = "/api/openai/" + strings.TrimPrefix(r.URL.RawPath, "/api/openai/v1/")
			}
		}

		// /api/anthropic/v1/v1/... → /api/anthropic/v1/...
		// The canonical routes already include one /v1, so strip the doubled one.
		if strings.HasPrefix(p, "/api/anthropic/v1/v1/") {
			r.URL.Path = "/api/anthropic/v1/" + strings.TrimPrefix(p, "/api/anthropic/v1/v1/")
			if r.URL.RawPath != "" {
				r.URL.RawPath = "/api/anthropic/v1/" + strings.TrimPrefix(r.URL.RawPath, "/api/anthropic/v1/v1/")
			}
		}

		next.ServeHTTP(w, r)
	})
}
