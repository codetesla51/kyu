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
		DSN:         envOr("DATABASE_URL", "postgres://postgres:password@localhost:5432/kyu?sslmode=disable"),
		RedisAddr:   envOr("REDIS_ADDR", "localhost:6379"),
		Workers:     5,
		MetricsPort: 9090,
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

	log.Println("kyu example: starting")
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
