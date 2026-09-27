package database

import "testing"

func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	return NewRepository(New(":memory:"))
}

func TestRepositorySandboxCRUD(t *testing.T) {
	repo := newTestRepo(t)

	sb := Sandbox{
		ID:    "sb-1",
		Name:  "demo",
		Image: "node:22",
		Ports: JSONMap{"3000/tcp": "32768"},
		Port:  "3000/tcp",
	}

	if err := repo.Save(sb); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	byID, err := repo.FindByID("sb-1")
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if byID == nil || byID.Name != "demo" {
		t.Fatalf("FindByID() mismatch: %+v", byID)
	}

	byName, err := repo.FindByName("demo")
	if err != nil {
		t.Fatalf("FindByName() error: %v", err)
	}
	if byName == nil || byName.ID != "sb-1" {
		t.Fatalf("FindByName() mismatch: %+v", byName)
	}

	if err := repo.UpdatePorts("sb-1", JSONMap{"3000/tcp": "32769"}); err != nil {
		t.Fatalf("UpdatePorts() error: %v", err)
	}

	updated, err := repo.FindByID("sb-1")
	if err != nil {
		t.Fatalf("FindByID() after update error: %v", err)
	}
	if updated.Ports["3000/tcp"] != "32769" {
		t.Fatalf("ports not updated: %+v", updated.Ports)
	}

	all, err := repo.FindAll()
	if err != nil {
		t.Fatalf("FindAll() error: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("FindAll() len = %d, want 1", len(all))
	}

	if err := repo.Delete("sb-1"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	notFound, err := repo.FindByID("sb-1")
	if err != nil {
		t.Fatalf("FindByID() after delete error: %v", err)
	}
	if notFound != nil {
		t.Fatalf("expected nil after delete, got %+v", notFound)
	}
}

func TestRepositoryCommandsCRUD(t *testing.T) {
	repo := newTestRepo(t)

	if err := repo.Save(Sandbox{ID: "sb-1", Name: "demo", Image: "node:22"}); err != nil {
		t.Fatalf("Save sandbox error: %v", err)
	}

	if err := repo.SaveCommand(Command{ID: "cmd-2", SandboxID: "sb-1", Name: "echo", Args: "[]", StartedAt: 2}); err != nil {
		t.Fatalf("SaveCommand cmd-2 error: %v", err)
	}
	if err := repo.SaveCommand(Command{ID: "cmd-1", SandboxID: "sb-1", Name: "ls", Args: "[]", StartedAt: 1}); err != nil {
		t.Fatalf("SaveCommand cmd-1 error: %v", err)
	}

	got, err := repo.FindCommandByID("cmd-1")
	if err != nil {
		t.Fatalf("FindCommandByID() error: %v", err)
	}
	if got == nil || got.Name != "ls" {
		t.Fatalf("FindCommandByID() mismatch: %+v", got)
	}

	ordered, err := repo.FindCommandsBySandbox("sb-1")
	if err != nil {
		t.Fatalf("FindCommandsBySandbox() error: %v", err)
	}
	if len(ordered) != 2 {
		t.Fatalf("FindCommandsBySandbox() len = %d, want 2", len(ordered))
	}
	if ordered[0].ID != "cmd-1" || ordered[1].ID != "cmd-2" {
		t.Fatalf("commands are not ordered by started_at ASC: %+v", ordered)
	}

	if err := repo.UpdateCommandFinished("cmd-1", 0, 99); err != nil {
		t.Fatalf("UpdateCommandFinished() error: %v", err)
	}

	finished, err := repo.FindCommandByID("cmd-1")
	if err != nil {
		t.Fatalf("FindCommandByID() after update error: %v", err)
	}
	if finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatalf("exit code not updated: %+v", finished)
	}
	if finished.FinishedAt == nil || *finished.FinishedAt != 99 {
		t.Fatalf("finished_at not updated: %+v", finished)
	}

	if err := repo.DeleteCommandsBySandbox("sb-1"); err != nil {
		t.Fatalf("DeleteCommandsBySandbox() error: %v", err)
	}

	empty, err := repo.FindCommandsBySandbox("sb-1")
	if err != nil {
		t.Fatalf("FindCommandsBySandbox() after delete error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected 0 commands after delete, got %d", len(empty))
	}
}

func TestRepositoryReturnsNilForMissingRecords(t *testing.T) {
	repo := newTestRepo(t)
	if got, err := repo.FindByName("missing"); err != nil || got != nil {
		t.Fatalf("FindByName() missing = %v, %v; want nil, nil", got, err)
	}
	if got, err := repo.FindCommandByID("missing"); err != nil || got != nil {
		t.Fatalf("FindCommandByID() missing = %v, %v; want nil, nil", got, err)
	}
	all, err := repo.FindAll()
	if err != nil || len(all) != 0 {
		t.Fatalf("FindAll() empty = %v, %v", all, err)
	}
}

func TestRepositoryPropagatesClosedDatabaseErrors(t *testing.T) {
	db := New(":memory:")
	repo := NewRepository(db)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.FindByID("sb"); err == nil {
		t.Fatal("FindByID() should return a database error when the pool is closed")
	}
	if _, err := repo.FindByName("demo"); err == nil {
		t.Fatal("FindByName() should return a database error when the pool is closed")
	}
	if _, err := repo.FindAll(); err == nil {
		t.Fatal("FindAll() should return a database error when the pool is closed")
	}
	if _, err := repo.FindCommandByID("cmd"); err == nil {
		t.Fatal("FindCommandByID() should return a database error when the pool is closed")
	}
	if _, err := repo.FindCommandsBySandbox("sb"); err == nil {
		t.Fatal("FindCommandsBySandbox() should return a database error when the pool is closed")
	}
	if err := repo.Save(Sandbox{ID: "sb"}); err == nil { t.Fatal("Save() should return a database error when the pool is closed") }
	if err := repo.CreateOwnership(Sandbox{ID: "sbx-public", NativeID: "native"}); err == nil { t.Fatal("CreateOwnership() should return a database error when the pool is closed") }
	if err := repo.UpdatePorts("sb", JSONMap{"3000/tcp": "39001"}); err == nil { t.Fatal("UpdatePorts() should return a database error when the pool is closed") }
	if err := repo.Delete("sb"); err == nil { t.Fatal("Delete() should return a database error when the pool is closed") }
	if err := repo.SaveCommand(Command{ID: "cmd", SandboxID: "sb"}); err == nil { t.Fatal("SaveCommand() should return a database error when the pool is closed") }
	if err := repo.UpdateCommandFinished("cmd", 0, 1); err == nil { t.Fatal("UpdateCommandFinished() should return a database error when the pool is closed") }
	if err := repo.DeleteCommandsBySandbox("sb"); err == nil { t.Fatal("DeleteCommandsBySandbox() should return a database error when the pool is closed") }
}
