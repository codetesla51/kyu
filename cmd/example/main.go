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
		Workers:         5,
		MetricsPort:     9090,
		StaleJobTimeout: 1,
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
	q.Use(func(ctx context.Context, jobtype, payload string, next func() error) error {
		log.Printf("logging middleware")
		err := next()
		if err != nil {
			log.Printf("Job failed : %s", jobtype)
			return err
		}
		return err
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		time.Sleep(1 * time.Second) // wait for workers to start

		// Successful jobs
		for i := 1; i <= 5; i++ {
			q.Enqueue(ctx, "send_email", fmt.Sprintf(`{"user_id": %d, "email": "user%d@test.com"}`, i, i), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   1,
			})
		}

		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "process_payment", fmt.Sprintf(`{"order_id": %d, "amount": %d}`, i, i*100), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   3,
			})
		}

		for i := 1; i <= 4; i++ {
			q.Enqueue(ctx, "send_sms", fmt.Sprintf(`{"phone": "555-%d", "msg": "hello"}`, i), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   2,
			})
		}

		// Failing jobs
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "failing_job", fmt.Sprintf(`{"task": "fail-%d"}`, i), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   0,
			})
		}

		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "cleanup_old_data", fmt.Sprintf(`{"table": "logs-%d"}`, i), kyu.EnqueueOptions{
				MaxRetries: 1,
				Priority:   0,
			})
		}

		// Mixed
		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "generate_report", fmt.Sprintf(`{"report_id": %d}`, i), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   1,
			})
		}

		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "sync_user_data", fmt.Sprintf(`{"user_id": %d}`, i), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   2,
			})
		}

		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "notify_admin", fmt.Sprintf(`{"alert": "issue-%d"}`, i), kyu.EnqueueOptions{
				MaxRetries: 3,
				Priority:   2,
			})
		}

		// Scheduled job
		scheduledAt := time.Now().Add(30 * time.Second)
		q.Enqueue(ctx, "send_email", `{"user_id": 99, "email": "scheduled@gmail.com"}`, kyu.EnqueueOptions{
			MaxRetries:  3,
			Priority:    0,
			ScheduledAt: &scheduledAt,
			TimeOut:     1 * time.Second,
		})

		// Cancel job test - use generate_report which takes 3 seconds
		cancelJobID, _ := q.Enqueue(ctx, "generate_report", `{"report_id": 999}`, kyu.EnqueueOptions{
			MaxRetries: 2,
			Priority:   0,
		})
		log.Printf("Enqueued job to cancel: %s", cancelJobID)

		time.Sleep(500 * time.Millisecond) // wait a bit before cancelling

		err := q.CancelJob(ctx, cancelJobID)
		if err != nil {
			log.Printf("CancelJob error: %v", err)
		} else {
			log.Printf("Successfully cancelled job: %s", cancelJobID)
		}

		time.Sleep(15 * time.Second)

		// Enqueue more after a while
		for i := 1; i <= 3; i++ {
			q.Enqueue(ctx, "send_email", fmt.Sprintf(`{"user_id": %d, "email": "later%d@test.com"}`, i+100, i), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   1,
			})
		}

		for i := 1; i <= 2; i++ {
			q.Enqueue(ctx, "failing_job", fmt.Sprintf(`{"task": "later-fail-%d"}`, i), kyu.EnqueueOptions{
				MaxRetries: 2,
				Priority:   0,
			})
		}

		deadJobs, err := q.DeadJobs(ctx)
		if err != nil {
			log.Printf("failed to get dead jobs: %v", err)
		}
		for _, j := range deadJobs {
			log.Printf("dead job: id=%s type=%s retries=%d error=%s",
				j.ID, j.JobType, j.RetryCount, j.ErrorMessage)
		}
	}()
	log.Println("kyu example: starting")
	if err := q.Connect(ctx); err != nil {
		log.Printf("kyu: %v", err)
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
