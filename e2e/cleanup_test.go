//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"opensbx/internal/database"
)

func (h *harness) native(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, h.runtime, args...).CombinedOutput()
}

func (h *harness) preflight(t *testing.T) {
	t.Helper()
	if h.runtime == "docker" {
		b, err := h.native("info", "--format", "{{.OSType}}/{{.Architecture}}")
		require.NoError(t, err, "Docker is required (never skipped): %s", b)
		h.platform = strings.TrimSpace(string(b))
		h.platform = strings.ReplaceAll(h.platform, "aarch64", "arm64")
		h.platform = strings.ReplaceAll(h.platform, "x86_64", "amd64")
	} else {
		b, err := h.native("system", "status")
		require.NoError(t, err, "Apple container must already be running: %s", b)
		h.platform = "linux/arm64"
	}
	require.Contains(t, []string{"linux/amd64", "linux/arm64"}, h.platform)
	var err error
	h.baseline, err = h.imageRefs()
	require.NoError(t, err, "snapshot native image references before creating resources")
}

func (h *harness) imageRefs() (map[string]bool, error) {
	refs := map[string]bool{}
	if h.runtime == "docker" {
		b, err := h.native("image", "ls", "--format", "{{.Repository}}:{{.Tag}}")
		if err != nil {
			return nil, fmt.Errorf("list Docker image refs: %w: %s", err, b)
		}
		for _, ref := range strings.Fields(string(b)) {
			refs[ref] = true
		}
	} else {
		b, err := h.native("image", "list", "--format", "json")
		if err != nil {
			return nil, fmt.Errorf("list Apple image refs: %w: %s", err, b)
		}
		var entries []struct{ Configuration struct{ Name string } }
		if err := json.Unmarshal(b, &entries); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			refs[entry.Configuration.Name] = true
		}
	}
	return refs, nil
}

func (h *harness) inventory() (map[string]string, error) {
	states := map[string]string{}
	if h.runtime == "docker" {
		b, err := h.native("ps", "--all", "--no-trunc", "--format", "{{.ID}} {{.State}}")
		if err != nil {
			return nil, fmt.Errorf("Docker inventory: %w: %s", err, b)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 2 {
				return nil, fmt.Errorf("invalid Docker inventory line: %q", line)
			}
			states[fields[0]] = fields[1]
		}
	} else {
		b, err := h.native("list", "--all", "--format", "json")
		if err != nil {
			return nil, fmt.Errorf("Apple inventory: %w: %s", err, b)
		}
		var entries []struct {
			Configuration struct{ ID string }
			Status        struct{ State string }
		}
		if err := json.Unmarshal(b, &entries); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Configuration.ID == "" || entry.Status.State == "" {
				return nil, fmt.Errorf("invalid Apple inventory entry")
			}
			states[entry.Configuration.ID] = entry.Status.State
		}
	}
	return states, nil
}

