# kyu

![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?style=flat&logo=postgresql)
![Redis](https://img.shields.io/badge/Redis-7-DC382D?style=flat&logo=redis)
![Prometheus](https://img.shields.io/badge/Prometheus-metrics-E6522C?style=flat&logo=prometheus)
![License](https://img.shields.io/badge/license-MIT-green?style=flat)

A distributed job queue library for Go, backed by PostgreSQL and Redis.

![Grafana Dashboard](docs/graphana.png)
*kyu jobs visualized on Grafana*

PostgreSQL is the source of truth - every job, its full history, retry count, and error message are persisted there. Redis acts as the priority queue - workers pop job IDs from a sorted set and fetch the full record from Postgres to process. Jobs survive a Redis restart because nothing is lost if the sorted set is cleared.

---

## Contents

- [How it works](#how-it-works)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Enqueuing jobs](#enqueuing-jobs)
- [Config](#config)
- [Registering handlers](#registering-handlers)
- [Middleware](#middleware)
- [Job lifecycle](#job-lifecycle)
- [Inspecting jobs](#inspecting-jobs)
- [RunOnce](#runonce-cron-mode)
- [Metrics](#metrics)
- [Benchmarks](#benchmarks)
- [Docker Compose](#docker-compose)
- [Running tests](#running-tests)


## How it works


Kyu has three components running concurrently once you call `Start`.

The **worker pool** pops job IDs from a Redis sorted set, fetches the full job record from Postgres, runs the registered handler, then updates the job status. Failed jobs with retries remaining are re-queued with exponential backoff. Failed jobs with no retries left are marked dead.

The **scheduler** ticks every 5 seconds and queries Postgres for scheduled or failed jobs whose time has arrived, pushing their IDs back into Redis.

The **stale reaper** ticks every minute and resets any job stuck in the `running` state longer than `StaleJobTimeout`. This handles workers that crashed mid-job.

The **metrics server** exposes a Prometheus `/metrics` endpoint on `MetricsPort`.

---

## Installation

```bash
go get github.com/codetesla51/kyu
```

Requires PostgreSQL and Redis. The `jobs` table is created automatically via GORM AutoMigrate on the first `Connect` call.

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

    q.Register("send_email", func(ctx context.Context, payload string) error {
        log.Printf("sending email: %s", payload)
        return nil
    })

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    if err := q.Connect(ctx); err != nil {
        log.Fatal(err)
    }

    if err := q.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

`Start` blocks. On SIGINT or SIGTERM the context is cancelled, workers finish their current jobs, the scheduler and metrics server shut down cleanly, and `Start` returns.

The required call order is: `New` -> `Register` -> `Connect` -> `Enqueue` / `Start`. `Connect` must be called before `Enqueue` or `Start` - both require an open database and Redis connection.

---

## Enqueuing jobs

Jobs are enqueued independently of `Start` - you can enqueue from a separate service, an HTTP handler, or anywhere you have access to the `Queue`.

```go
// run immediately
jobID, err := q.Enqueue(ctx, "send_email", `{"to":"user@example.com"}`, kyu.EnqueueOptions{
    MaxRetries: 3,
    Priority:   1,
})

// run after 1 minute
at := time.Now().Add(1 * time.Minute)
jobID, err := q.Enqueue(ctx, "send_email", `{"to":"user@example.com"}`, kyu.EnqueueOptions{
    MaxRetries:  3,
    Priority:    0,
    ScheduledAt: &at,   // pointer so nil means "no schedule, run now"
    TimeOut:     10 * time.Second,
})

// high priority - processed before lower priority jobs
jobID, err := q.Enqueue(ctx, "process_payment", `{"order_id":"123"}`, kyu.EnqueueOptions{
    MaxRetries: 5,
    Priority:   10, // higher score = picked up first
})
```

`ScheduledAt` is a pointer because `nil` means "run immediately" and a real value means "run at this time". A plain `time.Time` cannot represent the absence of a value.

`Priority` maps directly to the Redis sorted set score. Workers always pop the highest score first, so higher numbers are processed before lower ones.

---

## Config

```go
kyu.Config{
    // Required
    DSN:       "postgres://user:pass@localhost:5432/db?sslmode=disable",
    RedisAddr: "localhost:6379",

    // Worker pool
    Workers: 5, // number of concurrent goroutines processing jobs

    // Queue
    QueueName: "kyu:default", // Redis sorted set key - use different names to isolate queues

    // Metrics
    MetricsPort: 9090, // set to 0 to disable

    // Stale job reaper
    // A job stuck in "running" beyond this duration is reset to "pending"
    // and re-queued. This handles crashed workers.
    StaleJobTimeout: 10 * time.Minute,

    // Postgres connection pool
    MaxOpenConns:    25,
    MaxIdleConns:    25,
    ConnMaxLifetime: 5 * time.Minute,

    Logger: log.Default(),
}
```

All fields have defaults. `kyu.New(kyu.Config{})` connects to local Postgres and Redis with 5 workers.

If you are running multiple applications against the same Redis instance, set a unique `QueueName` per application. Workers compete for any job in their queue - two apps sharing the same queue name will process each other's jobs.

---

## Registering handlers

Handlers receive the context and the payload string you passed at enqueue time. Return an error to trigger a retry (if retries remain) or mark the job dead (if none remain).

```go
q.Register("send_email", func(ctx context.Context, payload string) error {
    var data struct {
        To      string `json:"to"`
        Subject string `json:"subject"`
    }
    if err := json.Unmarshal([]byte(payload), &data); err != nil {
        return err // will retry
    }
    return sendEmail(ctx, data.To, data.Subject)
})

q.Register("process_payment", func(ctx context.Context, payload string) error {
    // respect context cancellation for long-running work
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }
    return chargeCard(payload)
})
```

Handlers are safe to register concurrently. Registering the same job type twice overwrites the first handler.

---

## Middleware

Middleware wraps every job execution regardless of type. Middlewares are applied in registration order - the first registered is the outermost wrapper. In the example below, the logging middleware runs first, then timing, so the log line appears before the duration line.

```go
// order: logging wraps timing wraps handler
q.Use(loggingMiddleware)  // outermost
q.Use(timingMiddleware)   // inner
```

```go
// logging
q.Use(func(ctx context.Context, jobType, payload string, next func() error) error {
    log.Printf("job started: %s", jobType)
    err := next()
    if err != nil {
        log.Printf("job failed: %s: %v", jobType, err)
    }
    return err
})

// timing
q.Use(func(ctx context.Context, jobType, payload string, next func() error) error {
    start := time.Now()
    err := next()
    log.Printf("job=%s duration=%s", jobType, time.Since(start))
    return err
})

// panic recovery
q.Use(func(ctx context.Context, jobType, payload string, next func() error) error {
    defer func() {
        if r := recover(); r != nil {
            log.Printf("job panicked: %s: %v", jobType, r)
        }
    }()
    return next()
})
```

---

## Job lifecycle

```
pending
   |
   |-- (scheduler promotes to Redis when scheduled_at is reached)
   |
   `-- running
          |
          |-- completed       handler returned nil
          |
          |-- failed          handler returned error, retries remain
          |      `-- re-enqueued with exponential backoff (1s, 2s, 4s, ...)
          |
          |-- dead            handler returned error, no retries left
          |
          `-- cancelled       CancelJob was called before the job ran
```

Failed jobs use exponential backoff between retries - a job that has failed once waits 1 second, twice waits 2 seconds, three times waits 4 seconds, and so on. The scheduler picks them back up once their `scheduled_at` arrives.

Jobs table columns:

| Column        | Description                                              |
|---------------|----------------------------------------------------------|
| id            | UUID, primary key                                        |
| job_type      | matches the name passed to Register                      |
| payload       | arbitrary string passed to the handler                   |
| status        | pending, running, failed, completed, dead, cancelled     |
| priority      | higher score = picked up first                           |
| scheduled_at  | job will not run until this time                         |
| max_retries   | maximum retry attempts                                   |
| retry_count   | number of attempts so far                                |
| error_message | last error returned by the handler                       |
| locked_by     | which worker is running it, e.g. worker-3                |
| locked_at     | when the worker locked it                                |
| completed_at  | when it finished successfully                            |

---

## Inspecting jobs

```go
// get a single job by ID
job, err := q.Inspect(ctx, jobID)
log.Printf("status=%s retries=%d error=%s", job.Status, job.RetryCount, job.ErrorMessage)

// get all jobs that exhausted their retries
dead, err := q.DeadJobs(ctx)
for _, j := range dead {
    log.Printf("dead: id=%s type=%s attempts=%d error=%s",
        j.ID, j.JobType, j.RetryCount, j.ErrorMessage)
}

// cancel a job that hasn't started yet
// works on: pending, scheduled, failed
// has no effect once a job is running
err := q.CancelJob(ctx, jobID)
```

---

## RunOnce (cron mode)

`RunOnce` drains the current queue and returns instead of running a persistent loop. Use it when you want an external scheduler (cron, Kubernetes CronJob) to control when work happens rather than running workers continuously.

```go
if err := q.Connect(ctx); err != nil {
    log.Fatal(err)
}
// processes everything currently in Redis, then returns
if err := q.RunOnce(ctx); err != nil {
    log.Fatal(err)
}
```

---

## Metrics

When `MetricsPort` is set, a Prometheus `/metrics` endpoint is available on that port. Each `Queue` instance uses its own private Prometheus registry so multiple instances in the same process do not conflict.

| Metric                   | Type        | Description                             |
|--------------------------|-------------|-----------------------------------------|
| kyu_jobs_total           | counter     | total jobs ever submitted               |
| kyu_jobs_processed_total | counter vec | completed jobs, labelled by status      |
| kyu_job_failures_total   | counter vec | failures, labelled by job_type          |
| kyu_jobs_dead_total      | counter     | jobs that exhausted all retries         |
| kyu_queue_depth          | gauge       | jobs currently waiting in Redis         |

Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: kyu
    static_configs:
      - targets: ["localhost:9090"]
```

---

## Benchmarks

Measured on an Intel Core i5-6300U (4 cores, 2.4GHz).

```
BenchmarkRegister               ~52 ns/op     0 B/op    0 allocs/op
BenchmarkExecute                ~950 ns/op  320 B/op    5 allocs/op
BenchmarkExecuteWithMiddleware  ~1.2 µs/op  480 B/op    7 allocs/op
BenchmarkExecuteParallel        ~600 ns/op  320 B/op    5 allocs/op
```

Job dispatch runs in under one microsecond. Zero allocations on `Register`. Each additional middleware layer costs one closure allocation (~160 bytes). Under parallel load the registry mutex shows no measurable contention. In practice throughput is bounded by Postgres write latency and Redis round-trip time, not by the dispatch path.

---

## Docker Compose

A `docker-compose.yml` is included with Postgres, Redis, Prometheus, and Grafana. A pre-built Grafana dashboard (`dashboard.json`) covers queue depth, job throughput, failure rates by job type, goroutine count, and memory usage.

```bash
docker compose up --build
```

| Service    | Address          |
|------------|------------------|
| Metrics    | localhost:9090   |
| Prometheus | localhost:9090   |
| Grafana    | localhost:3000   |
| Postgres   | localhost:5432   |
| Redis      | localhost:6380   |

---

## Running tests

Unit tests - no infrastructure required:

```bash
go test -short ./...
```

Integration tests - requires Postgres on 5432 and Redis on 6380.

> Before running, update the DSN and Redis address in `kyu_integration_test.go` to match your local setup. The default credentials in the test file are for local development only.

```bash
go test ./...
```

Benchmarks:

```bash
go test -bench=. -benchmem -count=3
```
