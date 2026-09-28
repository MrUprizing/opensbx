package runtimeio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/sandbox"
)

type engineCall struct{ method, id string }

type engineFake struct {
	Engine
	repo        *database.Repository
	nativeID    string
	calls       []engineCall
	discardErr  error
	createRow   bool
	createErr   error
	listErr     error
	createPorts []string
	createName  string
	listRows    []SandboxSummary
	inspectRow  *SandboxDetail
}

func (e *engineFake) call(method, id string)     { e.calls = append(e.calls, engineCall{method, id}) }
func (e *engineFake) Ping(context.Context) error { e.call("ping", ""); return nil }
func (e *engineFake) Create(ctx context.Context, req CreateSandboxRequest) (CreateSandboxResponse, error) {
	e.call("create", req.Image)
	if e.createErr != nil {
		return CreateSandboxResponse{}, e.createErr
	}
	publicID := sandbox.CreationID(ctx, e.nativeID)
	if e.createRow {
		root := sandbox.CreationImage(ctx, req.Image)
		row := database.Sandbox{ID: publicID, NativeID: e.nativeID, Name: "fixture", Image: root, NativeImage: req.Image, Ports: database.JSONMap{"3000/tcp": "39001"}, Port: "3000/tcp"}
		if err := e.repo.Save(row); err != nil {
			return CreateSandboxResponse{}, err
		}
	}
	name := e.createName
	if name == "" {
		name = "native"
	}
	ports := req.Ports
	if e.createPorts != nil {
		ports = e.createPorts
	}
	return CreateSandboxResponse{ID: e.nativeID, Name: name, Ports: ports}, nil
}
func (e *engineFake) DiscardCreated(_ context.Context, id string) error {
	e.call("discard", id)
	if e.discardErr != nil {
		return e.discardErr
	}
	return e.repo.NativeView().DeleteSandbox(id)
}
func (e *engineFake) List(context.Context) ([]SandboxSummary, error) {
	e.call("list", "")
	if e.listErr != nil {
		return nil, e.listErr
	}
	if e.listRows != nil {
		return e.listRows, nil
	}
	return []SandboxSummary{{ID: e.nativeID, Name: "native", Status: "running", Ports: []string{"3000/tcp"}}}, nil
}
func (e *engineFake) Inspect(_ context.Context, id string) (SandboxDetail, error) {
	e.call("inspect", id)
	if e.inspectRow != nil {
		return *e.inspectRow, nil
	}
	return SandboxDetail{ID: id, Name: "native", Status: "running", Running: true, Ports: []string{"3000/tcp"}, Resources: ResourceLimits{Memory: 128, CPUs: 1}}, nil
}
func (e *engineFake) Start(_ context.Context, id string) (RestartResponse, error) {
	e.call("start", id)
	return RestartResponse{Status: "started", Ports: []string{"3000/tcp"}}, nil
}
func (e *engineFake) Stop(_ context.Context, id string) error { e.call("stop", id); return nil }
func (e *engineFake) Restart(_ context.Context, id string) (RestartResponse, error) {
	e.call("restart", id)
	return RestartResponse{Status: "restarted"}, nil
}
func (e *engineFake) GetNetwork(_ context.Context, id string) (SandboxNetwork, error) {
	e.call("network", id)
	return SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}, nil
}
func (e *engineFake) Routing(_ context.Context, id string) (RoutingState, error) {
	e.call("routing", id)
	return RoutingState{Running: true, Network: SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}}, nil
}
func (e *engineFake) Remove(_ context.Context, id string) error {
	e.call("remove", id)
	return e.repo.Delete(id)
}
func (e *engineFake) Pause(_ context.Context, id string) error  { e.call("pause", id); return nil }
func (e *engineFake) Resume(_ context.Context, id string) error { e.call("resume", id); return nil }
func (e *engineFake) RenewExpiration(_ context.Context, id string, timeout int) error {
	e.call("renew:"+time.Duration(timeout).String(), id)
	return nil
}
func (e *engineFake) Stats(_ context.Context, id string) (SandboxStats, error) {
	e.call("stats", id)
	return SandboxStats{CPU: 2, Memory: MemoryUsage{Usage: 10, Limit: 20, Percent: 50}, PIDs: 3}, nil
}
func (e *engineFake) ExecCommand(_ context.Context, id string, req ExecCommandRequest) (CommandDetail, error) {
	e.call("exec:"+req.Command, id)
	if err := e.repo.SaveCommand(database.Command{ID: "cmd-1", SandboxID: id, Name: req.Command, StartedAt: 1}); err != nil {
		return CommandDetail{}, err
	}
	return CommandDetail{ID: "cmd-1", SandboxID: id, Name: req.Command, Args: req.Args, Cwd: req.Cwd}, nil
}
func (e *engineFake) GetCommand(_ context.Context, id, cmd string) (CommandDetail, error) {
	e.call("get-command:"+cmd, id)
	return CommandDetail{ID: cmd, SandboxID: id}, nil
}
func (e *engineFake) ListCommands(_ context.Context, id string) ([]CommandDetail, error) {
	e.call("list-commands", id)
	return []CommandDetail{{ID: "cmd-1", SandboxID: id}}, nil
}
func (e *engineFake) KillCommand(_ context.Context, id, cmd string, signal int) (CommandDetail, error) {
	e.call("kill:"+cmd, id)
	return CommandDetail{ID: cmd, SandboxID: id}, nil
}
func (e *engineFake) WaitCommand(_ context.Context, id, cmd string) (CommandDetail, error) {
	e.call("wait:"+cmd, id)
	return CommandDetail{ID: cmd, SandboxID: id}, nil
}
func (e *engineFake) StreamCommandLogs(_ context.Context, id, cmd string) (io.ReadCloser, io.ReadCloser, error) {
	e.call("stream:"+cmd, id)
	return io.NopCloser(&emptyReader{}), io.NopCloser(&emptyReader{}), nil
}
func (e *engineFake) GetCommandLogs(_ context.Context, id, cmd string) (CommandLogsResponse, error) {
	e.call("logs:"+cmd, id)
	return CommandLogsResponse{Stdout: "out", Stderr: "err"}, nil
}
func (e *engineFake) ReadFile(_ context.Context, id, path string) (string, error) {
	e.call("read:"+path, id)
	return "content", nil
}
func (e *engineFake) WriteFile(_ context.Context, id, path, content string) error {
	e.call("write:"+path, id)
	return nil
}
func (e *engineFake) DeleteFile(_ context.Context, id, path string) error {
	e.call("delete:"+path, id)
	return nil
}
func (e *engineFake) ListDir(_ context.Context, id, path string) (string, error) {
	e.call("list-dir:"+path, id)
	return "entry", nil
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

type nativeCacheFake struct {
	caps             sandbox.Capabilities
	ref              string
	err              error
	materializeCalls int
}

func (c *nativeCacheFake) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return c.caps, nil
}
func (c *nativeCacheFake) Materialize(context.Context, sandbox.Image) (string, error) {
	c.materializeCalls++
	if c.err != nil {
		return "", c.err
	}
	return c.ref, nil
}

