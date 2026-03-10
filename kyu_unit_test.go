package kyu

import (
	"context"
	"errors"
	"log"
	"testing"
	"time"
)

type testWriter struct {
	t *testing.T
}

func (tw testWriter) Write(p []byte) (int, error) {
	tw.t.Log(string(p))
	return len(p), nil
}

// ---------------------------------------------------------------------------
// Config.withDefaults
// ---------------------------------------------------------------------------

func TestConfigDefaults_AllZero(t *testing.T) {
	cfg := Config{}.withDefaults()

	if cfg.DSN == "" {
		t.Error("DSN should not be empty")
	}
	if cfg.RedisAddr == "" {
		t.Error("RedisAddr should not be empty")
	}
	if cfg.Workers <= 0 {
		t.Errorf("Workers should be > 0, got %d", cfg.Workers)
	}
	if cfg.MetricsPort <= 0 {
		t.Errorf("MetricsPort should be > 0, got %d", cfg.MetricsPort)
	}
	if cfg.Logger == nil {
		t.Error("Logger should not be nil")
	}
}

func TestConfigDefaults_PartialOverride(t *testing.T) {
	cfg := Config{
		Workers:     10,
		MetricsPort: 2112,
	}.withDefaults()

	if cfg.Workers != 10 {
		t.Errorf("expected Workers=10, got %d", cfg.Workers)
	}
	if cfg.MetricsPort != 2112 {
		t.Errorf("expected MetricsPort=2112, got %d", cfg.MetricsPort)
	}
	// non-overridden fields still get defaults
	if cfg.RedisAddr == "" {
		t.Error("RedisAddr should have a default")
	}
}

func TestConfigDefaults_ZeroMetricsPort_GetsDefault(t *testing.T) {
	cfg := Config{MetricsPort: 0}.withDefaults()
	if cfg.MetricsPort != 9090 {
		t.Errorf("expected default MetricsPort=9090, got %d", cfg.MetricsPort)
	}
}

func TestConfigDefaults_NegativeWorkers_GetsDefault(t *testing.T) {
	cfg := Config{Workers: -3}.withDefaults()
	if cfg.Workers != 5 {
		t.Errorf("expected default Workers=5, got %d", cfg.Workers)
	}
}

func TestConfigDefaults_ExplicitValuesNotOverwritten(t *testing.T) {
	cfg := Config{
		DSN:         "postgres://custom/db",
		RedisAddr:   "redis-host:6380",
		Workers:     3,
		MetricsPort: 8888,
	}.withDefaults()

	if cfg.DSN != "postgres://custom/db" {
		t.Errorf("DSN overwritten: got %q", cfg.DSN)
	}
	if cfg.RedisAddr != "redis-host:6380" {
		t.Errorf("RedisAddr overwritten: got %q", cfg.RedisAddr)
	}
	if cfg.Workers != 3 {
		t.Errorf("Workers overwritten: got %d", cfg.Workers)
	}
	if cfg.MetricsPort != 8888 {
		t.Errorf("MetricsPort overwritten: got %d", cfg.MetricsPort)
	}
}

// ---------------------------------------------------------------------------
// Queue.Register and execute
// ---------------------------------------------------------------------------

func newTestQueue() *Queue {
	return New(Config{
		DSN:         "postgres://unused/test",
		RedisAddr:   "localhost:6379",
		Workers:     1,
		MetricsPort: 0, // disabled
	})
}

func TestRegister_HandlerIsCalled(t *testing.T) {
	q := newTestQueue()
	called := false

	q.Register("test_job", func(ctx context.Context, payload string) error {
		called = true
		return nil
	})

	if err := q.execute(context.Background(), "test_job", "hello", time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("handler was not called")
	}
}

func TestRegister_PayloadPassedThrough(t *testing.T) {
	q := newTestQueue()
	var got string

	q.Register("echo", func(ctx context.Context, payload string) error {
		got = payload
		return nil
	})

	_ = q.execute(context.Background(), "echo", "my-payload", time.Second)
	if got != "my-payload" {
		t.Errorf("expected payload %q, got %q", "my-payload", got)
	}
}

func TestExecute_UnknownJobType_ReturnsError(t *testing.T) {
	q := newTestQueue()
	err := q.execute(context.Background(), "nonexistent", "data", time.Second)
	if err == nil {
		t.Fatal("expected error for unknown job type, got nil")
	}
}

