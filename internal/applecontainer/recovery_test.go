package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

func TestRecoverRestoresExactDeadlineAndIgnoresOtherRuntimeRowsAndIntents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apple-recovery.db")
	base := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	deadline := base.Add(45 * time.Minute)
	db := database.New(path)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	repo := database.NewRepository(db)
	const appleID = "opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const dockerID = "container-docker"
	if err := repo.CreateOwnership(database.Sandbox{ID: appleID, NativeID: appleID, RuntimeKind: "container", AttemptToken: appleID, Name: "apple", ExpiresAt: &deadline}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateOwnership(database.Sandbox{ID: "docker-public", NativeID: dockerID, RuntimeKind: "docker", AttemptToken: "docker-token", Name: "docker", ExpiresAt: timePointer(base.Add(-time.Minute))}); err != nil {
		t.Fatal(err)
	}
	foreignOp := database.Operation{ID: "docker:pending-delete", RuntimeKind: "docker", PublicID: "docker-public", NativeID: dockerID, Token: "docker-token", Kind: "delete"}
	if err := db.Create(&foreignOp).Error; err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := database.New(path)
	reopenedSQL, err := reopened.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedSQL.Close() })
	runner := &scriptedRunner{t: t, run: func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			return errors.New("unexpected native mutation during future-deadline recovery")
		}
		_, _ = io.WriteString(out, listJSON(appleID, "running", "[]"))
		return nil
	}}
	client := New(database.NewRepository(reopened), runner, func() time.Time { return base })
	if err := client.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() = %v", err)
	}
	client.mu.Lock()
	entry := client.timers[appleID]
	client.clearTimer(appleID)
	_, dockerTimer := client.timers[dockerID]
	client.mu.Unlock()
	if entry == nil || !entry.at.Equal(deadline) {
		t.Fatalf("restored Apple timer deadline=%v; want %v", expirationDeadline(entry), deadline)
	}
	if dockerTimer {
		t.Fatal("Apple recovery restored a Docker deadline")
	}
	row, err := database.NewRepository(reopened).FindByID(appleID)
	if err != nil || row == nil || row.ExpiresAt == nil || !row.ExpiresAt.Equal(deadline) {
		t.Fatalf("recovery changed persisted deadline: row=%+v err=%v", row, err)
	}
	ops, err := database.NewRepository(reopened).Operations("docker")
	if err != nil || len(ops) != 1 || ops[0].ID != foreignOp.ID {
		t.Fatalf("Apple recovery processed another runtime's intent: ops=%+v err=%v", ops, err)
	}
}

func TestRecoverRetriesOverdueStopWithOriginalDeadline(t *testing.T) {
	const id = "opensbx-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	base := time.Date(2036, 7, 8, 9, 10, 11, 0, time.UTC)
	deadline := base.Add(-time.Minute)
	var state = "running"
	stopFailures := 1
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(id, state, "[]"))
		case reflect.DeepEqual(args, []string{"stop", id}):
			if stopFailures > 0 {
				stopFailures--
				return errors.New("temporary stop failure")
			}
			state = "stopped"
		default:
			return fmt.Errorf("unexpected recovery command %#v", args)
		}
		return nil
	}
	db := database.New(t.TempDir() + "/overdue.db")
	repo := database.NewRepository(db)
	if err := repo.CreateOwnership(database.Sandbox{ID: id, NativeID: id, RuntimeKind: "container", AttemptToken: id, Name: "overdue", ExpiresAt: &deadline}); err != nil {
		t.Fatal(err)
	}
	client := New(repo, runner, func() time.Time { return base })
	cleanupAppleTestClient(t, client, db)
	if err := client.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() should keep retrying after transient stop failure: %v", err)
	}
	client.mu.Lock()
	entry := client.timers[id]
	client.mu.Unlock()
	if entry == nil || !entry.at.Equal(deadline) {
		t.Fatalf("retry expiration=%v; want original %v", expirationDeadline(entry), deadline)
	}
	row, err := repo.FindByID(id)
	if err != nil || row == nil || row.ExpiresAt == nil || !row.ExpiresAt.Equal(deadline) {
		t.Fatalf("transient retry changed durable deadline: row=%+v err=%v", row, err)
	}
	ops, err := repo.Operations("container")
	if err != nil || len(ops) != 1 || ops[0].Kind != "stop" {
		t.Fatalf("pending stop intent=%+v err=%v", ops, err)
	}

	client.mu.Lock()
	err = client.reconcileResource(context.Background(), id)
	client.clearTimer(id)
	client.mu.Unlock()
	if err != nil {
		t.Fatalf("retry reconciliation = %v", err)
	}
	row, err = repo.FindByID(id)
	if err != nil || row == nil || row.ExpiresAt != nil {
		t.Fatalf("successful retry did not clear deadline: row=%+v err=%v", row, err)
	}
	ops, err = repo.Operations("container")
	if err != nil || len(ops) != 0 {
		t.Fatalf("successful retry retained intent=%+v err=%v", ops, err)
	}
}

