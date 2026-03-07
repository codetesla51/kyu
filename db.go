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
func connectDB(dsn string) (*db, error) {
	conn, err := gorm.Open(gormpostgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
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