func newAdapterFixture(t *testing.T) (*Adapter, *engineFake, *nativeCacheFake, *database.Repository) {
	t.Helper()
	db := database.New(filepath.Join(t.TempDir(), "runtimeio.sqlite"))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	engine := &engineFake{repo: repo.NativeView(), nativeID: "native-container-9", createRow: true}
	cache := &nativeCacheFake{caps: sandbox.Capabilities{Runtime: "fixture", Version: "v1", Platform: sandbox.Platform{OS: "linux", Architecture: "amd64"}}, ref: "cache://manifest"}
	return New(engine, cache, repo), engine, cache, repo
}

func createAdopted(t *testing.T, adapter *Adapter, id sandbox.SandboxID) sandbox.Created {
	t.Helper()
	ctx := context.Background()
	prepared, err := adapter.Materialize(ctx, sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest", ConfigDigest: "sha256:config", Platform: sandbox.Platform{OS: "linux", Architecture: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := adapter.Create(ctx, sandbox.RunOptions{ID: id, Image: prepared, Ports: []sandbox.Port{{Number: 3000, Protocol: "tcp"}}, Timeout: time.Minute, Resources: sandbox.ResourceLimits{MemoryMB: 256, CPUs: 2}})
	if err != nil {
		t.Fatal(err)
	}
	identity := sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest", CacheVersion: "fixture:v1"}
	if err := transaction.Adopt(ctx, identity); err != nil {
		t.Fatal(err)
	}
	return transaction.Sandbox()
}

func TestAdapterProvisionAdoptionAndRollbackKeepPublicIdentityAndNativeHandleSeparate(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	ctx := context.Background()
	const id = sandbox.SandboxID("sbx-0123456789abcdef0123456789abcdef")
	prepared, err := adapter.Materialize(ctx, sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest", ConfigDigest: "sha256:config", Platform: sandbox.Platform{OS: "linux", Architecture: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.Create(ctx, sandbox.RunOptions{ID: id, Image: prepared, Ports: []sandbox.Port{{Number: 3000, Protocol: "tcp"}}, Timeout: time.Minute, Resources: sandbox.ResourceLimits{MemoryMB: 256, CPUs: 2}})
	if err != nil {
		t.Fatal(err)
	}
	provision := created
	if provision.Sandbox().ID != id {
		t.Fatalf("provisioned public ID=%q", provision.Sandbox().ID)
	}
	if err := provision.Adopt(ctx, sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest", CacheVersion: "fixture:v1"}); err != nil {
		t.Fatal(err)
	}
	row, err := repo.FindByID(string(id))
	if err != nil || row == nil {
		t.Fatalf("adopted row=%+v err=%v", row, err)
	}
	if row.ID != string(id) || row.NativeID != engine.nativeID || row.ImageRoot != "sha256:root" || row.ImageManifest != "sha256:manifest" || row.NativeImage != "cache://manifest" || row.CacheVersion != "fixture:v1" {
		t.Fatalf("adoption identity/provenance=%+v", row)
	}
	if err := repo.SaveCommand(database.Command{ID: "cmd-owned", SandboxID: string(id), Name: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := provision.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if len(engine.calls) == 0 || engine.calls[len(engine.calls)-1] != (engineCall{"discard", engine.nativeID}) {
		t.Fatalf("rollback discarded wrong native resource: %+v", engine.calls)
	}
	if row, err := repo.FindByID(string(id)); err != nil || row != nil {
		t.Fatalf("rollback left ownership row=%+v err=%v", row, err)
	}
	cmds, err := repo.FindCommandsBySandbox(string(id))
	if err != nil || len(cmds) != 0 {
		t.Fatalf("rollback left public command rows=%+v err=%v", cmds, err)
	}
}

func TestAdapterRecoveryPersistsProvisionedOwnershipWhenDiscardFails(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	engine.discardErr = errors.New("native delete failed")
	ctx := context.Background()
	id := sandbox.SandboxID("sbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	prepared, err := adapter.Materialize(ctx, sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest", ConfigDigest: "sha256:config", Platform: sandbox.Platform{OS: "linux", Architecture: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	provision, err := adapter.Create(ctx, sandbox.RunOptions{ID: id, Image: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if err := provision.Adopt(ctx, sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest", CacheVersion: "fixture:v1"}); err != nil {
		t.Fatal(err)
	}
	rollbackErr := provision.Rollback(ctx)
	if !errors.Is(rollbackErr, engine.discardErr) {
		t.Fatalf("rollback error=%v", rollbackErr)
	}
	identity := sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest", CacheVersion: "fixture:v1"}
	if err := provision.Recover(ctx, identity, rollbackErr); err != nil {
		t.Fatalf("persist recovery ownership: %v", err)
	}
	recovery, err := repo.FindByID(string(id))
	if err != nil || recovery == nil || recovery.NativeID != engine.nativeID || recovery.ImageRoot != identity.Root || recovery.ImageManifest != identity.Manifest || recovery.NativeImage != "cache://manifest" || recovery.CacheVersion != identity.CacheVersion || recovery.RecoveryError == "" {
		t.Fatalf("recovery row=%+v err=%v", recovery, err)
	}
}

func TestAdapterTranslatesTypedDomainOperationsToNativeReferences(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	publicID := sandbox.SandboxID("sbx-11111111111111111111111111111111")
	created := createAdopted(t, adapter, publicID)
	if created.ID != publicID || created.Name != "native" || created.Ports[0] != (sandbox.Port{Number: 3000, Protocol: "tcp"}) {
		t.Fatalf("created sandbox=%+v", created)
	}
	items, err := adapter.List(context.Background())
	if err != nil || len(items) != 1 || items[0].ID != publicID || items[0].Image != "sha256:root" {
		t.Fatalf("typed summary list=%+v err=%v", items, err)
	}
	detail, err := adapter.Inspect(context.Background(), publicID)
	if err != nil || detail.ID != publicID || detail.Resources.MemoryMB != 128 || !detail.Running {
		t.Fatalf("typed inspect=%+v err=%v", detail, err)
	}
	started, err := adapter.Start(context.Background(), publicID)
	if err != nil || started.Status != "started" || len(started.Ports) != 1 {
		t.Fatalf("typed start=%+v err=%v", started, err)
	}
	if _, err := adapter.Restart(context.Background(), publicID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stop(context.Background(), publicID); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.GetNetwork(context.Background(), publicID); err != nil {
		t.Fatal(err)
	}
	route, err := adapter.Routing(context.Background(), publicID)
	if err != nil || !route.Running || route.Network.Main != (sandbox.Port{Number: 3000, Protocol: "tcp"}) || route.Network.Ports[0].Host != 39001 {
		t.Fatalf("typed routing snapshot=%+v err=%v", route, err)
	}
	if err := adapter.RenewExpiration(context.Background(), publicID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Stats(context.Background(), publicID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Pause(context.Background(), publicID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Resume(context.Background(), publicID); err != nil {
		t.Fatal(err)
	}
	command, err := adapter.ExecCommand(context.Background(), publicID, sandbox.ProcessRequest{Command: "echo", Args: []string{"domain"}, Cwd: "/tmp"})
	if err != nil || command.SandboxID != publicID || command.ID != "cmd-1" {
		t.Fatalf("typed command=%+v err=%v", command, err)
	}
	if got, err := adapter.GetCommand(context.Background(), publicID, command.ID); err != nil || got.SandboxID != publicID {
		t.Fatalf("typed GetCommand=%+v err=%v", got, err)
	}
	if got, err := adapter.ListCommands(context.Background(), publicID); err != nil || len(got) != 1 || got[0].SandboxID != publicID {
		t.Fatalf("typed ListCommands=%+v err=%v", got, err)
	}
	if got, err := adapter.KillCommand(context.Background(), publicID, command.ID, 15); err != nil || got.SandboxID != publicID {
		t.Fatalf("typed KillCommand=%+v err=%v", got, err)
	}
	if got, err := adapter.WaitCommand(context.Background(), publicID, command.ID); err != nil || got.SandboxID != publicID {
		t.Fatalf("typed WaitCommand=%+v err=%v", got, err)
	}
	if logs, err := adapter.GetCommandLogs(context.Background(), publicID, command.ID); err != nil || logs.Stdout != "out" {
		t.Fatalf("typed logs=%+v err=%v", logs, err)
	}
	stdout, stderr, err := adapter.StreamCommandLogs(context.Background(), publicID, command.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = stdout.Close()
	_ = stderr.Close()
	if body, err := adapter.ReadFile(context.Background(), publicID, "/tmp/item"); err != nil || body != "content" {
		t.Fatalf("typed ReadFile=%q err=%v", body, err)
	}
	if err := adapter.WriteFile(context.Background(), publicID, "/tmp/item", "content"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DeleteFile(context.Background(), publicID, "/tmp/item"); err != nil {
		t.Fatal(err)
	}
	if name, err := adapter.ListDir(context.Background(), publicID, "/tmp"); err != nil || name != "entry" {
		t.Fatalf("typed ListDir=%q err=%v", name, err)
	}
	for _, call := range engine.calls {
		if call.id != "" && call.method != "create" && call.method != "list" && call.id != engine.nativeID {
			t.Errorf("native engine received non-native ID in %s: %q", call.method, call.id)
		}
	}
	if err := adapter.Remove(context.Background(), publicID); err != nil {
		t.Fatal(err)
	}
	if row, err := repo.FindByID(string(publicID)); err != nil || row != nil {
		t.Fatalf("native remove left row=%+v err=%v", row, err)
	}
}

func TestAdapterRejectsUnpreparedForeignImageAndInvalidRunIdentityBeforeEngineCreate(t *testing.T) {
	adapter, engine, _, _ := newAdapterFixture(t)
	if _, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: "sbx-empty-image"}); err == nil {
		t.Fatal("adapter accepted a missing prepared-image handle")
	}
	foreignAdapter, _, _, _ := newAdapterFixture(t)
	foreignPrepared, err := foreignAdapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:foreign", ManifestDigest: "sha256:foreign-manifest", Platform: sandbox.Platform{OS: "linux", Architecture: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: "sbx-foreign", Image: foreignPrepared}); err == nil {
		t.Fatal("adapter accepted another runtime's prepared image")
	}
	prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest", Platform: sandbox.Platform{OS: "linux", Architecture: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Create(context.Background(), sandbox.RunOptions{Image: prepared}); !errors.Is(err, sandbox.ErrInvalidInput) {
		t.Fatalf("missing public identity error=%v", err)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("invalid runs reached native engine: %+v", engine.calls)
	}
}

func TestAdapterValidatesPreparedContentBeforeNativeCacheAndPropagatesCacheProbeErrors(t *testing.T) {
	adapter, engine, cache, _ := newAdapterFixture(t)
	validationErr := errors.New("OCI content validation failed")
	_, err := adapter.Materialize(context.Background(), sandbox.Image{Validate: func(context.Context) error { return validationErr }})
	if !errors.Is(err, validationErr) {
		t.Fatalf("content validation error=%v", err)
	}
	if cache.materializeCalls != 0 {
		t.Fatalf("native cache touched invalid OCI content %d time(s)", cache.materializeCalls)
	}
	cache.err = errors.New("native cache import failed")
	_, err = adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
	if !errors.Is(err, cache.err) || cache.materializeCalls != 1 {
		t.Fatalf("native cache failure=%v calls=%d", err, cache.materializeCalls)
	}
	cache.err = nil
	prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
	if err != nil {
		t.Fatal(err)
	}
	if identity := prepared.Identity(); identity.Root != "sha256:root" || identity.Manifest != "sha256:manifest" {
		t.Fatalf("opaque prepared identity=%+v", identity)
	}
	if err := adapter.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if caps, err := adapter.Capabilities(context.Background()); err != nil || caps.Runtime != "fixture" {
		t.Fatalf("capabilities=%+v err=%v", caps, err)
	}
	if len(engine.calls) != 1 || engine.calls[0].method != "ping" {
		t.Fatalf("cache/validation operation contacted execution engine: %+v", engine.calls)
	}
}

func TestAdapterDoesNotAcceptNativeIDAsPublicLookupAlias(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	if err := repo.Save(database.Sandbox{ID: "sbx-public", NativeID: engine.nativeID, Name: "owned"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []sandbox.SandboxID{sandbox.SandboxID(engine.nativeID), sandbox.SandboxID("unknown-public")} {
		if _, err := adapter.Inspect(context.Background(), id); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("Inspect(%q) error=%v want not found", id, err)
		}
		if _, err := adapter.ReadFile(context.Background(), id, "/tmp/x"); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("ReadFile(%q) error=%v want not found", id, err)
		}
	}
	if len(engine.calls) != 0 {
		t.Fatalf("native alias/unknown public IDs reached backend: %+v", engine.calls)
	}
}

func TestProvisionedAdoptionRefusesMissingOrNativeMismatchOwnershipRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove bool
	}{
		{name: "row disappeared", remove: true},
		{name: "native reference differs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, _, _, repo := newAdapterFixture(t)
			id := sandbox.SandboxID("sbx-adoption-check")
			prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
			if err != nil {
				t.Fatal(err)
			}
			transaction, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: id, Image: prepared})
			if err != nil {
				t.Fatal(err)
			}
			if tc.remove {
				if err := repo.Delete(string(id)); err != nil {
					t.Fatal(err)
				}
			} else if err := repo.Save(database.Sandbox{ID: string(id), NativeID: "native-other", Name: "other"}); err != nil {
				t.Fatal(err)
			}
			if err := transaction.Adopt(context.Background(), sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest"}); err == nil {
				t.Fatal("provision was adopted without its matching ownership row")
			}
		})
	}
}

func TestProvisionedAdoptionChecksContextAndImageIdentityAndRecoveryDoesNotResurrectDeletedOwner(t *testing.T) {
	adapter, _, _, repo := newAdapterFixture(t)
	const publicID = sandbox.SandboxID("sbx-provision-recovery")
	prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: publicID, Image: prepared})
	if err != nil {
		t.Fatal(err)
	}
	identity := sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest", CacheVersion: "fixture:v1"}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := transaction.Adopt(canceled, identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled adoption error=%v", err)
	}
	if err := transaction.Adopt(context.Background(), sandbox.Provenance{Root: "sha256:other", Manifest: identity.Manifest}); err == nil || !strings.Contains(err.Error(), "image identity mismatch") {
		t.Fatalf("mismatched image adoption error=%v", err)
	}
	if err := repo.Delete(string(publicID)); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Recover(context.Background(), identity, errors.New("recovery test cause")); err == nil || !strings.Contains(err.Error(), "ownership disappeared") {
		t.Fatalf("recovery without current ownership error=%v; want refusal to resurrect deleted row", err)
	}
	row, err := repo.FindByID(string(publicID))
	if err != nil || row != nil {
		t.Fatalf("deleted owner was resurrected by stale recovery: row=%+v err=%v", row, err)
	}
}

type operationErrorEngine struct {
	Engine
	err error
}

func (e operationErrorEngine) Inspect(context.Context, string) (SandboxDetail, error) {
	return SandboxDetail{}, e.err
}
func (e operationErrorEngine) GetNetwork(context.Context, string) (SandboxNetwork, error) {
	return SandboxNetwork{}, e.err
}
func (e operationErrorEngine) Routing(context.Context, string) (RoutingState, error) {
	return RoutingState{}, e.err
}

func TestAdapterPropagatesNativeOperationErrorsWithoutChangingTheirIdentity(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	const id = sandbox.SandboxID("sbx-operation-errors")
	if err := repo.Save(database.Sandbox{ID: string(id), NativeID: engine.nativeID, Name: "fixture"}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("native operation failed")
	adapter.engine = operationErrorEngine{Engine: engine, err: want}
	checks := []struct {
		name string
		call func() error
	}{
		{"inspect", func() error { _, err := adapter.Inspect(context.Background(), id); return err }},
		{"network", func() error { _, err := adapter.GetNetwork(context.Background(), id); return err }},
		{"routing", func() error { _, err := adapter.Routing(context.Background(), id); return err }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, want) {
				t.Fatalf("native error=%v, want original error", err)
			}
		})
	}
	if _, err := started(RestartResponse{}, want); !errors.Is(err, want) {
		t.Fatalf("start response conversion error=%v", err)
	}
	if _, err := command(CommandDetail{}, id, want); !errors.Is(err, want) {
		t.Fatalf("command response conversion error=%v", err)
	}
}

