package kyu

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const pendingQueue = "jobs:pending"

// execute looks up and runs the handler registered for jobType.
func (q *Queue) execute(jobType, payload string) error {
	q.mu.RLock()
	handler, ok := q.registry[jobType]
	q.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown job type: %s", jobType)
	}
	return handler(payload)
}

// runWorkerPool spawns numWorkers goroutines and waits for them all to exit
// (which happens when ctx is cancelled).
func (q *Queue) runWorkerPool(ctx context.Context, numWorkers int) {
	var wg sync.WaitGroup
	for i := 1; i <= numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			q.runWorker(ctx, id)
		}(i)
	}
	wg.Wait()
}

// runWorker is the main loop for a single worker goroutine.
func (q *Queue) runWorker(ctx context.Context, workerID int) {
	for {
		if ctx.Err() != nil {
			return
		}
		// ZPopMin is non-blocking. If the queue is empty we sleep briefly
		// and loop, which also gives us a clean shutdown check on every tick.
		results, err := q.rdb.client.ZPopMin(ctx, pendingQueue).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			q.cfg.Logger.Printf("kyu: worker %d: redis error: %v", workerID, err)
			time.Sleep(time.Second)
			continue
		}
		if len(results) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		result := results[0]

		jobID, _ := result.Member.(string)
		var j job
		if err := q.db.conn.First(&j, "id = ?", jobID).Error; err != nil {
			q.cfg.Logger.Printf("kyu: worker %d: job %s not found: %v", workerID, jobID, err)
			continue
		}

		now := time.Now()
		q.db.conn.Model(&j).Updates(job{
			Status:   "running",
			LockedAt: &now,
			LockedBy: fmt.Sprintf("worker-%d", workerID),
		})

		execErr := q.execute(j.JobType, j.Payload)

		if execErr != nil {
			if j.RetryCount < j.MaxRetries {
				q.db.conn.Model(&j).Updates(map[string]any{
					"status":        "failed",
					"retry_count":   j.RetryCount + 1,
					"error_message": execErr.Error(),
				})
				if err := q.rdb.client.ZAdd(ctx, pendingQueue, redis.Z{
					Score:  float64(j.Priority),
					Member: j.ID,
				}).Err(); err != nil {
					q.cfg.Logger.Printf("kyu: worker %d: re-queue job %s: %v", workerID, j.ID, err)
				}
				q.met.jobFailures.WithLabelValues(j.JobType).Inc()
			} else {
				q.db.conn.Model(&j).Updates(map[string]any{
					"status":        "dead",
					"error_message": fmt.Sprintf("job failed after %d retries: %v", j.RetryCount, execErr),
				})
				q.met.jobsDeadTotal.Inc()
			}
		} else {
			completed := time.Now()
			q.db.conn.Model(&j).Updates(map[string]any{
				"status":       "completed",
				"completed_at": completed,
			})
			q.met.jobsProcessed.WithLabelValues("completed").Inc()
		}
	}
}

// runScheduler ticks every 5 seconds and enqueues any pending jobs whose
// scheduled_at time has arrived.
func (q *Queue) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var jobs []job
			err := q.db.conn.
				Where("status = ? AND scheduled_at <= ?", "pending", time.Now()).
				Find(&jobs).Error
			if err != nil {
				q.cfg.Logger.Printf("kyu: scheduler: fetch scheduled jobs: %v", err)
				continue
			}
			for _, j := range jobs {
				if err := q.rdb.client.ZAdd(ctx, pendingQueue, redis.Z{
					Score:  float64(j.Priority),
					Member: j.ID,
				}).Err(); err != nil {
					q.cfg.Logger.Printf("kyu: scheduler: queue job %s: %v", j.ID, err)
				}
			}
		}
	}
}
