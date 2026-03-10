package kyu

import (
	"fmt"

	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// db wraps a *gorm.DB so the field name doesn't collide with the gorm package.
type db struct {
	conn *gorm.DB
}

// connectDB opens a GORM/Postgres connection using the provided DSN.
func (q *Queue) connectDB(dsn string) (*db, error) {
	conn, err := gorm.Open(gormpostgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	sqlDB, err := conn.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db: %w", err)
	}
	if q.cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(q.cfg.MaxOpenConns)
	}
	if q.cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(q.cfg.MaxIdleConns)
	}
	if q.cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(q.cfg.ConnMaxLifetime)
	}

	return &db{conn: conn}, nil
}

// migrate runs GORM AutoMigrate for the supplied model pointers.
func (d *db) migrate(models ...interface{}) error {
	if err := d.conn.AutoMigrate(models...); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	return nil
}