func TestAdapterAndProvisioningPropagateRepositoryFailures(t *testing.T) {
	db := database.New(":memory:")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	repo := database.NewRepository(db)
	engine := &engineFake{repo: repo.NativeView(), nativeID: "native-repository-error", createRow: true}
	cache := &nativeCacheFake{ref: "cache://manifest"}
	adapter := New(engine, cache, repo)
	prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
	if err != nil {
		t.Fatal(err)
	}
	id := sandbox.SandboxID("sbx-repository-error")
	transaction, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: id, Image: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		call func() error
	}{
		{"native ownership lookup", func() error { _, err := adapter.native(id); return err }},
		{"create ownership lookup", func() error {
			_, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: "sbx-second", Image: prepared})
			return err
		}},
		{"list ownership lookup", func() error { _, err := adapter.List(context.Background()); return err }},
		{"lifecycle ownership lookup", func() error { return adapter.RenewExpiration(context.Background(), id, time.Minute) }},
		{"provision adoption lookup", func() error {
			return transaction.Adopt(context.Background(), sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest"})
		}},
		{"provision recovery lookup", func() error {
			return transaction.Recover(context.Background(), sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:manifest"}, errors.New("recovery cause"))
		}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("repository failure was suppressed")
			}
		})
	}
}

func TestAdapterRejectsNonpositiveOrFractionalRenewalWithoutBackendCall(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	const id = sandbox.SandboxID("sbx-invalid-renewal")
	if err := repo.Save(database.Sandbox{ID: string(id), NativeID: engine.nativeID, Name: "fixture"}); err != nil {
		t.Fatal(err)
	}
	for _, timeout := range []time.Duration{0, -time.Second, 1500 * time.Millisecond} {
		if err := adapter.RenewExpiration(context.Background(), id, timeout); !errors.Is(err, sandbox.ErrInvalidInput) {
			t.Errorf("RenewExpiration(%s) error=%v, want invalid input", timeout, err)
		}
	}
	if len(engine.calls) != 0 {
		t.Fatalf("invalid renewal durations reached native engine: %+v", engine.calls)
	}
}

