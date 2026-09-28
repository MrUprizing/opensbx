package docker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

func TestRecoverRestoresUnchangedPersistedDeadlineAfterSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker-recovery.db")
	db := database.New(path)
	firstSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	repo := database.NewRepository(db)
	deadline := time.Now().Add(45 * time.Minute).UTC().Round(0)
	if err := repo.CreateOwnership(database.Sandbox{ID: "public-id", NativeID: "container-1", RuntimeKind: "docker", Name: "demo", ExpiresAt: &deadline}); err != nil {
		t.Fatal(err)
	}
	if err := firstSQL.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := database.New(path)
	reopenedSQL, err := reopened.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedSQL.Close() })
	dc, fixture := newDockerFixture(t)
	dc.repo = database.NewRepository(reopened).NativeView()
	if err := dc.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() = %v", err)
	}
	if err := dc.Recover(context.Background()); err != nil {
		t.Fatalf("repeated Recover() = %v", err)
	}
	entry := dc.getTimerEntry("container-1")
	if entry == nil || !entry.expiresAt.Equal(deadline) {
		t.Fatalf("restored timer deadline=%v; want persisted %v", entryDeadline(entry), deadline)
	}
	row, err := database.NewRepository(reopened).FindByID("public-id")
	if err != nil || row == nil || row.ExpiresAt == nil || !row.ExpiresAt.Equal(deadline) {
		t.Fatalf("recovered persisted deadline=%v row=%+v err=%v", sandboxDeadline(row), row, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.requests) != 0 {
		t.Fatalf("future deadline recovery made unexpected native requests: %v", fixture.requests)
	}
}

