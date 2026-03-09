# kyu

A distributed job queue library for Go, backed by PostgreSQL and Redis.

PostgreSQL stores every job and its full history. Redis acts as the priority queue — workers pop job IDs from a sorted set and fetch the full record from Postgres to process. This means jobs survive a Redis restart because the source of truth is always the database.

---

## How it works

```
Your app
   │
   ├── q.Register("send_email", handler)   registers a handler for a job type
   │
   └── q.Start(ctx)                        blocks until ctx is cancelled
              │
              ├── Postgres                 stores jobs, status, retries, errors
              │
              ├── Redis sorted set         jobs:pending  (score = priority)
              │        │
              │        └── workers poll ZPopMin every 500ms
              │                 │
              │                 ├── fetch full job from Postgres
              │                 ├── mark running
              │                 ├── call handler(payload)
              │                 ├── on success  → mark completed
              │                 ├── on error + retries left → mark failed, re-enqueue
              │                 └── on error + no retries   → mark dead
              │
              ├── Scheduler (every 5s)     queries Postgres for scheduled jobs
              │                            whose scheduled_at <= now(), pushes
              │                            their IDs into Redis
              │
              └── Metrics server           Prometheus /metrics on MetricsPort
```

---

## Installation

```bash
go get github.com/codetesla51/kyu
```

Requires PostgreSQL and Redis. The library creates the `jobs` table automatically via GORM AutoMigrate on first `Start`.

---

## Quick start

```go
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"

    "github.com/codetesla51/kyu"
)

func main() {
    q := kyu.New(kyu.Config{
        DSN:         "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
        RedisAddr:   "localhost:6379",
        Workers:     5,
        MetricsPort: 9090,
    })

    q.Register("send_email", func(payload string) error {
        // payload is whatever string you stored when creating the job
        log.Printf("sending email: %s", payload)
        return nil
    })

    q.Register("resize_image", func(payload string) error {
        // return an error to trigger retry
        return nil
    })

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    if err := q.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

`Start` blocks. When you hit Ctrl-C the context is cancelled, workers finish their current jobs, the scheduler and metrics server shut down, and `Start` returns.

---

## Config

```go
kyu.Config{
    DSN:         "postgres://...",   // PostgreSQL connection string
    RedisAddr:   "localhost:6379",   // Redis host:port
    Workers:     5,                  // number of concurrent worker goroutines
    MetricsPort: 9090,               // Prometheus metrics port, 0 = disabled
    Logger:      log.Default(),      // any *log.Logger
}
```

All fields have defaults — `kyu.New(kyu.Config{})` is valid and will connect to local Postgres and Redis with 5 workers.

---

## Submitting jobs

There is currently no `Enqueue` method on the library (see [What's missing](#whats-missing)). To submit a job you write directly to Postgres and Redis:

```go
// 1. insert the job row into postgres
job := Job{
    ID:         uuid.New().String(),
    JobType:    "send_email",
    Payload:    "user@example.com",
    Status:     "pending",
    MaxRetries: 3,
    Priority:   0,         // lower score = higher priority in Redis sorted set
}
db.Create(&job)

// 2. push the job ID into redis
redis.ZAdd(ctx, "jobs:pending", redis.Z{
    Score:  float64(job.Priority),
    Member: job.ID,
})
```

For scheduled jobs, set `ScheduledAt` to a future time and skip the Redis push. The scheduler will pick it up automatically when the time arrives.

---

## Job lifecycle

```
pending
   │
   ├── (scheduler pushes to Redis if scheduled_at reached)
   │
   └── running
          │
          ├── completed       handler returned nil
          │
          ├── failed          handler returned error, retries remain
          │      └── (re-enqueued in Redis, retry_count incremented)
          │
          └── dead            handler returned error, no retries left
