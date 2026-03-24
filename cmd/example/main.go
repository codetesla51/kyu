package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codetesla51/kyu"
)

func main() {
	q := kyu.New(kyu.Config{
		DSN:             envOr("DATABASE_URL", "postgres://localhost:5432/kyu?sslmode=disable"),
		RedisAddr:       envOr("REDIS_ADDR", "localhost:6379"),
		RedisPassword:   envOr("REDIS_PASSWORD", ""),
		Workers:         5,
		MetricsPort:     9090,
		StaleJobTimeout: 2 * time.Second,
		Logger:          log.Default(),
	})

	q.Register("send_email", func(ctx context.Context, payload string) error {
		log.Printf("send_email: processing payload=%q", payload)
		time.Sleep(2 * time.Second)
		return nil
	})

	q.Register("failing_job", func(ctx context.Context, payload string) error {
		return fmt.Errorf("something went wrong processing: %s", payload)
	})

	q.Register("process_payment", func(ctx context.Context, payload string) error {
		log.Printf("process_payment: processing payload=%q", payload)
		time.Sleep(1 * time.Second)
		return nil
	})
	q.Register("slow_job", func(ctx context.Context, payload string) error {
		log.Printf("process_slow_job: processing payload=%q", payload)
		time.Sleep(10 * time.Second)
		return nil
	})

	q.Register("send_sms", func(ctx context.Context, payload string) error {
		log.Printf("send_sms: processing payload=%q", payload)
		time.Sleep(500 * time.Millisecond)
		return nil
	})

	q.Register("generate_report", func(ctx context.Context, payload string) error {
		log.Printf("generate_report: processing payload=%q", payload)
		time.Sleep(3 * time.Second)
		return nil
	})

	q.Register("cleanup_old_data", func(ctx context.Context, payload string) error {
		return fmt.Errorf("cleanup failed: %s", payload)
	})

	q.Register("sync_user_data", func(ctx context.Context, payload string) error {
		log.Printf("sync_user_data: processing payload=%q", payload)
		time.Sleep(800 * time.Millisecond)
		return nil
	})

	q.Register("notify_admin", func(ctx context.Context, payload string) error {
		log.Printf("notify_admin: processing payload=%q", payload)
		return nil
	})

	// new failing job types
	q.Register("db_backup", func(ctx context.Context, payload string) error {
		return fmt.Errorf("db_backup: connection refused: %s", payload)
	})

	q.Register("send_webhook", func(ctx context.Context, payload string) error {
		return fmt.Errorf("send_webhook: upstream timeout after 30s: %s", payload)
	})

	q.Register("resize_image", func(ctx context.Context, payload string) error {
		return fmt.Errorf("resize_image: unsupported format: %s", payload)
	})

	q.Register("export_csv", func(ctx context.Context, payload string) error {
		return fmt.Errorf("export_csv: out of memory: %s", payload)
	})

	q.Register("send_push_notification", func(ctx context.Context, payload string) error {
		return fmt.Errorf("send_push_notification: invalid device token: %s", payload)
	})

	// flaky — fails first two attempts then succeeds
	attempts := map[string]int{}
	q.Register("flaky_job", func(ctx context.Context, payload string) error {
		attempts[payload]++
		if attempts[payload] < 3 {
			return fmt.Errorf("flaky_job: attempt %d failed: %s", attempts[payload], payload)
		}
		log.Printf("flaky_job: succeeded on attempt %d: %s", attempts[payload], payload)
		return nil
	})

	q.Use(func(ctx context.Context, jobtype, payload string, next func() error) error {
		log.Printf("logging middleware: job=%s", jobtype)
		err := next()
		if err != nil {
			log.Printf("job failed: type=%s error=%v", jobtype, err)
			return err
		}
		return nil
	})

	q.Use(func(ctx context.Context, jobtype, payload string, next func() error) error {
		start := time.Now()
		err := next()
		duration := time.Since(start)
		log.Printf("metrics middleware: job=%s duration=%s error=%v", jobtype, duration, err)
		return err
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		time.Sleep(1 * time.Second)

		// --- successful jobs ---
		for i := 1; i <= 5; i++ {
			q.Enqueue(ctx, "send_email", fmt.Sprintf(`{"user_id": %d, "email": "user%d@test.com"}`, i, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 1})
		}
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "process_payment", fmt.Sprintf(`{"order_id": %d, "amount": %d}`, i, i*100), kyu.EnqueueOptions{MaxRetries: 2, Priority: 3})
		}
		for i := 1; i <= 4; i++ {
			q.Enqueue(ctx, "send_sms", fmt.Sprintf(`{"phone": "555-%d", "msg": "hello"}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 2})
		}
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "sync_user_data", fmt.Sprintf(`{"user_id": %d}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 2})
		}
		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "notify_admin", fmt.Sprintf(`{"alert": "issue-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 3, Priority: 2})
		}
		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "generate_report", fmt.Sprintf(`{"report_id": %d}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 1})
		}

		// --- always-failing jobs (will exhaust retries and go dead) ---
		for i := 1; i <= 5; i++ {
			q.Enqueue(ctx, "failing_job", fmt.Sprintf(`{"task": "fail-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 0})
		}
		for i := 1; i <= 4; i++ {
			q.Enqueue(ctx, "cleanup_old_data", fmt.Sprintf(`{"table": "logs-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 1, Priority: 0})
		}
		for i := 1; i <= 4; i++ {
			q.Enqueue(ctx, "db_backup", fmt.Sprintf(`{"db": "shard-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 3, Priority: 0})
		}
		for i := 1; i <= 5; i++ {
			q.Enqueue(ctx, "send_webhook", fmt.Sprintf(`{"url": "https://hook-%d.example.com"}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 1})
		}
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "resize_image", fmt.Sprintf(`{"file": "image-%d.bmp"}`, i), kyu.EnqueueOptions{MaxRetries: 1, Priority: 1})
		}
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "export_csv", fmt.Sprintf(`{"report": "monthly-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 0})
		}
		for i := 1; i <= 4; i++ {
			q.Enqueue(ctx, "send_push_notification", fmt.Sprintf(`{"device": "token-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 1})
		}

		// --- flaky jobs (fail then succeed) ---
		for i := 1; i <= 4; i++ {
			q.Enqueue(ctx, "flaky_job", fmt.Sprintf(`task-%d`, i), kyu.EnqueueOptions{MaxRetries: 5, Priority: 1})
		}

		// --- scheduled job ---
		scheduledAt := time.Now().Add(30 * time.Second)
		q.Enqueue(ctx, "send_email", `{"user_id": 99, "email": "scheduled@gmail.com"}`, kyu.EnqueueOptions{
			MaxRetries:  3,
			Priority:    0,
			ScheduledAt: &scheduledAt,
			TimeOut:     1 * time.Second,
		})

		time.Sleep(20 * time.Second)

		// --- second wave ---
		log.Println("enqueuing second wave...")
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "send_email", fmt.Sprintf(`{"user_id": %d, "email": "later%d@test.com"}`, i+100, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 1})
		}
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "failing_job", fmt.Sprintf(`{"task": "later-fail-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 0})
		}
		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "send_webhook", fmt.Sprintf(`{"url": "https://wave2-hook-%d.example.com"}`, i), kyu.EnqueueOptions{MaxRetries: 1, Priority: 1})
		}
		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "db_backup", fmt.Sprintf(`{"db": "wave2-shard-%d"}`, i), kyu.EnqueueOptions{MaxRetries: 2, Priority: 0})
		}

		deadJobs, err := q.DeadJobs(ctx)
		if err != nil {
			log.Printf("failed to get dead jobs: %v", err)
		}
		log.Printf("dead jobs count: %d", len(deadJobs))
		for _, j := range deadJobs {
			log.Printf("dead job: id=%s type=%s retries=%d error=%s", j.ID, j.JobType, j.RetryCount, j.ErrorMessage)
		}
	}()

	log.Println("kyu example: starting")
	if err := q.Connect(ctx); err != nil {
		log.Fatalf("kyu: connect: %v", err)
	}
	if err := q.Start(ctx); err != nil {
		log.Fatalf("kyu: %v", err)
	}
	log.Println("kyu example: shutdown complete")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
