package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExecutionPath never silently moves/adopts a working-directory database.
// Explicit legacy mode retains its original file, IDs and command foreign keys.
func (c *Config) ExecutionPath(runtime string) (string, error) {
	name := "sandbox.db"
	if runtime == "container" {
		name = "sandbox-container.db"
	}
	if c.LegacyDB != "" {
		if filepath.Base(c.LegacyDB) != name {
			return "", fmt.Errorf("legacy database for %s must be named %s; select its original runtime", runtime, name)
		}
		info, err := os.Stat(c.LegacyDB)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("legacy database is not a regular file")
		}
		return filepath.Abs(c.LegacyDB)
	}
	for _, legacy := range []string{"sandbox.db", "sandbox-container.db"} {
		if _, err := os.Stat(legacy); err == nil {
			originalRuntime := "docker"
			if legacy == "sandbox-container.db" {
				originalRuntime = "container"
			}
			return "", fmt.Errorf("legacy %s detected: stop the old service, make a SQLite backup, then explicitly use -runtime %s -legacy-db /absolute/path/%s; no files were moved", legacy, originalRuntime, legacy)
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	dir := filepath.Join(c.DataDir, "runtimes", runtime)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "sandbox.db"), nil
}
