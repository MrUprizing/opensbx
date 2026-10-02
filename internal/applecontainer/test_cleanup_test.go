package applecontainer

import (
	"testing"

	"gorm.io/gorm"
)

func cleanupAppleTestClient(t *testing.T, client *Client, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Cancel delayed work, then drain the lifecycle lock before closing SQLite.
		client.recovery.Stop()
		client.mu.Lock()
		client.closing = true
		for id := range client.timers {
			client.clearTimer(id)
		}
		client.mu.Unlock()
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close Apple test database: %v", err)
		}
	})
}
