package kyu

import (
	"time"

	"gorm.io/gorm"
)

// job is the internal GORM model — unexported so callers never depend on it.
type job struct {
	ID           string         `json:"id"           gorm:"primaryKey"`
	JobType      string         `json:"job_type"     gorm:"index"`
	Priority     int            `json:"priority"     gorm:"index;default:0"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `json:"deleted_at"   gorm:"index"`
	Payload      string         `json:"payload"`
	Status       string         `json:"status"       gorm:"index:idx_status_scheduled;default:pending"`
	CompletedAt  *time.Time     `json:"completed_at"`
	ScheduledAt  *time.Time     `json:"scheduled_at" gorm:"index:idx_status_scheduled"`
	MaxRetries   int            `json:"max_retries"`
	RetryCount   int            `json:"retry_count"`
	ErrorMessage string         `json:"error_message"`
	LockedAt     *time.Time     `json:"locked_at"    gorm:"index"`
	LockedBy     string         `json:"locked_by"`
}