func TestAdapterCreatePropagatesNativeErrorsAndRejectsDuplicatePublicIdentity(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
	if err != nil {
		t.Fatal(err)
	}
	engine.createErr = errors.New("native create rejected")
	if _, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: "sbx-native-failure", Image: prepared}); !errors.Is(err, engine.createErr) {
		t.Fatalf("native Create error=%v", err)
	}
	if err := repo.Save(database.Sandbox{ID: "sbx-duplicate", NativeID: "native-old", Name: "existing"}); err != nil {
		t.Fatal(err)
	}
	engine.createErr = nil
	if _, err := adapter.Create(context.Background(), sandbox.RunOptions{ID: "sbx-duplicate", Image: prepared}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate public ID error=%v", err)
	}
	creates := 0
	for _, call := range engine.calls {
		if call.method == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("invalid/duplicate provisioning reached backend %d time(s): %+v", creates, engine.calls)
	}
}

func TestAdapterRollsBackInvalidNativePortResponseAndRecordsRecoveryIfDiscardFails(t *testing.T) {
	for _, failDiscard := range []bool{false, true} {
		t.Run(fmt.Sprintf("discard-fails-%v", failDiscard), func(t *testing.T) {
			adapter, engine, _, repo := newAdapterFixture(t)
			engine.createPorts = []string{"70000/tcp"}
			if failDiscard {
				engine.discardErr = errors.New("discard failed")
			}
			prepared, err := adapter.Materialize(context.Background(), sandbox.Image{RootDigest: "sha256:root", ManifestDigest: "sha256:manifest"})
			if err != nil {
				t.Fatal(err)
			}
			id := sandbox.SandboxID("sbx-invalid-native-ports")
			_, err = adapter.Create(context.Background(), sandbox.RunOptions{ID: id, Image: prepared})
			if err == nil {
				t.Fatal("invalid runtime-provided port was accepted")
			}
			if len(engine.calls) == 0 || engine.calls[len(engine.calls)-1].method != "discard" || engine.calls[len(engine.calls)-1].id != engine.nativeID {
				t.Fatalf("invalid creation did not compensate exact resource: %+v", engine.calls)
			}
			row, findErr := repo.FindByID(string(id))
			if failDiscard {
				if findErr != nil || row == nil || row.NativeID != engine.nativeID || row.RecoveryError == "" {
					t.Fatalf("failed discard did not preserve recovery record: row=%+v err=%v", row, findErr)
				}
			} else if findErr != nil || row != nil {
				t.Fatalf("successful rollback left row=%+v err=%v", row, findErr)
			}
		})
	}
}

