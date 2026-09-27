package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/sandbox"
)

func TestDeleteAndListFileOperationsUseFixedScriptsAndReturnGuestErrors(t *testing.T) {
	id := "opensbx-22222222222222222222222222222222"
	r := &scriptedRunner{t: t}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(id, "running", "[]"))
		case len(args) == 8 && args[0] == "exec" && args[1] == "--interactive" && args[2] == id && args[3] == "/bin/sh" && args[4] == "-c" && args[6] == "opensbx-file" && args[7] == "relative":
			if args[5] != `case "$1" in /*) ;; *) set -- "./$1";; esac; exec ls -la "$1"` {
				t.Fatalf("ListDir script/argument changed: %#v", args)
			}
			_, _ = io.WriteString(out, "directory listing\n")
		case len(args) == 8 && args[0] == "exec" && args[1] == "--interactive" && args[2] == id && args[3] == "/bin/sh" && args[4] == "-c" && args[6] == "opensbx-file" && args[7] == "/tmp/unlink me":
			if args[5] != `case "$1" in /*) ;; *) set -- "./$1";; esac; exec rm -rf -- "$1"` {
				t.Fatalf("DeleteFile script/argument changed: %#v", args)
			}
			return errors.New("guest exited with status 1")
		default:
			t.Fatalf("unexpected file CLI argv: %#v", args)
		}
		return nil
	}
	db := database.New(t.TempDir() + "/files.db")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	c := New(repo, r, time.Now)
	listing, err := c.ListDir(context.Background(), id, "relative")
	if err != nil || listing != "directory listing\n" {
		t.Fatalf("ListDir() = %q, %v", listing, err)
	}
	if err := c.DeleteFile(context.Background(), id, "/tmp/unlink me"); err == nil {
		t.Fatal("guest nonzero exit was reported as successful delete")
	}
}

func TestStatsUsesTwoTimedCPUObservationsAndLatestMemoryAndPIDs(t *testing.T) {
	id := "opensbx-cccccccccccccccccccccccccccccccc"
	r := &scriptedRunner{t: t}
	count := 0
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(id, "running", "[]"))
		case reflect.DeepEqual(args, []string{"stats", "--no-stream", "--format", "json", id}):
			count++
			cpu, memory, pids := 100000, 250000000, 8
			if count == 2 {
				cpu, memory, pids = 600000, 500000000, 11
			}
			_, _ = fmt.Fprintf(out, `[{"id":%q,"cpuUsageUsec":%d,"memoryUsageBytes":%d,"memoryLimitBytes":1000000000,"numProcesses":%d}]`, id, cpu, memory, pids)
		default:
			t.Fatalf("unexpected stats argv: %#v", args)
		}
		return nil
	}
	db := database.New(t.TempDir() + "/stats.db")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	calls := 0
	c := New(repo, r, func() time.Time { calls++; return base.Add(time.Duration(calls-1) * time.Second) })
	got, err := c.Stats(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.CPU != 50 || got.Memory.Usage != 500000000 || got.Memory.Limit != 1000000000 || got.Memory.Percent != 50 || got.PIDs != 11 {
		t.Fatalf("Stats() = %+v; expected interval CPU 50%% and second memory/PID sample", got)
	}
	if count != 2 {
		t.Fatalf("stats samples = %d, want 2", count)
	}
}