```

The `jobs` table columns:

| Column | Description |
|---|---|
| `id` | UUID string, primary key |
| `job_type` | matches the name passed to `Register` |
| `payload` | arbitrary string passed to the handler |
| `status` | `pending`, `running`, `failed`, `completed`, `dead` |
| `priority` | lower number = higher priority (Redis score) |
| `scheduled_at` | if set, job won't run until this time |
| `max_retries` | how many times to retry on failure |
| `retry_count` | how many times it has been retried so far |
| `error_message` | last error returned by the handler |
| `locked_by` | which worker is currently running it, e.g. `worker-3` |
| `locked_at` | when the worker locked it |
| `completed_at` | when it finished successfully |

---

## Metrics

When `MetricsPort` is set, a Prometheus `/metrics` endpoint is available. Metrics exposed:

| Metric | Type | Description |
|---|---|---|
| `kyu_jobs_processed_total` | counter | jobs completed, labelled by `status` |
| `kyu_job_failures_total` | counter | failures labelled by `job_type` |
| `kyu_jobs_dead_total` | counter | jobs that exhausted all retries |
| `kyu_queue_depth` | gauge | current size of the pending queue |
| `kyu_jobs_total` | counter | total jobs ever submitted |

Scrape config for Prometheus:

```yaml
scrape_configs:
  - job_name: kyu
    static_configs:
      - targets: ["localhost:9090"]
