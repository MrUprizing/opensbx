package database

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestLegacySandboxRowsMigrateWithoutAdoptionOrMetadataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE sandboxes (
		id text PRIMARY KEY, native_id text, image_root text, image_manifest text,
		native_image text, cache_version text, recovery_error text, name text,
		image text, ports text, port text
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sandboxes (id, native_id, name, image, ports, port)
		VALUES (?, '', ?, ?, ?, ?)`, "legacy-id", "legacy-name", "node:20", `{"3000/tcp":"32768"}`, "3000/tcp").Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	migrated := New(path)
	migratedSQL, err := migrated.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migratedSQL.Close() })
	row, err := NewRepository(migrated).FindByID("legacy-id")
	if err != nil || row == nil {
		t.Fatalf("legacy row after migration=%+v err=%v", row, err)
	}
	if row.ID != "legacy-id" || row.NativeID != "" || row.Name != "legacy-name" || row.Image != "node:20" || row.Ports["3000/tcp"] != "32768" {
		t.Fatalf("migration changed legacy metadata: %+v", row)
	}
	if row.RuntimeKind != "" || row.AttemptToken != "" || row.ExpiresAt != nil {
		t.Fatalf("migration implicitly adopted legacy ownership: %+v", row)
	}
	ops, err := NewRepository(migrated).Operations("docker")
	if err != nil || len(ops) != 0 {
		t.Fatalf("legacy migration created lifecycle intents: %+v err=%v", ops, err)
	}
}

func TestSandboxDeadlineAndOperationSurviveSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.db")
	deadline := time.Date(2032, 4, 5, 6, 7, 8, 9, time.UTC)
	db := New(path)
	firstSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	if err := repo.CreateOwnership(Sandbox{
		ID: "public-id", NativeID: "native-id", RuntimeKind: "docker", AttemptToken: "owner-token",
		ExpiresAt: &deadline, Name: "durable", Image: "node:25",
	}); err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: "docker:attempt", RuntimeKind: "docker", PublicID: "public-id", NativeID: "native-id", Token: "owner-token", Kind: "restart", Deadline: &deadline}
	if err := db.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstSQL.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := New(path)
	reopenedSQL, err := reopened.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedSQL.Close() })
	reopenedRepo := NewRepository(reopened)
	row, err := reopenedRepo.FindByID("public-id")
	if err != nil || row == nil || row.ExpiresAt == nil || !row.ExpiresAt.Equal(deadline) {
		t.Fatalf("reopened sandbox deadline=%+v row=%+v err=%v", rowDeadline(row), row, err)
	}
	ops, err := reopenedRepo.Operations("docker")
	if err != nil || len(ops) != 1 {
		t.Fatalf("reopened operations=%+v err=%v", ops, err)
	}
	if ops[0].ID != op.ID || ops[0].Kind != op.Kind || ops[0].RuntimeKind != "docker" || ops[0].NativeID != "native-id" || ops[0].Deadline == nil || !ops[0].Deadline.Equal(deadline) {
		t.Fatalf("reopened operation changed: %+v", ops[0])
	}
}

func TestDeleteOperationRollsBackHistoryOwnershipAndIntentTogether(t *testing.T) {
	db := New(filepath.Join(t.TempDir(), "delete.db"))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := NewRepository(db)
	if err := repo.CreateOwnership(Sandbox{ID: "public-id", NativeID: "native-id", RuntimeKind: "docker", AttemptToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCommand(Command{ID: "command-id", SandboxID: "public-id", Name: "echo"}); err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: "docker:delete", RuntimeKind: "docker", PublicID: "public-id", NativeID: "native-id", Token: "token", Kind: "delete"}
	if err := db.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	failure := errors.New("sandbox row delete failed")
	if err := db.Callback().Delete().Before("gorm:delete").Register("test:delete-owner-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "sandboxes" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteOperation(op); !errors.Is(err, failure) {
		t.Fatalf("DeleteOperation() error=%v; want injected failure", err)
	}
	assertDeleteState := func(wantPresent bool) {
		t.Helper()
		row, err := repo.FindByID("public-id")
		if err != nil || (row != nil) != wantPresent {
			t.Errorf("owner present=%t want=%t row=%+v err=%v", row != nil, wantPresent, row, err)
		}
		commands, err := repo.FindCommandsBySandbox("public-id")
		if err != nil || (len(commands) == 1) != wantPresent {
			t.Errorf("history count=%d wantPresent=%t err=%v", len(commands), wantPresent, err)
		}
		ops, err := repo.Operations("docker")
		if err != nil || (len(ops) == 1) != wantPresent {
			t.Errorf("intent count=%d wantPresent=%t err=%v", len(ops), wantPresent, err)
		}
	}
	assertDeleteState(true)
	if err := db.Callback().Delete().Remove("test:delete-owner-failure"); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteOperation(op); err != nil {
		t.Fatalf("retry DeleteOperation() = %v", err)
	}
	assertDeleteState(false)
}

func rowDeadline(row *Sandbox) *time.Time {
	if row == nil {
		return nil
	}
	return row.ExpiresAt
}