// Open existing state read-only: assertions must not migrate, repair, or create
// the database they are verifying.
func (h *harness) database() (*gorm.DB, func(), error) {
	path := filepath.Join(h.data, "runtimes", h.runtime, "sandbox.db")
	if _, err := os.Stat(path); err != nil {
		return nil, nil, err
	}
	db, err := gorm.Open(sqlite.Open("file:"+filepath.ToSlash(path)+"?mode=ro"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	return db, func() { _ = sqlDB.Close() }, nil
}

func (h *harness) captureOwnership() error {
	db, closeDB, err := h.database()
	if err != nil {
		return err
	}
	defer closeDB()
	var rows []database.Sandbox
	if err := db.Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if !strings.HasPrefix(row.ID, "sbx-") || row.NativeID == "" {
			return fmt.Errorf("invalid identity in isolated database: %q", row.ID)
		}
		h.owned[row.ID] = row.NativeID
	}
	return nil
}

func (h *harness) emptyDB() error {
	db, closeDB, err := h.database()
	if err != nil {
		return err
	}
	defer closeDB()
	for _, model := range []any{&database.Sandbox{}, &database.Command{}} {
		var n int64
		if err := db.Model(model).Count(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("%T retains %d test records", model, n)
		}
	}
	return nil
}

func (h *harness) cleanup(t *testing.T) {
	// Capture recovery rows even when a create failed before returning an ID.
	if err := h.captureOwnership(); err != nil && !os.IsNotExist(err) {
		t.Errorf("cleanup ownership discovery: %v", err)
	}
	if h.done != nil {
		select {
		case <-h.done:
		default:
			for id := range h.owned {
				status, b, err := h.request("DELETE", "/v1/sandboxes/"+id, nil, h.key, nil)
				if err != nil || (status != 204 && status != 404) {
					t.Errorf("API cleanup %s: status=%d err=%v body=%s", id, status, err, b)
				}
			}
		}
	}
	if err := h.stop(); err != nil {
		t.Errorf("cleanup server termination: %v", err)
	}
	if len(h.owned) > 0 {
		states, err := h.inventory()
		if err != nil {
			t.Errorf("cleanup native inventory: %v", err)
		} else {
			for _, id := range h.owned {
				if _, exists := states[id]; !exists {
					continue
				}
				t.Errorf("API cleanup left native sandbox %s; attempting exact-resource fallback", id)
				// This fallback is limited to exact IDs from our private DB.
				args := []string{"rm", "--force", id}
				if h.runtime == "container" {
					args = []string{"delete", "--force", id}
				}
				b, err := h.native(args...)
				if err != nil {
					t.Errorf("fallback removal %s: %v: %s", id, err, b)
				}
			}
			states, err = h.inventory()
			if err != nil {
				t.Errorf("verify native cleanup: %v", err)
			}
			for _, id := range h.owned {
				if _, exists := states[id]; exists {
					t.Errorf("test-owned sandbox remains in runtime: %s", id)
				}
			}
		}
	}
	if err := h.emptyDB(); err != nil && !os.IsNotExist(err) {
		t.Errorf("database cleanup verification: %v", err)
	}
	if h.cache != "" && !h.baseline[h.cache] {
		refs, err := h.imageRefs()
		if err != nil {
			t.Errorf("cache cleanup inventory: %v", err)
		} else if refs[h.cache] {
			args := []string{"image", "rm", h.cache}
			if h.runtime == "container" {
				args = []string{"image", "delete", h.cache}
			}
			b, err := h.native(args...)
			if err != nil {
				t.Errorf("cache cleanup: %v: %s", err, b)
			}
			refs, err = h.imageRefs()
			if err != nil || refs[h.cache] {
				t.Errorf("cache reference remains or verification failed: %v", err)
			}
		}
	}
	if h.baseline != nil {
		refs, err := h.imageRefs()
		if err != nil {
			t.Errorf("verify preserved native image references: %v", err)
		} else {
			for ref := range h.baseline {
				if ref != "<none>:<none>" && !refs[ref] {
					t.Errorf("pre-existing native image reference is missing: %s", ref)
				}
			}
		}
	}
	var logs strings.Builder
	for i, log := range h.logs {
		fmt.Fprintf(&logs, "\n--- server process %d ---\n%s", i+1, log.text())
	}
	if t.Failed() {
		t.Log(logs.String())
	}
	if dir := os.Getenv("OPENSBX_E2E_ARTIFACTS"); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Errorf("create diagnostic directory: %v", err)
		} else if err := os.WriteFile(filepath.Join(dir, h.runtime+"-server.log"), []byte(logs.String()), 0600); err != nil {
			t.Errorf("save diagnostic log: %v", err)
		}
	}
	h.http.CloseIdleConnections()
	if err := os.RemoveAll(h.root); err != nil {
		t.Errorf("remove isolated database/catalog directory: %v", err)
	}
	if _, err := os.Stat(h.root); !os.IsNotExist(err) {
		t.Errorf("isolated test state still exists: %v", err)
	}
	if !t.Failed() {
		t.Log("Cleanup verified: no owned sandboxes, no sandbox/command rows, no introduced cache reference; temporary database and catalog removed")
	}
}
