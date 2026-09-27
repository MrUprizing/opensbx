package database

import (
	"testing"
)

func TestNativeViewTranslatesNativeIDsWithoutChangingPublicKeysOrCommandBackreferences(t *testing.T) {
	db := New(t.TempDir() + "/identity.db")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	public := NewRepository(db)
	native := public.NativeView()
	if err := public.Save(Sandbox{ID: "sbx-0123456789abcdef0123456789abcdef", NativeID: "container-native-1", Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := public.Save(Sandbox{ID: "legacy-container-2", Name: "legacy"}); err != nil {
		t.Fatal(err)
	}
	if row, err := public.FindByID("container-native-1"); err != nil || row != nil {
		t.Fatalf("public view accepted native alias: row=%+v err=%v", row, err)
	}
	row, err := native.FindByID("container-native-1")
	if err != nil || row == nil || row.ID != "container-native-1" || row.NativeID != "container-native-1" {
		t.Fatalf("native view lookup=%+v err=%v", row, err)
	}
	if err := native.SaveCommand(Command{ID: "cmd-new", SandboxID: "container-native-1", Name: "echo", StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := public.SaveCommand(Command{ID: "cmd-old", SandboxID: "legacy-container-2", Name: "echo", StartedAt: 2}); err != nil {
		t.Fatal(err)
	}
	rows, err := public.FindAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("persisted sandbox rows=%+v err=%v", rows, err)
	}
	var storedPublic bool
	for _, item := range rows {
		if item.ID == "sbx-0123456789abcdef0123456789abcdef" {
			storedPublic = item.NativeID == "container-native-1"
		}
	}
	if !storedPublic {
		t.Fatalf("database primary key was rewritten: %+v", rows)
	}
	nativeRows, err := native.FindAll()
	if err != nil || len(nativeRows) != 2 {
		t.Fatalf("native sandbox view=%+v err=%v", nativeRows, err)
	}
	var nativeIDTranslated bool
	for _, item := range nativeRows {
		if item.ID == "container-native-1" {
			nativeIDTranslated = true
		}
	}
	if !nativeIDTranslated {
		t.Fatalf("native list retained public IDs: %+v", nativeRows)
	}
	if named, err := native.FindByName("new"); err != nil || named == nil || named.ID != "container-native-1" {
		t.Fatalf("native named lookup=%+v err=%v", named, err)
	}
	commands, err := public.FindCommandsBySandbox("sbx-0123456789abcdef0123456789abcdef")
	if err != nil || len(commands) != 1 || commands[0].SandboxID != "sbx-0123456789abcdef0123456789abcdef" {
		t.Fatalf("public command history=%+v err=%v", commands, err)
	}
	nativeCommands, err := native.FindCommandsBySandbox("container-native-1")
	if err != nil || len(nativeCommands) != 1 || nativeCommands[0].SandboxID != "container-native-1" {
		t.Fatalf("native command view=%+v err=%v", nativeCommands, err)
	}
	legacy, err := native.FindByID("legacy-container-2")
	if err != nil || legacy == nil || legacy.ID != "legacy-container-2" || legacy.RuntimeID() != "legacy-container-2" {
		t.Fatalf("legacy fallback=%+v err=%v", legacy, err)
	}
	if err := native.Delete("container-native-1"); err != nil {
		t.Fatal(err)
	}
	if row, err := public.FindByID("sbx-0123456789abcdef0123456789abcdef"); err != nil || row != nil {
		t.Fatalf("deleted public row=%+v err=%v", row, err)
	}
	if row, err := public.FindByID("legacy-container-2"); err != nil || row == nil {
		t.Fatalf("native deletion removed unrelated legacy row: row=%+v err=%v", row, err)
	}
	legacyCommands, err := public.FindCommandsBySandbox("legacy-container-2")
	if err != nil || len(legacyCommands) != 1 {
		t.Fatalf("unrelated legacy command history=%+v err=%v", legacyCommands, err)
	}
	if err := public.SaveCommand(Command{ID: "cmd-orphan", SandboxID: "deleted-sandbox", Name: "echo", StartedAt: 3}); err != nil {
		t.Fatal(err)
	}
	orphan, err := native.FindCommandByID("cmd-orphan")
	if err != nil || orphan == nil || orphan.SandboxID != "deleted-sandbox" {
		t.Fatalf("orphan command reference changed without ownership row: command=%+v err=%v", orphan, err)
	}
	orphanCommands, err := native.FindCommandsBySandbox("deleted-sandbox")
	if err != nil || len(orphanCommands) != 1 || orphanCommands[0].SandboxID != "deleted-sandbox" {
		t.Fatalf("orphan native command list=%+v err=%v", orphanCommands, err)
	}
}

func TestNativeViewSaveUpdatesExistingPublicRecordAndPreservesLegacyFallback(t *testing.T) {
	db := New(t.TempDir() + "/native-save.db")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	public := NewRepository(db)
	native := public.NativeView()
	if err := public.Save(Sandbox{ID: "sbx-public-1", NativeID: "native-1", Name: "before"}); err != nil {
		t.Fatal(err)
	}
	if err := native.Save(Sandbox{ID: "native-1", Name: "after", Image: "image"}); err != nil {
		t.Fatal(err)
	}
	updated, err := public.FindByID("sbx-public-1")
	if err != nil || updated == nil || updated.ID != "sbx-public-1" || updated.NativeID != "native-1" || updated.Name != "after" {
		t.Fatalf("native update rewrote key or lost native ref: row=%+v err=%v", updated, err)
	}
	if row, err := public.PublicView().FindByID("native-1"); err != nil || row != nil {
		t.Fatalf("public view unexpectedly aliased native ID: row=%+v err=%v", row, err)
	}
	if err := native.Save(Sandbox{ID: "legacy-native", Name: "legacy"}); err != nil {
		t.Fatal(err)
	}
	legacy, err := public.FindByID("legacy-native")
	if err != nil || legacy == nil || legacy.NativeID != "" || legacy.RuntimeID() != "legacy-native" {
		t.Fatalf("native view failed legacy fallback: row=%+v err=%v", legacy, err)
	}
}

func TestCreateOwnershipIsInsertOnlyAndDoesNotOverwritePublicHistory(t *testing.T) {
	db := New(t.TempDir() + "/insert-only.db")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := NewRepository(db)
	original := Sandbox{ID: "sbx-ownership", NativeID: "native-first", Name: "original", Image: "sha256:legacy", ImageRoot: "sha256:root-old"}
	if err := repo.CreateOwnership(original); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateOwnership(Sandbox{ID: original.ID, NativeID: "native-attacker", Name: "replacement", ImageRoot: "sha256:new"}); err == nil {
		t.Fatal("insert-only ownership accepted a duplicate public ID")
	}
	row, err := repo.FindByID(original.ID)
	if err != nil || row == nil || row.NativeID != original.NativeID || row.Name != original.Name || row.Image != original.Image || row.ImageRoot != original.ImageRoot {
		t.Fatalf("duplicate create changed existing public history: row=%+v err=%v", row, err)
	}
	if err := repo.CreateOwnership(Sandbox{ID: "sbx-second", NativeID: "native-second", Name: "second"}); err != nil {
		t.Fatal(err)
	}
	if row, err := repo.FindByID("sbx-second"); err != nil || row == nil || row.NativeID != "native-second" {
		t.Fatalf("insert-only create failed: row=%+v err=%v", row, err)
	}
}

func TestRepositoryOperationsPropagateClosedDatabaseErrors(t *testing.T) {
	db := New(":memory:")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	public, native := NewRepository(db), NewRepository(db).NativeView()
	checks := []struct {
		name string
		call func() error
	}{
		{"native lookup", func() error { _, err := native.FindByNativeID("native"); return err }},
		{"native save resolution", func() error { return native.Save(Sandbox{ID: "native"}) }},
		{"public lookup", func() error { _, err := public.FindByID("public"); return err }},
		{"native lookup", func() error { _, err := native.FindByID("native"); return err }},
		{"public list", func() error { _, err := public.FindAll(); return err }},
		{"native port update resolution", func() error { return native.UpdatePorts("native", JSONMap{}) }},
		{"name lookup", func() error { _, err := public.FindByName("sandbox"); return err }},
		{"native delete resolution", func() error { return native.Delete("native") }},
		{"native command save resolution", func() error { return native.SaveCommand(Command{ID: "cmd", SandboxID: "native"}) }},
		{"command lookup", func() error { _, err := public.FindCommandByID("cmd"); return err }},
		{"native command list resolution", func() error { _, err := native.FindCommandsBySandbox("native"); return err }},
		{"public command list", func() error { _, err := public.FindCommandsBySandbox("public"); return err }},
		{"command finish update", func() error { return public.UpdateCommandFinished("cmd", 1, 1) }},
		{"native command delete resolution", func() error { return native.DeleteCommandsBySandbox("native") }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("closed database error was suppressed")
			}
		})
	}
}
