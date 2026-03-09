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
		DSN:             envOr("DATABASE_URL", "postgres://user:pass@localhost:5432/kyu?sslmode=disable"),
		RedisAddr:       envOr("REDIS_ADDR", "localhost:6379"),
		Workers:         5,
		MetricsPort:     9090,
		StaleJobTimeout: 1,
	})

	q.Register("send_email", func(payload string) error {
		log.Printf("send_email: processing payload=%q", payload)
		time.Sleep(2 * time.Second)
		return nil
	})

	q.Register("failing_job", func(payload string) error {
		return fmt.Errorf("something went wrong processing: %s", payload)
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		time.Sleep(1 * time.Second) // wait for workers to start
		q.Enqueue(ctx, "send_email", `{"user_id": 42, "email": "usman@gmail.com"}`, kyu.EnqueueOptions{
			MaxRetries: 3,
			Priority:   0,
		})

		// Enqueue a failing job
		q.Enqueue(ctx, "failing_job", `{"task": "will fail"}`, kyu.EnqueueOptions{
			MaxRetries: 3,
			Priority:   1,
		})
		scheduledAt := time.Now().Add(30 * time.Second)
		q.Enqueue(ctx, "send_email", `{"user_id": 99, "email": "scheduled@gmail.com"}`, kyu.EnqueueOptions{
			MaxRetries:  3,
			Priority:    0,
			ScheduledAt: &scheduledAt,
		})
		jobs, err := q.Inspect(ctx, "e5961dc9-9959-4d2f-97ac-73e06ab0e4f5")
		if err != nil {
			return
		}
		fmt.Println(jobs)
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