func TestAdapterMapsListAndInspectOwnershipAndRejectsMalformedNativeSnapshots(t *testing.T) {
	adapter, engine, _, repo := newAdapterFixture(t)
	const public = sandbox.SandboxID("sbx-snapshot")
	if err := repo.Save(database.Sandbox{ID: string(public), NativeID: engine.nativeID, Name: "fixture", ImageRoot: "sha256:root", Port: "3000/tcp"}); err != nil {
		t.Fatal(err)
	}
	engine.listRows = []SandboxSummary{{ID: engine.nativeID, Name: "native", Ports: []string{"3000/tcp"}}}
	listed, err := adapter.List(context.Background())
	if err != nil || len(listed) != 1 || listed[0].ID != public || listed[0].Image != "sha256:root" {
		t.Fatalf("typed list=%+v err=%v", listed, err)
	}
	listErr := errors.New("native inventory unavailable")
	engine.listErr = listErr
	if _, err := adapter.List(context.Background()); !errors.Is(err, listErr) {
		t.Fatalf("native list failure=%v, want original error", err)
	}
	engine.listErr = nil
	engine.inspectRow = &SandboxDetail{ID: engine.nativeID, Status: "running", Running: true, Ports: []string{"bad"}}
	if _, err := adapter.Inspect(context.Background(), public); err == nil {
		t.Fatal("malformed native inspect port accepted")
	}
	engine.inspectRow = nil
	engine.listRows = []SandboxSummary{{ID: "not-owned-native"}}
	if _, err := adapter.List(context.Background()); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("runtime inventory without public ownership error=%v", err)
	}
}