func TestRecoverRetriesOverdueStopWithoutChangingOriginalDeadline(t *testing.T) {
	db := database.New(filepath.Join(t.TempDir(), "docker-overdue.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbSQL.Close() })
	dc, fixture := newDockerFixture(t)
	dc.repo = database.NewRepository(db).NativeView()
	deadline := time.Now().Add(-time.Minute).UTC().Round(0)
	if err := dc.repo.CreateOwnership(database.Sandbox{ID: "public-id", NativeID: "container-1", RuntimeKind: "docker", Name: "demo", ExpiresAt: &deadline}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.fail["POST /containers/container-1/stop"] = 500
	fixture.mu.Unlock()
	if err := dc.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() should schedule a retry after transient native stop failure: %v", err)
	}
	entry := dc.getTimerEntry("container-1")
	if entry == nil || !entry.expiresAt.Equal(deadline) {
		t.Fatalf("retry timer deadline=%v; want original %v", entryDeadline(entry), deadline)
	}
	row, err := database.NewRepository(db).FindByID("public-id")
	if err != nil || row == nil || row.ExpiresAt == nil || !row.ExpiresAt.Equal(deadline) {
		t.Fatalf("transient stop changed durable deadline=%v row=%+v err=%v", sandboxDeadline(row), row, err)
	}
	ops, err := dc.repo.Operations("docker")
	if err != nil || len(ops) != 1 || ops[0].Kind != "stop" {
		t.Fatalf("failed stop intent=%+v err=%v", ops, err)
	}

	fixture.mu.Lock()
	delete(fixture.fail, "POST /containers/container-1/stop")
	fixture.mu.Unlock()
	dc.expire("container-1", entry)
	row, err = database.NewRepository(db).FindByID("public-id")
	if err != nil || row == nil || row.ExpiresAt != nil {
		t.Fatalf("successful retry did not clear expired deadline: row=%+v err=%v", row, err)
	}
	ops, err = dc.repo.Operations("docker")
	if err != nil || len(ops) != 0 {
		t.Fatalf("successful retry retained stop intent=%+v err=%v", ops, err)
	}
}

func TestRecoverRefusesCreateIntentWithWrongNativeOwnershipLabel(t *testing.T) {
	db := database.New(filepath.Join(t.TempDir(), "docker-label.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbSQL.Close() })
	dc, fixture := newDockerFixture(t)
	dc.repo = database.NewRepository(db)
	deadline := time.Now().Add(time.Hour)
	op := database.Operation{ID: "docker:create-attempt", RuntimeKind: "docker", PublicID: "public-id", NativeID: "container-1", NativeName: "expected-name", Token: "expected-token", Kind: "create", Deadline: &deadline}
	if err := db.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.containerLabels = map[string]string{ownerLabel: "foreign-token"}
	fixture.mu.Unlock()
	err = dc.Recover(context.Background())
	if err == nil || !containsError(err, "ownership label mismatch") {
		t.Fatalf("Recover() wrong-label error=%v; want refusal", err)
	}
	ops, err := dc.repo.Operations("docker")
	if err != nil || len(ops) != 1 || ops[0].ID != op.ID {
		t.Fatalf("wrong-label recovery lost durable intent=%+v err=%v", ops, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if containsString(fixture.requests, "DELETE /containers/container-1") {
		t.Fatalf("recovery attempted to delete a resource with a foreign label: %v", fixture.requests)
	}
}

func TestCreatePersistsExactIntentBeforeNativeCreateAndClearsMissingResourceOnRecovery(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	fixture.mu.Lock()
	fixture.fail["POST /containers/create"] = 500
	fixture.mu.Unlock()
	_, err := dc.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: "alpine"})
	if err == nil || !strings.Contains(err.Error(), "create intent") {
		t.Fatalf("Create() failed native request error=%v; want durable intent retained", err)
	}
	ops, err := dc.repo.Operations("docker")
	if err != nil || len(ops) != 1 || ops[0].Kind != "create" || ops[0].NativeID != "" || ops[0].NativeName == "" || ops[0].Token == "" || ops[0].Deadline == nil {
		t.Fatalf("pre-native create intent=%+v err=%v", ops, err)
	}
	if len(ops) != 1 {
		t.Fatalf("missing-resource create intent disappeared before retry: %+v", ops)
	}
	if err := dc.recovery.Run(context.Background(), ops[0].ID); err != nil {
		t.Fatalf("same-process recovery for exact absent create name: %v", err)
	}
	ops, err = dc.repo.Operations("docker")
	if err != nil || len(ops) != 0 {
		t.Fatalf("missing native create retained intent=%+v err=%v", ops, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if containsString(fixture.requests, "DELETE /containers/container-1") {
		t.Fatalf("recovery deleted a resource when create response never produced an ID: %v", fixture.requests)
	}
}

func TestShutdownCancelsQueuedRecoveryAndExpirationTimers(t *testing.T) {
	dc, _ := newDockerFixture(t)
	var workRan atomic.Bool
	dc.recovery.Schedule("pending-intent", func(context.Context) error {
		workRan.Store(true)
		return nil
	})
	deadline := time.Now().Add(time.Hour)
	dc.scheduleDeadline("unowned-test-timer", deadline, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dc.Shutdown(ctx)
	if workRan.Load() {
		t.Fatal("shutdown ran a delayed recovery job")
	}
	if entry := dc.getTimerEntry("unowned-test-timer"); entry != nil {
		t.Fatal("shutdown retained a TTL timer")
	}
	if err := dc.recovery.Run(context.Background(), "pending-intent"); err != nil || workRan.Load() {
		t.Fatalf("stopped queue revived work: err=%v ran=%t", err, workRan.Load())
	}
}

func TestRecoverBindsCreationOwnershipAfterBindingWriteFailure(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	db := database.New(":memory:")
	dc.repo = database.NewRepository(db).NativeView()
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail-create-binding", func(tx *gorm.DB) {
		if tx.Statement.Table == "operations" {
			tx.AddError(errors.New("injected creation binding failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.fail["DELETE /containers/container-1"] = 500
	fixture.mu.Unlock()
	_, err := dc.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: "alpine"})
	if err == nil || !strings.Contains(err.Error(), "injected creation binding failure") || !strings.Contains(err.Error(), "fixture error") {
		t.Fatalf("Create() binding/rollback failure=%v", err)
	}
	ops, err := dc.repo.Operations("docker")
	if err != nil || len(ops) != 1 || ops[0].Kind != "create" || ops[0].NativeID != "" || ops[0].Token == "" {
		t.Fatalf("unbound recovery intent=%+v err=%v", ops, err)
	}
	rows, err := database.NewRepository(db).FindAll()
	if err != nil || len(rows) != 1 || rows[0].NativeID != "container-1" || rows[0].AttemptToken != ops[0].Token {
		t.Fatalf("failed rollback did not retain token-bound owner=%+v err=%v", rows, err)
	}
	if err := db.Callback().Update().Remove("test:fail-create-binding"); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	delete(fixture.fail, "DELETE /containers/container-1")
	fixture.mu.Unlock()
	if err := dc.recovery.Run(context.Background(), ops[0].ID); err != nil {
		t.Fatalf("same-process recovery did not resolve exact owner through attempt token: %v", err)
	}
	rows, err = database.NewRepository(db).FindAll()
	if err != nil || len(rows) != 0 {
		t.Fatalf("reconciled create left owner=%+v err=%v", rows, err)
	}
	ops, err = dc.repo.Operations("docker")
	if err != nil || len(ops) != 0 {
		t.Fatalf("reconciled create left intent=%+v err=%v", ops, err)
	}
}

func TestAdapterCommandWithStaleOwnershipCannotStartAfterRemove(t *testing.T) {
	db := database.New(filepath.Join(t.TempDir(), "stale-adapter.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	dbSQL.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = dbSQL.Close() })
	dc, fixture := newDockerFixture(t)
	dc.repo = database.NewRepository(db).NativeView()
	if err := dc.repo.CreateOwnership(database.Sandbox{ID: "public-id", NativeID: "container-1", RuntimeKind: "docker", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	engine := &blockedExecEngine{Engine: dc, entered: make(chan struct{}), release: make(chan struct{})}
	adapter := runtimeio.New(engine, emptyNativeCache{}, database.NewRepository(db))
	execDone := make(chan error, 1)
	go func() {
		_, err := adapter.ExecCommand(context.Background(), sandbox.SandboxID("public-id"), sandbox.ProcessRequest{Command: "echo"})
		execDone <- err
	}()
	select {
	case <-engine.entered:
	case <-time.After(2 * time.Second):
		close(engine.release)
		t.Fatal("adapter command did not retain its initial ownership snapshot")
	}
	if err := adapter.Remove(context.Background(), sandbox.SandboxID("public-id")); err != nil {
		close(engine.release)
		t.Fatalf("Remove() = %v", err)
	}
	close(engine.release)
	select {
	case err := <-execDone:
		if !errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("stale adapter command error=%v; want not found after ownership removal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale adapter command did not complete after release")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if containsString(fixture.requests, "POST /containers/container-1/exec") {
		t.Fatalf("stale adapter ownership launched native exec after Remove: %v", fixture.requests)
	}
}

func TestDockerCreateIntentSurvivesUntilAdapterProvenanceAdoption(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	db := database.New(":memory:")
	dc.repo = database.NewRepository(db).NativeView()
	publicRepo := database.NewRepository(db)
	adapter := runtimeio.New(dc, emptyNativeCache{}, publicRepo)
	prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
	if err != nil {
		t.Fatal(err)
	}
	const publicID = sandbox.SandboxID("sbx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	provision, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: publicID, Image: prepared, Timeout: time.Minute, Resources: sandbox.ResourceLimits{MemoryMB: 256, CPUs: 1}})
	if err != nil {
		t.Fatalf("Adapter.Create() = %v", err)
	}
	ops, err := publicRepo.Operations("docker")
	if err != nil || len(ops) != 1 || ops[0].Kind != "create" || ops[0].PublicID != string(publicID) || ops[0].NativeID != "container-1" || ops[0].Token == "" {
		t.Fatalf("pre-adoption Docker creation intent=%+v err=%v", ops, err)
	}
	createIntentID := ops[0].ID
	fixture.mu.Lock()
	requestsBeforeAdoptProbe := len(fixture.requests)
	fixture.mu.Unlock()
	if err := dc.recovery.Run(context.Background(), createIntentID); err != nil {
		t.Fatalf("probe successful pending create recovery job: %v", err)
	}
	if row, err := publicRepo.FindByID(string(publicID)); err != nil || row == nil {
		t.Fatalf("successful Docker create was rolled back before Adopt: row=%+v err=%v", row, err)
	}
	fixture.mu.Lock()
	label := fixture.containerLabels[ownerLabel]
	fixture.mu.Unlock()
	if label != ops[0].Token {
		t.Fatalf("native create label=%q does not bind pending token %q", label, ops[0].Token)
	}
	if err := provision.Adopt(context.Background(), sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest", CacheVersion: "docker:test"}); err != nil {
		t.Fatalf("Adopt() = %v", err)
	}
	ops, err = publicRepo.Operations("docker")
	if err != nil || len(ops) != 0 {
		t.Fatalf("successful provenance adoption retained create intent=%+v err=%v", ops, err)
	}
	if err := dc.recovery.Run(context.Background(), createIntentID); err != nil {
		t.Fatalf("completed create recovery probe = %v", err)
	}
	fixture.mu.Lock()
	requestsAfterAdoptProbe := len(fixture.requests)
	fixture.mu.Unlock()
	if requestsAfterAdoptProbe != requestsBeforeAdoptProbe {
		t.Fatalf("completed create had stale recovery work after Adopt: before=%d after=%d", requestsBeforeAdoptProbe, requestsAfterAdoptProbe)
	}
	row, err := publicRepo.FindByID(string(publicID))
	if err != nil || row == nil || row.NativeID != "container-1" || row.ImageRoot != "sha256:root" || row.ImageManifest != "sha256:manifest" {
		t.Fatalf("adopted Docker row=%+v err=%v", row, err)
	}
}

type blockedExecEngine struct {
	runtimeio.Engine
	entered chan struct{}
	release chan struct{}
}

func (e *blockedExecEngine) ExecCommand(ctx context.Context, id string, req runtimeio.ExecCommandRequest) (runtimeio.CommandDetail, error) {
	close(e.entered)
	<-e.release
	return e.Engine.ExecCommand(ctx, id, req)
}

type emptyNativeCache struct{}

func (emptyNativeCache) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{}, nil
}

func (emptyNativeCache) Materialize(context.Context, sandbox.Image) (string, error) {
	return "alpine", nil
}

func entryDeadline(entry *timerEntry) time.Time {
	if entry == nil {
		return time.Time{}
	}
	return entry.expiresAt
}

func sandboxDeadline(row *database.Sandbox) *time.Time {
	if row == nil {
		return nil
	}
	return row.ExpiresAt
}

func containsError(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}
