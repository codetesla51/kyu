package kyu

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// rdb wraps a *redis.Client.
type rdb struct {
	client *redis.Client
}

// connectRedis dials Redis at addr, pings it, and returns the wrapper.
// The ping uses a short background context so it is not affected by a
// caller-supplied ctx that may already be near cancellation.
func connectRedis(ctx context.Context, addr string) (*rdb, error) {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Ping(pingCtx).Result(); err != nil {
		return nil, fmt.Errorf("ping %s: %w", addr, err)
	}
	return &rdb{client: client}, nil
}