func TestEveryAdapterOperationRejectsUnknownPublicIdentityBeforeNativeEngineCall(t *testing.T) {
	adapter, engine, _, _ := newAdapterFixture(t)
	ctx := context.Background()
	id := sandbox.SandboxID("native-alias-not-public")
	checks := []struct {
		name string
		call func() error
	}{
		{"inspect", func() error { _, err := adapter.Inspect(ctx, id); return err }},
		{"start", func() error { _, err := adapter.Start(ctx, id); return err }},
		{"restart", func() error { _, err := adapter.Restart(ctx, id); return err }},
		{"stop", func() error { return adapter.Stop(ctx, id) }},
		{"remove", func() error { return adapter.Remove(ctx, id) }},
		{"pause", func() error { return adapter.Pause(ctx, id) }},
		{"resume", func() error { return adapter.Resume(ctx, id) }},
		{"renew", func() error { return adapter.RenewExpiration(ctx, id, time.Minute) }},
		{"network", func() error { _, err := adapter.GetNetwork(ctx, id); return err }},
		{"route", func() error { _, err := adapter.Routing(ctx, id); return err }},
		{"stats", func() error { _, err := adapter.Stats(ctx, id); return err }},
		{"exec", func() error {
			_, err := adapter.ExecCommand(ctx, id, sandbox.ProcessRequest{Command: "echo"})
			return err
		}},
		{"get command", func() error { _, err := adapter.GetCommand(ctx, id, "cmd"); return err }},
		{"list commands", func() error { _, err := adapter.ListCommands(ctx, id); return err }},
		{"kill", func() error { _, err := adapter.KillCommand(ctx, id, "cmd", 15); return err }},
		{"wait", func() error { _, err := adapter.WaitCommand(ctx, id, "cmd"); return err }},
		{"logs", func() error { _, err := adapter.GetCommandLogs(ctx, id, "cmd"); return err }},
		{"stream", func() error { _, _, err := adapter.StreamCommandLogs(ctx, id, "cmd"); return err }},
		{"read file", func() error { _, err := adapter.ReadFile(ctx, id, "/tmp/x"); return err }},
		{"write file", func() error { return adapter.WriteFile(ctx, id, "/tmp/x", "x") }},
		{"delete file", func() error { return adapter.DeleteFile(ctx, id, "/tmp/x") }},
		{"list dir", func() error { _, err := adapter.ListDir(ctx, id, "/tmp"); return err }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, sandbox.ErrNotFound) {
				t.Fatalf("error=%v want public-ID not found", err)
			}
		})
	}
	if len(engine.calls) != 0 {
		t.Fatalf("unknown public ID reached native engine: %+v", engine.calls)
	}
}

func TestNativeNetworkConversionRejectsMalformedGuestAndHostPorts(t *testing.T) {
	for _, item := range []SandboxNetwork{
		{MainPort: "0/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}},
		{MainPort: "3000/tcp", PortsMap: map[string]string{"bad": "39001"}},
		{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "70000"}},
		{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "0"}},
	} {
		if _, err := network(item); err == nil {
			t.Errorf("invalid native network map accepted: %+v", item)
		}
	}
	if _, err := started(RestartResponse{Ports: []string{"3000/tcp"}}, nil); err != nil {
		t.Fatalf("valid started ports rejected: %v", err)
	}
	if _, err := started(RestartResponse{Ports: []string{"3000/icmp"}}, nil); err == nil {
		t.Fatal("invalid native started port accepted")
	}
}

var _ Engine = (*engineFake)(nil)
var _ NativeCache = (*nativeCacheFake)(nil)