func TestRecoverRefusesForeignLabelWithoutDeletingPendingResource(t *testing.T) {
	const id = "opensbx-cccccccccccccccccccccccccccccccc"
	db := database.New(t.TempDir() + "/foreign.db")
	repo := database.NewRepository(db)
	if err := repo.CreateOwnership(database.Sandbox{ID: id, NativeID: id, RuntimeKind: "container", AttemptToken: id, Name: "owned"}); err != nil {
		t.Fatal(err)
	}
	op := database.Operation{ID: "container:delete", RuntimeKind: "container", PublicID: id, NativeID: id, Token: id, Kind: "delete"}
	if err := db.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	runner := &scriptedRunner{t: t, run: func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			return fmt.Errorf("recovery issued unsafe native call: %#v", args)
		}
		payload := strings.Replace(listJSON(id, "running", "[]"), ownerLabel+`":"`+id, ownerLabel+`":"foreign-token`, 1)
		_, _ = io.WriteString(out, payload)
		return nil
	}}
	client := New(repo, runner, time.Now)
	cleanupAppleTestClient(t, client, db)
	err := client.Recover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ownership label mismatch") {
		t.Fatalf("foreign-label recovery error=%v; want ownership refusal", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("foreign resource received native mutation: %+v", runner.calls)
	}
	row, err := repo.FindByID(id)
	if err != nil || row == nil {
		t.Fatalf("ownership row lost after safe refusal: row=%+v err=%v", row, err)
	}
	ops, err := repo.Operations("container")
	if err != nil || len(ops) != 1 || ops[0].ID != op.ID {
		t.Fatalf("pending delete intent lost after safe refusal: ops=%+v err=%v", ops, err)
	}
}

func TestShutdownCancelsQueuedRecoveryAndExpirationTimers(t *testing.T) {
	db := database.New(t.TempDir() + "/shutdown-recovery.db")
	runner := &scriptedRunner{t: t}
	client := New(database.NewRepository(db), runner, time.Now)
	cleanupAppleTestClient(t, client, db)
	var workRan atomic.Bool
	client.recovery.Schedule("pending-intent", func(context.Context) error {
		workRan.Store(true)
		return nil
	})
	deadline := time.Now().Add(time.Hour)
	client.mu.Lock()
	client.scheduleDeadline("unowned-test-timer", deadline, time.Hour)
	client.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client.Shutdown(ctx)
	if workRan.Load() {
		t.Fatal("shutdown ran a delayed recovery job")
	}
	client.mu.Lock()
	_, timerTracked := client.timers["unowned-test-timer"]
	client.mu.Unlock()
	if timerTracked {
		t.Fatal("shutdown retained an expiration timer")
	}
	if err := client.recovery.Run(context.Background(), "pending-intent"); err != nil || workRan.Load() {
		t.Fatalf("stopped queue revived work: err=%v ran=%t", err, workRan.Load())
	}
}