```

---

## Codebase reference

This section is a file-by-file walkthrough of what's actually in the repository and why it's structured the way it is. It is written for someone who understands the original service architecture and wants to understand what changed and where things live now.

### From service to library

The original version of this was a standalone HTTP service — its own `main` package, Gin routes, `godotenv` loading `.env` files, JWT auth, a `Dockerfile` that built a binary named `server`. The refactor stripped all of that out and turned the queue logic into an importable library.

The structural consequence is that the root of the repo is now `package kyu`, not `package main`. All the connection management, worker logic, and metrics that used to be wired up in a `main()` are now wired up inside `Start()`. The caller's `main` just calls `New`, registers handlers, and calls `Start`. The library owns its goroutines, its connections, and its shutdown sequence — the caller only needs to cancel the context.

A handful of artefacts from the service phase are still in the repo: `Dockerfile` (builds a `./server` binary that no longer exists), `docker-compose.yml` (exposes `app` on port 8080, but the library's default metrics port is 9090), `.env` files (contain `JWT_SECRET` and `PORT` that nothing in the library reads), `go.mod` (still declares `gin`, `uuid`, and `godotenv` as direct dependencies even though none of them are imported by any library file), and an empty `init.sql/` directory (replaced by GORM `AutoMigrate`).

---

### `kyu.go` — public API

This is the only file a caller ever needs to understand. Everything exported lives here.

**`Config`** is a flat struct of tunable parameters. All fields have zero values that `withDefaults()` fills in — Postgres DSN, Redis address, worker count (5), metrics port (9090), and a `*log.Logger`. `withDefaults()` is unexported and called inside `New`, so callers never call it directly. The defaults are deliberately local-dev friendly: `localhost` addresses, `sslmode=disable`.

**`Queue`** is an opaque handle. The fields are all unexported: `cfg`, `registry` (a `map[string]func(string) error`), a `sync.RWMutex` protecting the registry, and typed wrappers for the three infrastructure connections (`*db`, `*rdb`, `*metrics`). Callers hold a `*Queue` and call methods on it. They can't reach inside.

**`New(cfg Config) *Queue`** applies defaults and initialises the registry map. It does not open any connections — that happens in `Start`. This separation matters because it lets you call `Register` before any infrastructure is available, and it keeps construction cheap enough to use in tests without mocking anything.

**`Register(jobType string, handler func(string) error)`** stores a handler under a string key. Thread-safe via a write lock on `q.mu`. Can be called before or after `Start` — the registry lookup in `execute` uses a read lock, so late registration is safe.

**`Start(ctx context.Context) error`** is the entire startup sequence:
1. Opens Postgres, runs `AutoMigrate` on the `job` model.
2. Connects to Redis, pings it.
3. Creates the private Prometheus registry and registers all counters.
4. Launches three goroutines under a `sync.WaitGroup`: `runMetricsServer`, `runWorkerPool`, `runScheduler`. Each one receives the context and sends any fatal error into a buffered `errCh`.
5. Blocks on `<-ctx.Done()`.
6. Calls `wg.Wait()` to drain all goroutines.
7. Returns the first error from `errCh`, or `nil`.

The `errCh` is buffered to 3 (one slot per goroutine) so goroutines are never blocked trying to send after shutdown.

**`runMetricsServer(ctx, port int)`** is an unexported method that starts an `http.Server` with `promhttp.HandlerFor` pointed at the library's private registry (not `prometheus.DefaultRegisterer`). On context cancellation it calls `server.Shutdown` with a 5-second timeout. The separate registry is what allows multiple `Queue` instances in the same process without a "duplicate registration" panic.

---

### `worker.go` — worker pool and scheduler

All unexported. This is where all the runtime logic lives.

**`execute(jobType, payload string) error`** is a one-liner: acquire a read lock, look up the handler in `q.registry`, release the lock, call it. If the job type isn't registered it returns a plain `fmt.Errorf` — no panic. This is the only place the registry is read.

**`runWorkerPool(ctx, numWorkers int)`** spawns `numWorkers` goroutines each running `runWorker`, then calls `wg.Wait()`. Nothing more. The per-worker state is just the worker ID (an integer).

**`runWorker(ctx, workerID int)`** is the main loop. On each iteration:
- Check `ctx.Err()` first — clean exit before touching Redis.
- `ZPopMin("jobs:pending")` — atomically pop the lowest-score member. Non-blocking; returns immediately with nothing if the queue is empty.
- Empty queue: sleep 500ms and loop.
- Redis error: log it, sleep 1s.
- Got a job ID: fetch the full `job` row from Postgres, update status to `running` with `LockedAt` and `LockedBy = "worker-N"`, call `execute`.
- On success: `UPDATE` status to `completed`, set `completed_at`, increment `kyu_jobs_processed_total{status="completed"}`.
- On failure with retries left: `UPDATE` status to `failed`, increment `retry_count`, `ZAdd` the job ID back into `jobs:pending` at its original priority. Increment `kyu_job_failures_total{job_type=...}`.
- On failure with no retries: `UPDATE` status to `dead`, record `error_message`. Increment `kyu_jobs_dead_total`.

The `locked_by` value is `fmt.Sprintf("worker-%d", workerID)`. `locked_at` is recorded but nothing currently uses it for stale lock detection — it's informational.

**`runScheduler(ctx)`** ticks on a 5-second `time.Ticker`. On each tick it runs:

```sql
SELECT * FROM jobs WHERE status = 'pending' AND scheduled_at <= NOW()
```

using the composite index `idx_status_scheduled` on `(status, scheduled_at)`. For each row it calls `ZAdd` to push the job ID into `jobs:pending` with the job's priority as the score. Redis sorted sets deduplicate by member so calling `ZAdd` for an already-queued job is safe — it just updates the score.

The reason the scheduler queries `status = 'pending'` rather than a dedicated `'scheduled'` status is that there is no distinct scheduled status yet. All unstarted jobs are `pending` regardless of whether they have a `scheduled_at`. The scheduler relies on `scheduled_at IS NOT NULL AND scheduled_at <= NOW()` to filter correctly, but the query as written will also match plain `pending` jobs that have no `scheduled_at` — which is harmless because they should already be in Redis.

---

### `models.go` — job model

The `job` struct is unexported. Callers never import or instantiate it. GORM sees it via the `AutoMigrate` call in `Start`.

The fields map directly to the table described in the lifecycle section above. A few structural notes:

- `gorm.DeletedAt` enables soft deletes. Completed and dead jobs are never hard-deleted by the library; they stay in the table for audit purposes (or until you clean them yourself).
- The composite index `idx_status_scheduled` on `(status, scheduled_at)` is the only non-trivial index. It exists specifically for the scheduler query. Without it the scheduler does a full table scan every 5 seconds.
- `LockedAt *time.Time` and `LockedBy string` are written when a worker picks up a job. There is currently no stale lock detection — if a worker crashes mid-job the row stays in `running` forever. These fields exist so you could build detection on top (e.g., a cleanup query for jobs locked more than N minutes ago).

---

### `db.go` — Postgres connection

A minimal wrapper around `*gorm.DB` to avoid the struct field being named `gorm` (which would collide with the import alias). Two methods:

**`connectDB(dsn string) (*db, error)`** calls `gorm.Open(postgres.Open(dsn))` with default config. No connection pool tuning — uses GORM's defaults.

**`(d *db) migrate(models ...interface{}) error`** calls `d.conn.AutoMigrate(models...)`. It's variadic to leave room to add models later, but currently only `&job{}` is passed.

---

### `redis.go` — Redis connection

**`connectRedis(ctx context.Context, addr string) (*rdb, error)`** creates a `go-redis/v9` client and pings it. The ping uses a separate `context.WithTimeout(context.Background(), 5*time.Second)` rather than the caller's context — this is intentional. The caller's context might be near cancellation at startup (race between cancel and start), and the ping just needs to verify connectivity, not respect the application lifetime.

No connection pool configuration is exposed. `go-redis` defaults to a pool size of 10 — fine for development, probably too small for high-throughput production use.

---

### `metrics.go` — Prometheus metrics

**`newMetrics() *metrics`** creates a fresh `*prometheus.Registry` (not `DefaultRegisterer`) and registers five collectors on it, plus `NewGoCollector()` and `NewProcessCollector()` for Go runtime and OS process metrics.

The private registry is the key design decision here. The standard pattern of registering on `prometheus.DefaultRegisterer` breaks as soon as you have two `Queue` instances in the same process — the second `Register` call panics with "duplicate metrics collector registration". Using a private registry per `Queue` avoids this entirely. The tradeoff is that the metrics don't appear in a default `/metrics` endpoint — the library starts its own HTTP server pointed at its own registry, which is why `runMetricsServer` uses `promhttp.HandlerFor(q.met.registry, ...)` rather than `promhttp.Handler()`.

The five metrics:
- `kyu_jobs_processed_total` — `CounterVec` with a `status` label. Currently only incremented with `status="completed"`.
- `kyu_job_failures_total` — `CounterVec` with a `job_type` label. Incremented on each failed attempt that has retries left.
- `kyu_jobs_dead_total` — plain `Counter`. Incremented when a job exhausts all retries.
- `kyu_queue_depth` — `Gauge`. Declared but not currently updated anywhere in the source — it's a stub.
- `kyu_jobs_total` — plain `Counter`. Declared but not currently updated anywhere in the source — also a stub.

---

### `kyu_unit_test.go` — unit tests

`package kyu` — white-box tests in the same package. No Postgres or Redis; nothing in any test opens a real connection.

**Config tests** exercise `withDefaults()` for zero values, partial overrides (explicit fields survive), negative `Workers` (treated as zero, gets default), and zero `MetricsPort` (gets default 9090).

**Registry and dispatch tests** exercise `Register` and `execute` directly. The key cases: handler is called with the right payload, unknown job type returns an error, handler errors propagate via `errors.Is`, re-registering the same job type overwrites the previous handler.

**Metrics tests** verify that calling `newMetrics()` twice does not panic (proving the private registry approach works) and that all metric operations can be called without error.

`newTestQueue()` is a helper that creates a `Queue` with `MetricsPort: 0`. This prevents any test from starting a real HTTP server.

---

### `cmd/example/main.go` — runnable example

A minimal `main` package showing the full library usage pattern. Reads `DATABASE_URL` and `REDIS_URL` from environment (with `localhost` fallbacks via `envOr`), registers two handlers — `send_email` (sleeps 2s, succeeds) and `failing_job` (always returns an error to exercise the retry/dead path) — sets up signal handling with `signal.NotifyContext`, and calls `q.Start(ctx)`.

This is the right shape for any application embedding kyu: env vars for connection strings, signal context, `Start` as the last call in `main`.

---

### `go.mod` — declared vs. actual dependencies

The module is `github.com/codetesla51/kyu`. Direct dependencies declared:

| Dependency | Used | Notes |
|---|---|---|
| `gorm.io/gorm` | yes | ORM |
| `gorm.io/driver/postgres` | yes | GORM Postgres driver |
| `github.com/redis/go-redis/v9` | yes | Redis client |
| `github.com/prometheus/client_golang` | yes | Prometheus |
| `github.com/gin-gonic/gin` | **no** | Leftover from service phase |
| `github.com/google/uuid` | **no** | Leftover — callers generate UUIDs, library does not |
| `github.com/joho/godotenv` | **no** | Leftover — library does not read `.env` files |

`gin`, `uuid`, and `godotenv` can be removed with `go mod tidy`. They add nothing to the library binary and increase the dependency surface needlessly.

---

### Infrastructure files

**`Dockerfile`** is a two-stage build: `golang:1.25` builder compiles with `CGO_ENABLED=0 GOOS=linux -ldflags="-s -w"`, output binary named `server` is copied into `alpine:3.19`. This was written for the service phase. It will not build the current code as-is — `go build -o /app/server .` on a library package (no `main` in root) is a compile error. Needs to be updated to `go build -o /app/server ./cmd/example` or removed.

**`docker-compose.yml`** brings up six services: the app binary, Postgres 16, Redis 7, Prometheus, and Grafana. The `app` service passes `DATABASE_URL` and `REDIS_URL` via environment variables, which `cmd/example/main.go` reads. Port mapping has Redis on `6380:6379` externally (useful if you have a local Redis already running on 6379). The `prometheus.yml` scrapes `app:8080` — this matches the `app` container's exposed port but not the library's default `MetricsPort` of 9090; the example binary would need an explicit `MetricsPort: 8080` in its `Config` for this to work, or `prometheus.yml` needs to point at port 9090.

**`prometheus.yml`** is a minimal scrape config pointing at `app:8080`. See the note above about the port mismatch.

**`.env.example`** is the template for local development. The actual `.env` (gitignored) and `.env.production` (not gitignored — contains hardcoded credentials) are both present in the repo. The production file should be gitignored or replaced with a secrets management solution.

---

## What's missing

These are known gaps. The library works without them but they would need to be added before this is production-ready.

**`Enqueue` method**

The most important missing piece. Right now callers have to write to Postgres and Redis manually. The fix is straightforward — add a method on `Queue`:

```go
func (q *Queue) Enqueue(ctx context.Context, jobType, payload string, opts EnqueueOptions) (string, error)

