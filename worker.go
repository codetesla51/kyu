package kyu

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const pendingQueue = "jobs:pending"

// execute looks up and runs the handler registered for jobType.
func (q *Queue) execute(jobType, payload string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
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
		result, err := q.rdb.client.BZPopMin(ctx, 2*time.Second, pendingQueue).Result()
		if err != nil {
			if err.Error() == "redis:nil" {
				continue // queue empty, loop back
			}
			if ctx.Err() != nil {
				return
			}
			q.cfg.Logger.Printf("kyu: worker %d: redis error: %v", workerID, err)
			continue
		}

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
				delay := time.Duration(math.Pow(2, float64(j.RetryCount))) * time.Second
				schedule_fail_at := time.Now().Add(delay)
				q.db.conn.Model(&j).Updates(map[string]any{
					"status":        "failed",
					"retry_count":   j.RetryCount + 1,
					"scheduled_at":  schedule_fail_at,
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
func (q *Queue) staleReaper(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := q.cfg.StaleJobTimeout * time.Minute
			if timeout == 0 {
				timeout = 10 * time.Minute
			}
			cutoff := time.Now().Add(-timeout)
			var jobs []job
			err := q.db.conn.Where("status = ? AND locked_at <= ?", "running", cutoff).Find(&jobs).Error
			if err != nil {
				q.cfg.Logger.Printf("kyu: stale reaper: fetch stale jobs: %v", err)
				continue
			}
			for _, j := range jobs {
				q.db.conn.Model(&j).Updates(map[string]any{
					"status":    "pending",
					"locked_at": nil,
					"locked_by": "",
				})
				q.cfg.Logger.Printf("kyu: stale reaper: re-queued stale job %s", j.ID)
			}
		}
	}
}

func (q *Queue) runOnce(ctx context.Context, workerID int) {
	for {
		if ctx.Err() != nil {
			return
		}
		result, err := q.rdb.client.ZPopMin(ctx, pendingQueue, 1).Result()

		if err != nil {

			if ctx.Err() != nil {
				return
			}
			q.cfg.Logger.Printf("kyu: worker %d: redis error: %v", workerID, err)
			continue
		}
		if len(result) == 0 {
			return
		}

		jobID, _ := result[0].Member.(string)
		var j job
		if err := q.db.conn.First(&j, "id = ?", jobID).Error; err != nil {
			q.cfg.Logger.Printf("kyu: worker %d: job %s not found: %v", workerID, jobID, err)
			continue
		}
		if j.ScheduledAt != nil && j.ScheduledAt.After(time.Now()) {
			q.rdb.client.ZAdd(ctx, pendingQueue, redis.Z{
				Score:  float64(j.Priority),
				Member: j.ID,
			})
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
				delay := time.Duration(math.Pow(2, float64(j.RetryCount))) * time.Second
				schedule_fail_at := time.Now().Add(delay)
				q.db.conn.Model(&j).Updates(map[string]any{
					"status":        "failed",
					"retry_count":   j.RetryCount + 1,
					"scheduled_at":  schedule_fail_at,
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