func TestCreatePersistsIntentBeforeNativeCallAndRecoversLostResponse(t *testing.T) {
	const imageRef = "opensbx.invalid/cache@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	db := database.New(t.TempDir() + "/lost-create.db")
	repo := database.NewRepository(db)
	var client *Client
	nativeID := ""
	deleteFailures := 1
	createObservedIntent := false
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}):
			_, _ = io.WriteString(out, `[{"ID":"sha256:native","Configuration":{"Name":"`+imageRef+`","Descriptor":{"Digest":"sha256:native"}},"Variants":[{"Platform":{"Architecture":"arm64","OS":"linux"},"Size":1}]}]`)
		case len(args) > 0 && args[0] == "create":
			ops, err := repo.Operations("container")
			if err != nil || len(ops) != 1 || ops[0].Kind != "create" || ops[0].Token != args[4] || ops[0].NativeID != args[4] {
				return fmt.Errorf("native create reached before exact durable intent: ops=%+v err=%v args=%v", ops, err, args)
			}
			createObservedIntent = true
			nativeID = args[4]
			return errors.New("create response lost after native resource creation")
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			if nativeID == "" {
				return errors.New("inventory requested before native resource exists")
			}
			_, _ = io.WriteString(out, listJSON(nativeID, "running", "[]"))
		case len(args) == 3 && reflect.DeepEqual(args[:2], []string{"delete", "--force"}):
			if args[2] != nativeID {
				return fmt.Errorf("deleted %s, want exact attempted resource %s", args[2], nativeID)
			}
			if deleteFailures > 0 {
				deleteFailures--
				return errors.New("temporary delete failure")
			}
		default:
			return fmt.Errorf("unexpected create recovery command %#v", args)
		}
		return nil
	}
	client = New(repo, runner, func() time.Time { return time.Date(2037, 1, 2, 3, 4, 5, 0, time.UTC) })
	cleanupAppleTestClient(t, client, db)
	_, err := client.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: imageRef})
	if err == nil || !strings.Contains(err.Error(), "Apple container create failed") || !strings.Contains(err.Error(), "Apple container delete failed") {
		t.Fatalf("Create() lost-response compensation error=%v", err)
	}
	if !createObservedIntent || nativeID == "" {
		t.Fatal("create did not observe its durable token-bound intent before native call")
	}
	row, err := repo.FindByID(nativeID)
	if err != nil || row == nil || row.NativeID != nativeID || row.AttemptToken != nativeID {
		t.Fatalf("failed rollback did not retain exact ownership: row=%+v err=%v", row, err)
	}
	ops, err := repo.Operations("container")
	if err != nil || len(ops) != 1 || ops[0].Kind != "create" || ops[0].NativeID != nativeID {
		t.Fatalf("failed rollback did not retain create intent: ops=%+v err=%v", ops, err)
	}
	if err := client.recovery.Run(context.Background(), ops[0].ID); err != nil {
		t.Fatalf("same-process recovery after create response loss = %v", err)
	}
	row, err = repo.FindByID(nativeID)
	if err != nil || row != nil {
		t.Fatalf("reconciled create left ownership row=%+v err=%v", row, err)
	}
	ops, err = repo.Operations("container")
	if err != nil || len(ops) != 0 {
		t.Fatalf("reconciled create retained intent=%+v err=%v", ops, err)
	}
	callsAfterRecovery := len(runner.calls)
	if err := client.Recover(context.Background()); err != nil {
		t.Fatalf("repeated Recover() = %v", err)
	}
	if len(runner.calls) != callsAfterRecovery {
		t.Fatalf("idempotent recovery repeated native work: before=%d after=%d calls=%+v", callsAfterRecovery, len(runner.calls), runner.calls)
	}
}

