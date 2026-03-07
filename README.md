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

## File structure

```
kyu.go          public API — Config, Queue, New, Register, Start
worker.go       worker pool, scheduler (all unexported)
models.go       job GORM model (unexported)
db.go           postgres connect + migrate (unexported)
redis.go        redis connect (unexported)
metrics.go      prometheus metrics (unexported)
kyu_unit_test.go  unit tests (no infrastructure required)
cmd/example/main.go  runnable example
```

Everything inside the library is unexported. The only surface area is `Config`, `Queue`, `New`, `Register`, and `Start`.

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