type EnqueueOptions struct {
    Priority    int
    MaxRetries  int
    ScheduledAt *time.Time
}
```

It would create the job row in Postgres and push the ID into Redis in one call, returning the job ID.

**Retry backoff**

Right now failed jobs go straight back into the queue and will be retried immediately. In practice you want exponential backoff — wait 1s, then 2s, then 4s etc. between retries. This would be implemented by setting `scheduled_at` on the retry instead of re-enqueuing into Redis directly.

**Scheduler deduplication**

The scheduler runs every 5 seconds and pushes all overdue scheduled jobs into Redis. If a job is already in Redis but hasn't been picked up yet, it gets added again. Redis sorted sets deduplicate by member so this doesn't cause double processing, but it does reset the score. A proper fix is to move jobs to a `scheduled` status distinct from `pending` so the scheduler can filter more precisely.

**Job visibility / status API**

There is no way to ask the library "what is the status of job X" or "show me all dead jobs". You have to query Postgres directly. An `Inspect(id string)` method and a `DeadJobs()` method would cover most use cases.

**Worker isolation**

All `Queue` instances in the same process share the `jobs:pending` Redis key. If you create two queues pointing at different Postgres databases, their workers will compete for jobs from the same Redis queue. The fix is to make the queue key configurable in `Config`, e.g. `QueueName string`.

**Connection pool tuning**

Both the Postgres and Redis connections use library defaults. For production you would want to expose `MaxOpenConns`, `MaxIdleConns`, `ConnMaxLifetime` for Postgres and `PoolSize` for Redis in `Config`.

**No `log.Fatal` — but panics are unhandled**

If a handler panics the worker goroutine dies silently. Workers should recover from panics, log them, and mark the job as failed.

**Incomplete metrics**

`kyu_queue_depth` and `kyu_jobs_total` are registered but never updated. `kyu_jobs_processed_total` only records `status="completed"` — the `failed` label is never used even though it's a `CounterVec`. These need wiring up before the metrics endpoint is trustworthy.

**`go.mod` cleanup**

`gin`, `uuid`, and `godotenv` are declared as direct dependencies but nothing in the library imports them. `go mod tidy` will remove them from `go.mod` and reduce the dependency graph.