func TestCreateIntentRemainsUntilRuntimeioProvenanceAdoption(t *testing.T) {
	const imageRef = "opensbx.invalid/cache@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const publicID = sandbox.SandboxID("sbx-dddddddddddddddddddddddddddddddd")
	db := database.New(t.TempDir() + "/await-adoption.db")
	repo := database.NewRepository(db)
	nativeID := ""
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}):
			_, _ = io.WriteString(out, `[{"ID":"sha256:native","Configuration":{"Name":"`+imageRef+`","Descriptor":{"Digest":"sha256:native"}},"Variants":[{"Platform":{"Architecture":"arm64","OS":"linux"},"Size":1}]}]`)
		case len(args) > 0 && args[0] == "create":
			nativeID = args[4]
			ops, err := repo.Operations("container")
			if err != nil || len(ops) != 1 || ops[0].Kind != "create" || ops[0].NativeID != nativeID || ops[0].PublicID != string(publicID) {
				return fmt.Errorf("native create ran without the exact public/native intent: ops=%+v err=%v", ops, err)
			}
		case len(args) == 2 && args[0] == "start" && args[1] == nativeID:
		default:
			return fmt.Errorf("unexpected create/adoption command %#v", args)
		}
		return nil
	}
	client := New(repo, runner, time.Now)
	cleanupAppleTestClient(t, client, db)
	adapter := runtimeio.New(client, recoveryImageCache{reference: imageRef}, repo)
	prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
	if err != nil {
		t.Fatal(err)
	}
	provision, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: publicID, Image: prepared, Timeout: time.Minute, Resources: sandbox.ResourceLimits{MemoryMB: 256, CPUs: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if nativeID == "" {
		t.Fatal("runtime did not create a native resource")
	}
	ops, err := repo.Operations("container")
	if err != nil || len(ops) != 1 || ops[0].Kind != "create" {
		t.Fatalf("create intent retired before provenance adoption: ops=%+v err=%v", ops, err)
	}
	createIntentID := ops[0].ID
	if err := client.recovery.Run(context.Background(), createIntentID); err != nil {
		t.Fatalf("probe successful Apple create recovery job: %v", err)
	}
	if row, err := repo.FindByID(string(publicID)); err != nil || row == nil {
		t.Fatalf("successful Apple create was rolled back before Adopt: row=%+v err=%v", row, err)
	}
	row, err := repo.FindByID(string(publicID))
	if err != nil || row == nil || row.NativeID != nativeID {
		t.Fatalf("pre-adoption ownership row=%+v err=%v", row, err)
	}
	if err := provision.Adopt(context.Background(), sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest", CacheVersion: "container:1.4.1"}); err != nil {
		t.Fatalf("Adopt() = %v", err)
	}
	ops, err = repo.Operations("container")
	if err != nil || len(ops) != 0 {
		t.Fatalf("provenance adoption did not atomically retire create intent: ops=%+v err=%v", ops, err)
	}
	callsBeforeAdoptProbe := len(runner.calls)
	if err := client.recovery.Run(context.Background(), createIntentID); err != nil {
		t.Fatalf("completed create recovery probe = %v", err)
	}
	if len(runner.calls) != callsBeforeAdoptProbe {
		t.Fatalf("completed create had stale recovery work after Adopt: before=%d after=%d", callsBeforeAdoptProbe, len(runner.calls))
	}
	row, err = repo.FindByID(string(publicID))
	if err != nil || row == nil || row.NativeID != nativeID || row.ImageRoot != "sha256:root" || row.ImageManifest != "sha256:manifest" {
		t.Fatalf("adopted row provenance=%+v err=%v", row, err)
	}
}

type recoveryImageCache struct{ reference string }

func (c recoveryImageCache) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{Runtime: "container", Version: "1.4.1"}, nil
}

func (c recoveryImageCache) Materialize(context.Context, sandbox.Image) (string, error) {
	return c.reference, nil
}

func expirationDeadline(entry *expiration) time.Time {
	if entry == nil {
		return time.Time{}
	}
	return entry.at
}

func timePointer(value time.Time) *time.Time { return &value }