func TestStatsRejectsMissingFieldsAndCounterReset(t *testing.T) {
	id := "opensbx-dddddddddddddddddddddddddddddddd"
	for _, tc := range []struct {
		name          string
		first, second string
		sameTime      bool
	}{
		{"missing pids", `[{"id":"opensbx-dddddddddddddddddddddddddddddddd","cpuUsageUsec":1,"memoryUsageBytes":1,"memoryLimitBytes":2}]`, `[{"id":"opensbx-dddddddddddddddddddddddddddddddd","cpuUsageUsec":2,"memoryUsageBytes":1,"memoryLimitBytes":2,"numProcesses":1}]`, false},
		{"counter reset", `[{"id":"opensbx-dddddddddddddddddddddddddddddddd","cpuUsageUsec":20,"memoryUsageBytes":1,"memoryLimitBytes":2,"numProcesses":1}]`, `[{"id":"opensbx-dddddddddddddddddddddddddddddddd","cpuUsageUsec":10,"memoryUsageBytes":1,"memoryLimitBytes":2,"numProcesses":1}]`, false},
		{"clock did not advance", `[{"id":"opensbx-dddddddddddddddddddddddddddddddd","cpuUsageUsec":1,"memoryUsageBytes":1,"memoryLimitBytes":2,"numProcesses":1}]`, `[{"id":"opensbx-dddddddddddddddddddddddddddddddd","cpuUsageUsec":2,"memoryUsageBytes":1,"memoryLimitBytes":2,"numProcesses":1}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedRunner{t: t}
			sample := 0
			r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
				if reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
					_, _ = io.WriteString(out, listJSON(id, "running", "[]"))
					return nil
				}
				if !reflect.DeepEqual(args, []string{"stats", "--no-stream", "--format", "json", id}) {
					t.Fatalf("unexpected stats argv %#v", args)
				}
				sample++
				if sample == 1 {
					_, _ = io.WriteString(out, tc.first)
				} else {
					_, _ = io.WriteString(out, tc.second)
				}
				return nil
			}
			db := database.New(t.TempDir() + "/stats.db")
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			repo := database.NewRepository(db)
			if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
				t.Fatal(err)
			}
			n := 0
			base := time.Now()
			c := New(repo, r, func() time.Time {
				n++
				if tc.sameTime {
					return base
				}
				return base.Add(time.Duration(n) * time.Second)
			})
			if _, err := c.Stats(context.Background(), id); err == nil {
				t.Fatal("invalid/reset stats samples were accepted")
			}
		})
	}
}

func TestStatsRevalidatesSandboxStateAfterSampling(t *testing.T) {
	id := "opensbx-19191919191919191919191919191919"
	for _, tc := range []struct {
		name string
		want error
	}{
		{name: "stopped while sampling", want: sandbox.ErrNotRunning},
		{name: "restarted while sampling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inventoryCalls, sampleCalls := 0, 0
			r := &scriptedRunner{t: t}
			r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
				switch {
				case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
					inventoryCalls++
					state := "running"
					payload := listJSON(id, state, "[]")
					if inventoryCalls == 2 && tc.name == "stopped while sampling" {
						payload = listJSON(id, "stopped", "[]")
					}
					if inventoryCalls == 2 && tc.name == "restarted while sampling" {
						payload = strings.Replace(payload, "2026-09-26T00:00:00Z", "2026-09-26T00:00:01Z", 1)
					}
					_, _ = io.WriteString(out, payload)
				case reflect.DeepEqual(args, []string{"stats", "--no-stream", "--format", "json", id}):
					sampleCalls++
					_, _ = fmt.Fprintf(out, `[{"id":%q,"cpuUsageUsec":%d,"memoryUsageBytes":10,"memoryLimitBytes":20,"numProcesses":1}]`, id, uint64(sampleCalls*100))
				default:
					t.Fatalf("unexpected stats revalidation argv: %#v", args)
				}
				return nil
			}
			db := database.New(t.TempDir() + "/stats-revalidate.db")
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			repo := database.NewRepository(db)
			if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
				t.Fatal(err)
			}
			base := time.Now()
			clockCalls := 0
			client := New(repo, r, func() time.Time { clockCalls++; return base.Add(time.Duration(clockCalls) * time.Second) })
			_, err = client.Stats(context.Background(), id)
			if err == nil {
				t.Fatal("Stats returned a sample for a sandbox that stopped/restarted during sampling")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Stats error=%v, want %v", err, tc.want)
			}
			if tc.want == nil && !strings.Contains(err.Error(), "restarted") {
				t.Fatalf("Stats restart error=%v", err)
			}
			if inventoryCalls != 2 || sampleCalls != 2 {
				t.Fatalf("inventory/sample calls=%d/%d, want 2/2", inventoryCalls, sampleCalls)
			}
		})
	}
}
