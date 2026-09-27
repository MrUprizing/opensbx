package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionPathSeparatesRuntimeDatabases(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &Config{DataDir: dataDir}
	dockerPath, err := cfg.ExecutionPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	containerPath, err := cfg.ExecutionPath("container")
	if err != nil {
		t.Fatal(err)
	}
	if dockerPath == containerPath || filepath.Base(dockerPath) != "sandbox.db" || filepath.Base(containerPath) != "sandbox.db" || filepath.Base(filepath.Dir(dockerPath)) != "docker" || filepath.Base(filepath.Dir(containerPath)) != "container" {
		t.Fatalf("runtime DB paths docker=%q container=%q", dockerPath, containerPath)
	}
	for _, path := range []string{filepath.Dir(dockerPath), filepath.Dir(containerPath)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("runtime state directory %q mode=%#o exposes owner data", path, info.Mode().Perm())
		}
	}
}

func TestExecutionPathDetectsLegacyDatabaseWithoutMovingIt(t *testing.T) {
	work := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	legacy := filepath.Join(work, "sandbox.db")
	if err := os.WriteFile(legacy, []byte("legacy-db-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := (&Config{DataDir: filepath.Join(work, "new-data")}).ExecutionPath("docker")
	if err == nil || !strings.Contains(err.Error(), "explicitly use -runtime docker -legacy-db") {
		t.Fatalf("legacy detection result path=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(legacy); err != nil || string(got) != "legacy-db-fixture" {
		t.Fatalf("legacy file moved or changed: contents=%q err=%v", got, err)
	}
}

func TestExecutionPathRequiresExistingRuntimeMatchedRegularLegacyFile(t *testing.T) {
	work := t.TempDir()
	dockerDB := filepath.Join(work, "sandbox.db")
	if err := os.WriteFile(dockerDB, []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := (&Config{LegacyDB: dockerDB}).ExecutionPath("docker")
	if err != nil || path != dockerDB {
		t.Fatalf("explicit legacy path=%q err=%v", path, err)
	}
	if _, err := (&Config{LegacyDB: dockerDB}).ExecutionPath("container"); err == nil {
		t.Fatal("Docker database accepted for container runtime")
	}
	if _, err := (&Config{LegacyDB: filepath.Join(work, "missing.db")}).ExecutionPath("docker"); err == nil {
		t.Fatal("missing legacy database accepted")
	}
	dir := filepath.Join(work, "sandbox-container.db")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Config{LegacyDB: dir}).ExecutionPath("container"); err == nil {
		t.Fatal("directory accepted as legacy database file")
	}
}
