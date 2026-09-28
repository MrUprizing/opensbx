package database

import (
	"log"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// New opens a SQLite database at the given path and runs AutoMigrate.
// Panics on failure (unrecoverable at startup).
func New(path string) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("database: failed to open %s: %v", path, err)
	}

	if err := db.AutoMigrate(&Sandbox{}, &Command{}, &Operation{}); err != nil {
		log.Fatalf("database: migration failed: %v", err)
	}

	return db
}