func TestExecute_HandlerError_Propagated(t *testing.T) {
	q := newTestQueue()
	sentinel := errors.New("handler failed")

	q.Register("bad_job", func(ctx context.Context, payload string) error {
		return sentinel
	})

	err := q.execute(context.Background(), "bad_job", "x", time.Second)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

func TestRegister_OverwritesExistingHandler(t *testing.T) {
	q := newTestQueue()

	q.Register("job", func(ctx context.Context, payload string) error {
		return errors.New("old handler")
	})
	q.Register("job", func(ctx context.Context, payload string) error {
		return nil
	})

	if err := q.execute(context.Background(), "job", "x", time.Second); err != nil {
		t.Errorf("expected new handler (nil error), got: %v", err)
	}
}

func TestRegister_MultipleTypes(t *testing.T) {
	q := newTestQueue()
	results := map[string]string{}

	for _, name := range []string{"a", "b", "c"} {
		n := name // capture
		q.Register(n, func(ctx context.Context, payload string) error {
			results[n] = payload
			return nil
		})
	}

	for _, name := range []string{"a", "b", "c"} {
		if err := q.execute(context.Background(), name, name+"-payload", time.Second); err != nil {
			t.Errorf("execute %q: %v", name, err)
		}
	}

	for _, name := range []string{"a", "b", "c"} {
		if results[name] != name+"-payload" {
			t.Errorf("job %q: got payload %q", name, results[name])
		}
	}
}

// ---------------------------------------------------------------------------
// metrics
// ---------------------------------------------------------------------------

func TestNewMetrics_DoesNotPanic(t *testing.T) {
	// Two calls must not panic (each uses its own registry).
	_ = newMetrics()
	_ = newMetrics()
}

func TestNewMetrics_CountersStartAtZero(t *testing.T) {
	m := newMetrics()

	// Increment and check it doesn't panic — the prometheus API doesn't expose
	// a direct read on Counter without a gathering round-trip, so we just
	// exercise the happy path.
	m.jobsProcessed.WithLabelValues("completed").Inc()
	m.jobFailures.WithLabelValues("send_email").Inc()
	m.jobsDeadTotal.Inc()
	m.jobTotal.Inc()
	m.queueDepth.Set(3)
	m.queueDepth.Dec()
}

func TestExecute_Timeout(t *testing.T) {
	q := newTestQueue()
	q.Register("slow_job", func(ctx context.Context, payload string) error {
		select {
		case <-time.After(200 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	ctx := context.Background()
	err := q.execute(ctx, "slow_job", "", 100*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestRunOnce_RequiresConnect(t *testing.T) {
	q := newTestQueue()
	err := q.RunOnce(context.Background())
	if err == nil {
		t.Error("expected error when not connected")
	}
}

// ---------------------------------------------------------------------------
// Full integration tests (require PostgreSQL and Redis running)
// ---------------------------------------------------------------------------

func TestIntegration_EnqueueAndProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := log.New(testWriter{t}, "", 0)
	q := New(Config{
		DSN:         "postgres://postgres:2005code@localhost:5432/jobscheduler?sslmode=disable",
		RedisAddr:   "localhost:6380",
		Workers:     1,
		MetricsPort: 0,
		Logger:      logger,
		QueueName:   "kyu:test_enqueue",
	})
	q.Register("test_job", func(ctx context.Context, payload string) error {
		return nil
	})
	if err := q.Connect(ctx); err != nil {
		t.Skipf("skipping: cannot connect: %v", err)
	}
	// flush stale state
	q.rdb.client.Del(ctx, q.cfg.QueueName)
	q.db.conn.Exec("DELETE FROM jobs")

	var processed bool
	q.Register("test_job", func(ctx context.Context, payload string) error {
		processed = true
		return nil
	})
	_, err := q.Enqueue(ctx, "test_job", "test-payload", EnqueueOptions{MaxRetries: 0})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	go q.Start(ctx)
	time.Sleep(5 * time.Second)
	if !processed {
		t.Error("job was not processed")
	}
}

func TestIntegration_MiddlewareCalled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := log.New(testWriter{t}, "", 0)
	q := New(Config{
		DSN:         "postgres://postgres:2005code@localhost:5432/jobscheduler?sslmode=disable",
		RedisAddr:   "localhost:6380",
		Workers:     1,
		MetricsPort: 0,
		Logger:      logger,
		QueueName:   "kyu:test_middleware",
	})
	var mwCalled, handlerCalled bool
	q.Use(func(ctx context.Context, jobtype, payload string, next func() error) error {
		mwCalled = true
		return next()
	})
	q.Register("mw_job", func(ctx context.Context, payload string) error {
		handlerCalled = true
		return nil
	})
	if err := q.Connect(ctx); err != nil {
		t.Skipf("skipping: cannot connect: %v", err)
	}
	// flush stale state
	q.rdb.client.Del(ctx, q.cfg.QueueName)
	q.db.conn.Exec("DELETE FROM jobs")

	if _, err := q.Enqueue(ctx, "mw_job", "payload", EnqueueOptions{MaxRetries: 0}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	go q.Start(ctx)
	time.Sleep(5 * time.Second)
	if !mwCalled {
		t.Error("middleware was not called")
	}
	if !handlerCalled {
		t.Error("handler was not called")
	}
}
func BenchmarkRegister(b *testing.B) {
	q := newTestQueue()
	handler := func(ctx context.Context, payload string) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Register("bench_job", handler)
	}
}

func BenchmarkExecute(b *testing.B) {
	q := newTestQueue()
	q.Register("bench_job", func(ctx context.Context, payload string) error { return nil })
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.execute(ctx, "bench_job", "payload", time.Second)
	}
}

func BenchmarkExecuteWithMiddleware(b *testing.B) {
	q := newTestQueue()
	q.Use(func(ctx context.Context, jobtype, payload string, next func() error) error { return next() })
	q.Use(func(ctx context.Context, jobtype, payload string, next func() error) error { return next() })
	q.Register("bench_job", func(ctx context.Context, payload string) error { return nil })
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.execute(ctx, "bench_job", "payload", time.Second)
	}
}

func BenchmarkExecuteParallel(b *testing.B) {
	q := newTestQueue()
	q.Register("bench_job", func(ctx context.Context, payload string) error { return nil })
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.execute(ctx, "bench_job", "payload", time.Second)
		}
	})
}
