package database

import (
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkSQLiteRepositoryFindAll(b *testing.B) {
	benchmarkSQLiteRepository(b, func(repo *Repository) error {
		_, err := repo.FindAll()
		return err
	})
}

func BenchmarkSQLiteRepositoryLookup(b *testing.B) {
	benchmarkSQLiteRepository(b, func(repo *Repository) error {
		id := "public-000000"
		if repo.native {
			id = "native-000000"
		}
		row, err := repo.FindByID(id)
		if err == nil && row == nil {
			return fmt.Errorf("seeded sandbox was not found")
		}
		return err
	})
}

func BenchmarkSQLiteRepositoryCommandHistory(b *testing.B) {
	benchmarkSQLiteRepository(b, func(repo *Repository) error {
		sandboxID := "public-000000"
		if repo.native {
			sandboxID = "native-000000"
		}
		_, err := repo.FindCommandsBySandbox(sandboxID)
		return err
	})
}

func benchmarkSQLiteRepository(b *testing.B, run func(*Repository) error) {
	b.Helper()
	for _, mode := range []string{"memory", "file"} {
		for _, size := range []int{1, 100, 1000} {
			for _, view := range []string{"public", "native"} {
				name := fmt.Sprintf("sqlite_%s/records_%d/%s", mode, size, view)
				b.Run(name, func(b *testing.B) {
					repo := newPerformanceRepository(b, mode, size)
					if view == "native" {
						repo = repo.NativeView()
					}
					validatePerformanceRepository(b, repo, size)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if err := run(repo); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

func newPerformanceRepository(b *testing.B, mode string, size int) *Repository {
	b.Helper()
	path := ":memory:"
	if mode == "file" {
		path = filepath.Join(b.TempDir(), "repository.db")
	}
	db := New(path)
	sqlDB, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	// One connection keeps the in-memory SQLite database stable across GORM calls.
	sqlDB.SetMaxOpenConns(1)
	closeDB := func() {
		if err := sqlDB.Close(); err != nil {
			b.Errorf("close benchmark SQLite database: %v", err)
		}
	}
	b.Cleanup(closeDB)
	repo := NewRepository(db)
	for i := 0; i < size; i++ {
		publicID := fmt.Sprintf("public-%06d", i)
		nativeID := fmt.Sprintf("native-%06d", i)
		if err := repo.CreateOwnership(Sandbox{ID: publicID, NativeID: nativeID, RuntimeKind: "benchmark", Name: fmt.Sprintf("sandbox-%06d", i), Image: "node:25-alpine"}); err != nil {
			b.Fatalf("seed sandbox %d: %v", i, err)
		}
	}
	for i := 0; i < size; i++ {
		if err := repo.SaveCommand(Command{ID: fmt.Sprintf("cmd-%06d", i), SandboxID: "public-000000", Name: "node", Args: "[]", StartedAt: int64(i + 1)}); err != nil {
			b.Fatalf("seed command %d: %v", i, err)
		}
	}
	return repo
}

// Validate seeded sizes and view-specific identity translation before timing.
func validatePerformanceRepository(b *testing.B, repo *Repository, size int) {
	b.Helper()
	rows, err := repo.FindAll()
	if err != nil {
		b.Fatal(err)
	}
	if len(rows) != size {
		b.Fatalf("FindAll fixture rows=%d, want=%d", len(rows), size)
	}
	ids := make(map[string]bool, len(rows))
	for _, item := range rows {
		ids[item.ID] = true
	}
	for i := 0; i < size; i++ {
		want := fmt.Sprintf("public-%06d", i)
		if repo.native {
			want = fmt.Sprintf("native-%06d", i)
		}
		if !ids[want] {
			b.Fatalf("FindAll view omitted translated ID %q", want)
		}
	}
	publicID, lookupID := "public-000000", "public-000000"
	if repo.native {
		lookupID = "native-000000"
	}
	row, err := repo.FindByID(lookupID)
	if err != nil || row == nil {
		b.Fatalf("view lookup %q: row=%+v err=%v", lookupID, row, err)
	}
	if repo.native {
		publicID = "native-000000"
	}
	if row.ID != publicID {
		b.Fatalf("view lookup ID=%q, want=%q", row.ID, publicID)
	}
	commands, err := repo.FindCommandsBySandbox(lookupID)
	if err != nil {
		b.Fatal(err)
	}
	if len(commands) != size {
		b.Fatalf("command-history fixture rows=%d, want=%d", len(commands), size)
	}
	for i, command := range commands {
		wantCommandID := fmt.Sprintf("cmd-%06d", i)
		if command.ID != wantCommandID || command.SandboxID != publicID {
			b.Fatalf("command-history view item %d = %+v, want id=%q sandbox=%q", i, command, wantCommandID, publicID)
		}
	}
}
