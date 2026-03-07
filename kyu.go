// Package kyu is an importable distributed job queue library backed by
// PostgreSQL (persistence) and Redis (queue/priority sorted-set).
package kyu

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config holds all tunable parameters for a Queue.
// Zero-value fields fall back to sensible defaults applied by New.
type Config struct {
	// DSN is the PostgreSQL connection string.
	// Default: "postgres://user:password@localhost:5432/kyu?sslmode=disable"
	DSN string

	// RedisAddr is the Redis host:port address.
	// Default: "localhost:6379"
	RedisAddr string

	// Workers is the number of concurrent worker goroutines.
	// Default: 5
	Workers int

	// MetricsPort is the port the Prometheus /metrics HTTP server listens on.
	// Set to 0 to disable the metrics server.
	// Default: 9090
	MetricsPort int

	// Logger is used for internal diagnostic messages.
	// Defaults to the standard library logger when nil.
	Logger *log.Logger
}

func (c Config) withDefaults() Config {
	out := c
	if out.DSN == "" {
		out.DSN = "postgres://user:password@localhost:5432/kyu?sslmode=disable"
	}
	if out.RedisAddr == "" {
		out.RedisAddr = "localhost:6379"
	}
	if out.Workers <= 0 {
		out.Workers = 5
	}
	if out.MetricsPort == 0 {
		out.MetricsPort = 9090
	}
	if out.Logger == nil {
		out.Logger = log.Default()
	}
	return out
}

// Queue is the main handle for the kyu job queue library.
// Create one with New, register handlers with Register, then call Start.
type Queue struct {
	cfg      Config
	registry map[string]func(string) error
	mu       sync.RWMutex // protects registry

	db  *db
	rdb *rdb
	met *metrics
}

// New creates a new Queue with the given configuration.
// Defaults are applied for any zero-value fields; see Config for details.
// New does NOT open any connections — that happens inside Start.
func New(cfg Config) *Queue {
	return &Queue{
		cfg:      cfg.withDefaults(),
		registry: make(map[string]func(string) error),
	}
}

// Register associates a handler function with a named job type.
// Register is safe to call concurrently and may be called before or after Start.
func (q *Queue) Register(jobType string, handler func(payload string) error) {
	q.mu.Lock()
	q.registry[jobType] = handler
	q.mu.Unlock()
}

// Start opens database and Redis connections, runs auto-migration, then
// launches the worker pool, scheduler, and (optionally) the metrics HTTP server
// as goroutines.
//
// Start blocks until the provided context is cancelled, at which point it
// performs a graceful shutdown and returns any accumulated error. If multiple
// subsystems fail, only the first error is returned.
func (q *Queue) Start(ctx context.Context) error {
	// --- Postgres ---
	pgDB, err := connectDB(q.cfg.DSN)
	if err != nil {
		return fmt.Errorf("kyu: postgres: %w", err)
	}
	q.db = pgDB

	if err := q.db.migrate(&job{}); err != nil {
		return fmt.Errorf("kyu: migration: %w", err)
	}

	// --- Redis ---
	redisClient, err := connectRedis(ctx, q.cfg.RedisAddr)
	if err != nil {
		return fmt.Errorf("kyu: redis: %w", err)
	}
	q.rdb = redisClient

	// --- Metrics ---
	q.met = newMetrics()

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	// Metrics HTTP server
	if q.cfg.MetricsPort > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := q.runMetricsServer(ctx, q.cfg.MetricsPort); err != nil {
				errCh <- fmt.Errorf("kyu: metrics server: %w", err)
			}
		}()
	}

	// Worker pool
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.runWorkerPool(ctx, q.cfg.Workers)
	}()

	// Scheduler
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.runScheduler(ctx)
	}()

	// Wait for context cancellation, then drain
	<-ctx.Done()
	wg.Wait()

	// Return the first error, if any
	close(errCh)
	for e := range errCh {
		return e
	}
	return nil
}

// runMetricsServer starts a minimal HTTP server that exposes the Prometheus
// /metrics endpoint. It shuts down cleanly when ctx is cancelled.
func (q *Queue) runMetricsServer(ctx context.Context, port int) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(q.met.registry, promhttp.HandlerOpts{}))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	srvErr := make(chan error, 1)
	go func() {
		q.cfg.Logger.Printf("kyu: metrics server listening on :%d", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
		close(srvErr)
	}()

	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	case err := <-srvErr:
		return err
	}
}
