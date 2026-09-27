package docker

import (
	"testing"

	"opensbx/internal/database"
)

func TestNewConstructsClientFromValidatedLoopbackEndpointWithoutConnecting(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	db := database.New(":memory:")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	client := New(database.NewRepository(db))
	t.Cleanup(func() { _ = client.cli.Close() })
	if client == nil || client.cli == nil || client.repo == nil {
		t.Fatal("New() returned an incomplete Docker adapter")
	}
	if client.cli.DaemonHost() != "tcp://127.0.0.1:2375" {
		t.Fatalf("daemon host=%q", client.cli.DaemonHost())
	}
}
