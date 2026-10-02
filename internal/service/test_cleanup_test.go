package service

import (
	"testing"

	"gorm.io/gorm"
)

func cleanupServiceTestDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close service test database: %v", err)
		}
	})
}
