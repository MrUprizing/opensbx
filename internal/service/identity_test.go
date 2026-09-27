package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"opensbx/internal/database"
	"opensbx/internal/images"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

type cacheFake struct {
	caps             sandbox.Capabilities
	materializations int
	image            sandbox.Image
	capsErr          error
	err              error
}

func (c *cacheFake) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return c.caps, c.capsErr
}
func (c *cacheFake) Materialize(_ context.Context, image sandbox.Image) (string, error) {
	c.materializations++
	c.image = image
	if c.err != nil {
		return "", c.err
	}
	return "native-cache:" + image.ManifestDigest, nil
}

type runtimeCall struct{ op, id string }
type runtimeEngineFake struct {
	repo            *database.Repository
	calls           []runtimeCall
	nativeID        string
	inspectErr      error
	networkErr      error
	listErr         error
	running         *bool
	networkValue    *runtimeio.SandboxNetwork
	routeValue      *runtimeio.RoutingState
	routingErr      error
	routingBlock    <-chan struct{}
	routingEntered  chan struct{}
	routingOnce     sync.Once
	listID          string
	skipCreateRow   bool
	savedNativeID   string
	resultNativeID  string
	createdPublicID string
	removedIDs      []string
	removeErr       error
	createErr       error
}

func (r *runtimeEngineFake) record(op, id string)       { r.calls = append(r.calls, runtimeCall{op, id}) }
func (r *runtimeEngineFake) Ping(context.Context) error { r.record("ping", ""); return nil }
func (r *runtimeEngineFake) Create(ctx context.Context, req runtimeio.CreateSandboxRequest) (runtimeio.CreateSandboxResponse, error) {
	publicID := sandbox.CreationID(ctx, r.nativeID)
	root := sandbox.CreationImage(ctx, req.Image)
	r.createdPublicID = publicID
	if r.createErr != nil {
		r.record("create", req.Image)
		return runtimeio.CreateSandboxResponse{}, r.createErr
	}
	if !r.skipCreateRow {
		nativeID := r.nativeID
		if r.savedNativeID != "" {
			nativeID = r.savedNativeID
		}
		row := database.Sandbox{ID: publicID, NativeID: nativeID, Name: "fixture", Image: root, ImageRoot: root, NativeImage: req.Image, Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "39001"}}
		if err := r.repo.CreateOwnership(row); err != nil {
			return runtimeio.CreateSandboxResponse{}, err
		}
	}
	r.record("create", req.Image)
	resultID := r.nativeID
	if r.resultNativeID != "" {
		resultID = r.resultNativeID
	}
	return runtimeio.CreateSandboxResponse{ID: resultID, Name: "native-name", Ports: []string{"3000/tcp"}}, nil
}
func (r *runtimeEngineFake) DiscardCreated(_ context.Context, id string) error {
	r.record("discard-created", id)
	r.removedIDs = append(r.removedIDs, id)
	return r.removeErr
}
func (r *runtimeEngineFake) List(context.Context) ([]runtimeio.SandboxSummary, error) {
	r.record("list", "")
	if r.listErr != nil {
		return nil, r.listErr
	}
	id := r.nativeID
	if r.listID != "" {
		id = r.listID
	}
	return []runtimeio.SandboxSummary{{ID: id, Name: "runtime-name", Status: "running"}}, nil
}
func (r *runtimeEngineFake) Inspect(_ context.Context, id string) (runtimeio.SandboxDetail, error) {
	r.record("inspect", id)
	if r.inspectErr != nil {
		return runtimeio.SandboxDetail{}, r.inspectErr
	}
	running := true
	if r.running != nil {
		running = *r.running
	}
	return runtimeio.SandboxDetail{ID: id, Name: "runtime-name", Running: running, Status: "running"}, nil
}
func (r *runtimeEngineFake) Start(_ context.Context, id string) (runtimeio.RestartResponse, error) {
	r.record("start", id)
	return runtimeio.RestartResponse{Status: "started"}, nil
}
func (r *runtimeEngineFake) Stop(_ context.Context, id string) error {
	r.record("stop", id)
	return nil
}
func (r *runtimeEngineFake) Restart(_ context.Context, id string) (runtimeio.RestartResponse, error) {
	r.record("restart", id)
	return runtimeio.RestartResponse{Status: "restarted"}, nil
}
func (r *runtimeEngineFake) GetNetwork(_ context.Context, id string) (runtimeio.SandboxNetwork, error) {
	r.record("network", id)
	if r.networkErr != nil {
		return runtimeio.SandboxNetwork{}, r.networkErr
	}
	if r.networkValue != nil {
		return *r.networkValue, nil
	}
	return runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}, nil
}
func (r *runtimeEngineFake) Routing(_ context.Context, id string) (runtimeio.RoutingState, error) {
	r.record("routing", id)
	if r.routingEntered != nil {
		r.routingOnce.Do(func() { close(r.routingEntered) })
	}
	if r.routingBlock != nil {
		<-r.routingBlock
	}
	if r.routingErr != nil {
		return runtimeio.RoutingState{}, r.routingErr
	}
	if r.routeValue != nil {
		return *r.routeValue, nil
	}
	running := true
	if r.running != nil {
		running = *r.running
	}
	return runtimeio.RoutingState{Running: running, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}}, nil
}
func (r *runtimeEngineFake) Remove(_ context.Context, id string) error {
	r.record("remove", id)
	return r.repo.Delete(id)
}
func (r *runtimeEngineFake) Pause(_ context.Context, id string) error {
	r.record("pause", id)
	return nil
}
func (r *runtimeEngineFake) Resume(_ context.Context, id string) error {
	r.record("resume", id)
	return nil
}
func (r *runtimeEngineFake) RenewExpiration(_ context.Context, id string, _ int) error {
	r.record("renew", id)
	return nil
}
func (r *runtimeEngineFake) Stats(_ context.Context, id string) (runtimeio.SandboxStats, error) {
	r.record("stats", id)
	return runtimeio.SandboxStats{}, nil
}
func (r *runtimeEngineFake) ExecCommand(_ context.Context, id string, req runtimeio.ExecCommandRequest) (runtimeio.CommandDetail, error) {
	r.record("exec", id)
	if err := r.repo.SaveCommand(database.Command{ID: "cmd-1", SandboxID: id, Name: req.Command, StartedAt: 100}); err != nil {
		return runtimeio.CommandDetail{}, err
	}
	return runtimeio.CommandDetail{ID: "cmd-1", SandboxID: id, Name: req.Command}, nil
}
func (r *runtimeEngineFake) GetCommand(_ context.Context, id, cmd string) (runtimeio.CommandDetail, error) {
	r.record("get-command", id)
	return runtimeio.CommandDetail{ID: cmd, SandboxID: id}, nil
}
func (r *runtimeEngineFake) ListCommands(_ context.Context, id string) ([]runtimeio.CommandDetail, error) {
	r.record("list-commands", id)
	return []runtimeio.CommandDetail{{ID: "cmd-1", SandboxID: id}}, nil
}
func (r *runtimeEngineFake) KillCommand(_ context.Context, id, cmd string, _ int) (runtimeio.CommandDetail, error) {
	r.record("kill", id)
	return runtimeio.CommandDetail{ID: cmd, SandboxID: id}, nil
}
func (r *runtimeEngineFake) StreamCommandLogs(_ context.Context, id, _ string) (io.ReadCloser, io.ReadCloser, error) {
	r.record("stream", id)
	return io.NopCloser(strings.NewReader("out")), io.NopCloser(strings.NewReader("err")), nil
}
func (r *runtimeEngineFake) GetCommandLogs(_ context.Context, id, _ string) (runtimeio.CommandLogsResponse, error) {
	r.record("logs", id)
	return runtimeio.CommandLogsResponse{Stdout: "out"}, nil
}
func (r *runtimeEngineFake) WaitCommand(_ context.Context, id, cmd string) (runtimeio.CommandDetail, error) {
	r.record("wait", id)
	return runtimeio.CommandDetail{ID: cmd, SandboxID: id}, nil
}
func (r *runtimeEngineFake) ReadFile(_ context.Context, id, path string) (string, error) {
	r.record("read", id)
	return path, nil
}
func (r *runtimeEngineFake) WriteFile(_ context.Context, id, _, _ string) error {
	r.record("write", id)
	return nil
}
func (r *runtimeEngineFake) DeleteFile(_ context.Context, id, _ string) error {
	r.record("delete-file", id)
	return nil
}
func (r *runtimeEngineFake) ListDir(_ context.Context, id, path string) (string, error) {
	r.record("list-dir", id)
	return path, nil
}

func openPreparedService(t *testing.T, caps sandbox.Capabilities) (*Service, *runtimeEngineFake, *cacheFake, *database.Repository, *images.Store, v1.Platform, string) {
	t.Helper()
	archive, platform := testsupport.OCIArchive(t)
	caps.Platform = sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant}
	dbPath := filepath.Join(t.TempDir(), "execution.db")
	db := database.New(dbPath)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(context.Background(), archive, "example.test/team/app:stable", platform); err != nil {
		t.Fatal(err)
	}
	cache := &cacheFake{caps: caps}
	runtime := &runtimeEngineFake{repo: repo.NativeView(), nativeID: "native-container-7"}
	adapter := runtimeio.New(runtime, cache, repo)
	app, err := New(context.Background(), adapter, adapter, store, repo)
	if err != nil {
		t.Fatal(err)
	}
	app.SetAddress(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 43771})
	return app, runtime, cache, repo, store, platform, dbPath
}

func createOptions(image string) sandbox.CreateOptions { return sandbox.CreateOptions{Image: image} }

func TestCreateSeparatesPublicAndNativeIdentityAndTranslatesRuntimeOperations(t *testing.T) {
	app, runtime, cache, repo, store, platform, dbPath := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1", FractionalCPU: true, Pause: true})
	ctx := context.Background()
	root, err := store.Resolve(ctx, "example.test/team/app:stable", platform)
	if err != nil {
		t.Fatal(err)
	}
	imageList, err := app.ListImages(ctx)
	if err != nil || len(imageList) != 1 || string(imageList[0].ID) != root.Root.Digest.String() {
		t.Fatalf("OpenSBX image catalog list=%+v err=%v", imageList, err)
	}
	imageDetail, err := app.InspectImage(ctx, "example.test/team/app:stable")
	if err != nil || string(imageDetail.ID) != root.Root.Digest.String() {
		t.Fatalf("OpenSBX image inspect=%+v err=%v", imageDetail, err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("image catalog accessed execution runtime: %+v", runtime.calls)
	}
	options := createOptions("example.test/team/app:stable")
	options.Ports = []sandbox.Port{{Number: 3000, Protocol: "tcp"}}
	created, err := app.Create(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(created.ID), "sbx-") || len(created.ID) != len("sbx-")+32 || string(created.ID) == runtime.nativeID {
		t.Fatalf("public identity %q is not distinct sbx-128-bit ID", created.ID)
	}
	if created.Name != "fixture" || created.URL != "http://fixture.localhost:43771" {
		t.Fatalf("created public response=%+v", created)
	}
	if cache.materializations != 1 || cache.image.RootDigest != root.Root.Digest.String() || cache.image.ManifestDigest != root.Manifest.Digest.String() || created.ID == "" {
		t.Fatalf("cache provenance/materialization=%+v count=%d", cache.image, cache.materializations)
	}
	row, err := repo.FindByID(string(created.ID))
	if err != nil || row == nil {
		t.Fatalf("public persisted row=%+v err=%v", row, err)
	}
	if row.NativeID != runtime.nativeID || row.ImageRoot != root.Root.Digest.String() || row.ImageManifest != root.Manifest.Digest.String() || row.NativeImage != "native-cache:"+root.Manifest.Digest.String() || row.CacheVersion != "fake:1" {
		t.Fatalf("identity/provenance row=%+v", row)
	}
	if _, err := app.Inspect(ctx, sandbox.SandboxID(runtime.nativeID)); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("native ID alias lookup err=%v want public not-found", err)
	}
	detail, err := app.Inspect(ctx, created.ID)
	if err != nil || detail.ID != created.ID || detail.Name != "fixture" {
		t.Fatalf("public inspect=%+v err=%v", detail, err)
	}
	items, err := app.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != created.ID || string(items[0].Image) != root.Root.Digest.String() {
		t.Fatalf("public list=%+v err=%v", items, err)
	}
	if got, err := app.Route(ctx, "fixture"); err != nil || got != "39001" {
		t.Fatalf("Route()=%q err=%v", got, err)
	}
	if _, err := app.Start(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Restart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetNetwork(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.RenewExpiration(ctx, created.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Stats(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.Pause(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.Resume(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	cmd, err := app.ExecCommand(ctx, created.ID, sandbox.ProcessRequest{Command: "echo"})
	if err != nil || cmd.SandboxID != created.ID {
		t.Fatalf("ExecCommand public backreference=%+v err=%v", cmd, err)
	}
	for _, call := range []func() error{
		func() error {
			got, err := app.GetCommand(ctx, created.ID, cmd.ID)
			if err == nil && got.SandboxID != created.ID {
				t.Errorf("GetCommand leaked native ID: %+v", got)
			}
			return err
		},
		func() error {
			got, err := app.ListCommands(ctx, created.ID)
			if err == nil && (len(got) != 1 || got[0].SandboxID != created.ID) {
				t.Errorf("ListCommands leaked native ID: %+v", got)
			}
			return err
		},
		func() error {
			got, err := app.KillCommand(ctx, created.ID, cmd.ID, 15)
			if err == nil && got.SandboxID != created.ID {
				t.Errorf("KillCommand leaked native ID: %+v", got)
			}
			return err
		},
		func() error {
			got, err := app.WaitCommand(ctx, created.ID, cmd.ID)
			if err == nil && got.SandboxID != created.ID {
				t.Errorf("WaitCommand leaked native ID: %+v", got)
			}
			return err
		},
		func() error { _, err := app.GetCommandLogs(ctx, created.ID, cmd.ID); return err },
		func() error {
			stdout, stderr, err := app.StreamCommandLogs(ctx, created.ID, cmd.ID)
			if stdout != nil {
				_ = stdout.Close()
			}
			if stderr != nil {
				_ = stderr.Close()
			}
			return err
		},
		func() error { _, err := app.ReadFile(ctx, created.ID, "/tmp/x"); return err },
		func() error { return app.WriteFile(ctx, created.ID, "/tmp/x", "value") },
		func() error { return app.DeleteFile(ctx, created.ID, "/tmp/x") },
		func() error { _, err := app.ListDir(ctx, created.ID, "/tmp"); return err },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	commands, err := repo.FindCommandsBySandbox(string(created.ID))
	if err != nil || len(commands) == 0 {
		t.Fatalf("public command history=%+v err=%v", commands, err)
	}
	for _, command := range commands {
		if command.SandboxID != string(created.ID) {
			t.Errorf("public history leaked native ID: %+v", command)
		}
	}
	nativeCommands, err := repo.NativeView().FindCommandsBySandbox(runtime.nativeID)
	if err != nil || len(nativeCommands) == 0 || nativeCommands[0].SandboxID != runtime.nativeID {
		t.Fatalf("adapter command view=%+v err=%v", nativeCommands, err)
	}
	// Recreate the service/runtime facade around the same execution DB to prove IDs
	// and command history remain usable after reopening, not only in-memory.
	reopenedDB := database.New(dbPath)
	reopenedSQL, err := reopenedDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedSQL.Close() })
	reopenedRepo := database.NewRepository(reopenedDB)
	reopenedRuntime := &runtimeEngineFake{repo: reopenedRepo.NativeView(), nativeID: runtime.nativeID}
	reopenedCache := &cacheFake{caps: cache.caps}
	reopenedAdapter := runtimeio.New(reopenedRuntime, reopenedCache, reopenedRepo)
	reopened, err := New(ctx, reopenedAdapter, reopenedAdapter, store, reopenedRepo)
	if err != nil {
		t.Fatal(err)
	}
	if detail, err := reopened.Inspect(ctx, created.ID); err != nil || detail.ID != created.ID {
		t.Fatalf("reopened inspect=%+v err=%v", detail, err)
	}
	if _, err := reopened.Start(ctx, created.ID); err != nil {
		t.Fatalf("reopened start by public ID: %v", err)
	}
	if err := app.Remove(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if row, err := repo.FindByID(string(created.ID)); err != nil || row != nil {
		t.Fatalf("removed public row=%+v err=%v", row, err)
	}
	if len(runtime.calls) < 20 {
		t.Fatalf("expected every exercised call path to hit runtime, got %d calls: %+v", len(runtime.calls), runtime.calls)
	}
	for _, call := range runtime.calls {
		if call.id != "" && call.op != "create" && call.id != runtime.nativeID {
			t.Errorf("runtime operation %s received public/incorrect ID %q instead of native ID %q", call.op, call.id, runtime.nativeID)
		}
	}
}

func TestCreateFailsBeforeCacheOrRuntimeForMissingUnsupportedAndFractionalImages(t *testing.T) {
	app, runtime, cache, _, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "apple", Version: "1", FractionalCPU: false})
	ctx := context.Background()
	if _, err := app.Create(ctx, createOptions("example.test/missing:tag")); err != sandbox.ErrImageNotFound {
		t.Fatalf("missing image err=%v", err)
	}
	if cache.materializations != 0 || len(runtime.calls) != 0 {
		t.Fatalf("cache/runtime mutated on missing image: cache=%d calls=%+v", cache.materializations, runtime.calls)
	}
	if _, err := app.Create(ctx, sandbox.CreateOptions{Image: "example.test/team/app:stable", Resources: sandbox.ResourceLimits{CPUs: 1.5}}); !errors.Is(err, sandbox.ErrUnsupported) {
		t.Fatalf("fractional CPU err=%v", err)
	}
	if cache.materializations != 0 || len(runtime.calls) != 0 {
		t.Fatalf("cache/runtime mutated on unsupported request: cache=%d calls=%+v", cache.materializations, runtime.calls)
	}
}

func TestCreateRejectsInvalidTypedLimitsTimeoutAndPortsBeforeCacheMutation(t *testing.T) {
	app, runtime, cache, _, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1", FractionalCPU: true})
	for _, tc := range []struct {
		name    string
		options sandbox.CreateOptions
	}{
		{name: "NaN CPU", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Resources: sandbox.ResourceLimits{CPUs: math.NaN()}}},
		{name: "infinite CPU", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Resources: sandbox.ResourceLimits{CPUs: math.Inf(1)}}},
		{name: "negative CPU", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Resources: sandbox.ResourceLimits{CPUs: -1}}},
		{name: "CPU over max", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Resources: sandbox.ResourceLimits{CPUs: 5}}},
		{name: "negative memory", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Resources: sandbox.ResourceLimits{MemoryMB: -1}}},
		{name: "memory over max", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Resources: sandbox.ResourceLimits{MemoryMB: 8193}}},
		{name: "negative timeout", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Timeout: -time.Second}},
		{name: "fractional timeout", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Timeout: 1500 * time.Millisecond}},
		{name: "invalid guest port", options: sandbox.CreateOptions{Image: "example.test/team/app:stable", Ports: []sandbox.Port{{Number: 0, Protocol: "tcp"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := app.Create(context.Background(), tc.options); !errors.Is(err, sandbox.ErrInvalidInput) {
				t.Fatalf("invalid options error=%v want ErrInvalidInput", err)
			}
		})
	}
	if cache.materializations != 0 || len(runtime.calls) != 0 {
		t.Fatalf("invalid domain inputs crossed cache/runtime: cache=%d runtime=%+v", cache.materializations, runtime.calls)
	}
}

func TestCacheMaterializationFailureDoesNotCreateNativeSandbox(t *testing.T) {
	app, runtime, cache, _, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	cache.err = errors.New("native image import failed")
	_, err := app.Create(context.Background(), createOptions("example.test/team/app:stable"))
	if err == nil || !strings.Contains(err.Error(), "native image import failed") {
		t.Fatalf("materialization error=%v", err)
	}
	if cache.materializations != 1 || len(runtime.calls) != 0 {
		t.Fatalf("failed cache import reached backend: cacheCalls=%d runtimeCalls=%+v", cache.materializations, runtime.calls)
	}
}

type preparedImageStub struct{ identity sandbox.Provenance }

func (p *preparedImageStub) Identity() sandbox.Provenance { return p.identity }

type cacheResultFake struct {
	caps   sandbox.Capabilities
	result sandbox.PreparedImage
	err    error
	calls  int
}

func (c *cacheResultFake) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return c.caps, nil
}
func (c *cacheResultFake) Materialize(context.Context, sandbox.Image) (sandbox.PreparedImage, error) {
	c.calls++
	return c.result, c.err
}

type createOnlyRuntimeFake struct {
	sandbox.Runtime
	creates     int
	transaction sandbox.Provisioned
	factory     func(sandbox.RunOptions) sandbox.Provisioned
	err         error
}

func (r *createOnlyRuntimeFake) Create(_ context.Context, opts sandbox.RunOptions) (sandbox.Provisioned, error) {
	r.creates++
	if r.factory != nil {
		return r.factory(opts), r.err
	}
	return r.transaction, r.err
}

type provisionedResultFake struct {
	result                            sandbox.Created
	adoptErr, rollbackErr, recoverErr error
	adopted, rolledBack, recovered    int
}

func (p *provisionedResultFake) Sandbox() sandbox.Created { return p.result }
func (p *provisionedResultFake) Adopt(context.Context, sandbox.Provenance) error {
	p.adopted++
	return p.adoptErr
}
func (p *provisionedResultFake) Rollback(context.Context) error { p.rolledBack++; return p.rollbackErr }
func (p *provisionedResultFake) Recover(context.Context, sandbox.Provenance, error) error {
	p.recovered++
	return p.recoverErr
}

func TestCreateRejectsMissingOrMismatchedPreparedCacheProvenanceBeforeRuntime(t *testing.T) {
	_, _, _, repo, store, platform, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	caps := sandbox.Capabilities{Runtime: "fixture", Version: "1", Platform: sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture}}
	for _, tc := range []struct {
		name     string
		prepared sandbox.PreparedImage
	}{
		{name: "nil prepared image"},
		{name: "wrong root", prepared: &preparedImageStub{identity: sandbox.Provenance{Root: "sha256:wrong-root", Manifest: "sha256:manifest"}}},
		{name: "wrong manifest", prepared: &preparedImageStub{identity: sandbox.Provenance{Root: "sha256:root", Manifest: "sha256:wrong-manifest"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &createOnlyRuntimeFake{}
			cache := &cacheResultFake{caps: caps, result: tc.prepared}
			app := &Service{Runtime: runtime, cache: cache, images: store, repo: repo.PublicView(), caps: caps}
			if _, err := app.Create(context.Background(), createOptions("example.test/team/app:stable")); err == nil {
				t.Fatal("invalid cache preparation passed Create")
			}
			if runtime.creates != 0 || cache.calls != 1 {
				t.Fatalf("failed cache preparation crossed native Create: creates=%d materialize=%d", runtime.creates, cache.calls)
			}
		})
	}
}

func TestCreateRejectsNilProvisionHandleAndPropagatesRuntimeCreateFailure(t *testing.T) {
	_, _, _, repo, store, platform, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	caps := sandbox.Capabilities{Runtime: "fixture", Version: "1", Platform: sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture}}
	image, err := store.Resolve(context.Background(), "example.test/team/app:stable", platform)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedImageStub{identity: sandbox.Provenance{Root: image.Root.Digest.String(), Manifest: image.Manifest.Digest.String()}}
	for _, tc := range []struct {
		name         string
		runtimeError error
		want         string
	}{
		{name: "nil transaction", want: "no creation transaction"},
		{name: "native create error", runtimeError: errors.New("native create failed"), want: "native create failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &createOnlyRuntimeFake{err: tc.runtimeError}
			cache := &cacheResultFake{caps: caps, result: prepared}
			app := &Service{Runtime: runtime, cache: cache, images: store, repo: repo.PublicView(), caps: caps}
			if _, err := app.Create(context.Background(), createOptions("example.test/team/app:stable")); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("create error=%v want %q", err, tc.want)
			}
			if runtime.creates != 1 {
				t.Fatalf("native create called %d times", runtime.creates)
			}
		})
	}
}

func TestCreatePropagatesNativeProvisionFailureWithoutPublishingOwnership(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	createErr := errors.New("native engine rejected create")
	runtime.createErr = createErr
	if _, err := app.Create(context.Background(), createOptions("example.test/team/app:stable")); !errors.Is(err, createErr) {
		t.Fatalf("Create() native error=%v", err)
	}
	rows, err := repo.FindAll()
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed native creation published ownership: rows=%+v err=%v", rows, err)
	}
	if len(runtime.removedIDs) != 0 {
		t.Fatalf("backend reported Create failure but compensation targeted IDs=%v", runtime.removedIDs)
	}
}

func TestCreateRejectsRuntimeMissingProvisionTransaction(t *testing.T) {
	_, _, _, repo, store, platform, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	image, err := store.Resolve(context.Background(), "example.test/team/app:stable", platform)
	if err != nil {
		t.Fatal(err)
	}
	caps := sandbox.Capabilities{Runtime: "fixture", Version: "1", Platform: sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture}}
	cache := &cacheResultFake{caps: caps, result: &preparedImageStub{identity: sandbox.Provenance{Root: image.Root.Digest.String(), Manifest: image.Manifest.Digest.String()}}}
	runtime := &createOnlyRuntimeFake{}
	app := &Service{Runtime: runtime, cache: cache, images: store, repo: repo.PublicView(), caps: caps}
	if _, err := app.Create(context.Background(), createOptions("example.test/team/app:stable")); err == nil || !strings.Contains(err.Error(), "no creation transaction") {
		t.Fatalf("nil provisioned handle error=%v", err)
	}
	if runtime.creates != 1 {
		t.Fatalf("Create called %d times, want 1", runtime.creates)
	}
}

func TestCreateRequiresTransactionIdentityToMatchAllocatedPublicIDAndRunsRecoveryOnRollbackFailure(t *testing.T) {
	_, _, _, repo, store, platform, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	ctx := context.Background()
	image, err := store.Resolve(ctx, "example.test/team/app:stable", platform)
	if err != nil {
		t.Fatal(err)
	}
	caps := sandbox.Capabilities{Runtime: "fixture", Version: "1", Platform: sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture}}
	cache := &cacheResultFake{caps: caps, result: &preparedImageStub{identity: sandbox.Provenance{Root: image.Root.Digest.String(), Manifest: image.Manifest.Digest.String()}}}
	var transaction *provisionedResultFake
	runtime := &createOnlyRuntimeFake{factory: func(options sandbox.RunOptions) sandbox.Provisioned {
		transaction = &provisionedResultFake{result: sandbox.Created{ID: "sbx-wrong-identity"}}
		return transaction
	}}
	app := &Service{Runtime: runtime, cache: cache, images: store, repo: repo.PublicView(), caps: caps}
	if _, err := app.Create(ctx, createOptions("example.test/team/app:stable")); err == nil || !strings.Contains(err.Error(), "public identity mismatch") {
		t.Fatalf("mismatched transaction identity error=%v", err)
	}
	if transaction.adopted != 1 || transaction.rolledBack != 1 || transaction.recovered != 0 {
		t.Fatalf("identity mismatch transaction calls=%+v", transaction)
	}
	runtime.factory = func(options sandbox.RunOptions) sandbox.Provisioned {
		transaction = &provisionedResultFake{result: sandbox.Created{ID: options.ID}, adoptErr: errors.New("adopt failed"), rollbackErr: errors.New("rollback failed")}
		return transaction
	}
	_, err = app.Create(ctx, createOptions("example.test/team/app:stable"))
	if err == nil || !strings.Contains(err.Error(), "adopt failed") || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("joined provisioning failure=%v", err)
	}
	if transaction.adopted != 1 || transaction.rolledBack != 1 || transaction.recovered != 1 {
		t.Fatalf("rollback failure recovery calls=%+v", transaction)
	}
}

func TestCreateCompensatesWhenRuntimeReturnsWithoutOwnershipRow(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	runtime.skipCreateRow = true
	_, err := app.Create(context.Background(), createOptions("example.test/team/app:stable"))
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("missing ownership result error=%v", err)
	}
	if len(runtime.removedIDs) != 1 || runtime.removedIDs[0] != runtime.nativeID {
		t.Fatalf("post-create orphan was not compensated by exact native ID: removed=%v want=%s", runtime.removedIDs, runtime.nativeID)
	}
	if row, err := repo.FindByID(runtime.createdPublicID); err != nil || row != nil {
		t.Fatalf("compensated public row=%+v err=%v", row, err)
	}
}

func TestCreateCompensatesWhenPersistedNativeIdentityDoesNotMatchRuntimeResult(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	runtime.savedNativeID = "native-wrong-resource"
	_, err := app.Create(context.Background(), createOptions("example.test/team/app:stable"))
	if err == nil || !strings.Contains(err.Error(), "native identity mismatch") {
		t.Fatalf("identity mismatch result error=%v", err)
	}
	if len(runtime.removedIDs) != 1 || runtime.removedIDs[0] != runtime.nativeID {
		t.Fatalf("identity mismatch cleanup targeted wrong native resource: removed=%v want=%s", runtime.removedIDs, runtime.nativeID)
	}
	if row, err := repo.FindByID(runtime.createdPublicID); err != nil || row != nil {
		t.Fatalf("identity-mismatch row survived successful compensation: row=%+v err=%v", row, err)
	}
}

func TestCreateCompensatesAfterProvenancePersistenceFailure(t *testing.T) {
	app, runtime, _, repo, _, _, dbPath := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	adminDB := database.New(dbPath)
	adminSQL, err := adminDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminSQL.Close() })
	if err := adminDB.Exec("CREATE TRIGGER reject_service_provenance BEFORE UPDATE ON sandboxes BEGIN SELECT RAISE(FAIL, 'injected provenance save failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	_, err = app.Create(context.Background(), createOptions("example.test/team/app:stable"))
	if err == nil || !strings.Contains(err.Error(), "injected provenance save failure") {
		t.Fatalf("provenance persistence failure=%v", err)
	}
	if len(runtime.removedIDs) != 1 || runtime.removedIDs[0] != runtime.nativeID {
		t.Fatalf("failed provenance left native sandbox orphaned: removed=%v want=%s", runtime.removedIDs, runtime.nativeID)
	}
	if row, err := repo.FindByID(runtime.createdPublicID); err != nil || row != nil {
		t.Fatalf("provenance-failed row survived successful compensation: row=%+v err=%v", row, err)
	}
}

func TestCreatePersistsRecoveryOwnershipWhenCompensationFails(t *testing.T) {
	app, runtime, cache, repo, store, platform, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	runtime.skipCreateRow = true
	runtime.removeErr = errors.New("native delete failed")
	_, err := app.Create(context.Background(), createOptions("example.test/team/app:stable"))
	if err == nil || !strings.Contains(err.Error(), "native delete failed") {
		t.Fatalf("combined create/compensation failure=%v", err)
	}
	if len(runtime.removedIDs) != 1 || runtime.removedIDs[0] != runtime.nativeID {
		t.Fatalf("compensation attempted IDs=%v want exact native=%s", runtime.removedIDs, runtime.nativeID)
	}
	row, findErr := repo.FindByID(runtime.createdPublicID)
	if findErr != nil || row == nil {
		t.Fatalf("recovery ownership record missing: row=%+v err=%v", row, findErr)
	}
	artifact, resolveErr := store.Resolve(context.Background(), "example.test/team/app:stable", platform)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if row.NativeID != runtime.nativeID || row.ImageRoot != artifact.Root.Digest.String() || row.ImageManifest != artifact.Manifest.Digest.String() || row.NativeImage != "native-cache:"+artifact.Manifest.Digest.String() || row.CacheVersion != cache.caps.Runtime+":"+cache.caps.Version {
		t.Fatalf("recovery row lost native ownership/provenance: %+v", row)
	}
}

func TestServiceImagePullInspectRemoveStayInOwnedCatalogWithoutRuntimeCalls(t *testing.T) {
	ctx := context.Background()
	registryServer := httptest.NewServer(registry.New())
	t.Cleanup(registryServer.Close)
	ref := strings.TrimPrefix(registryServer.URL, "http://") + "/team/service:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", "amd64"
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	transport := registryServer.Client().Transport
	if err := remote.Write(parsed, image, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	store, err := images.Open(t.TempDir(), images.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	db := database.New(filepath.Join(t.TempDir(), "execution.db"))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	cache := &cacheFake{caps: sandbox.Capabilities{Runtime: "fake", Version: "1", Platform: sandbox.Platform{OS: "linux", Architecture: "amd64"}}}
	runtime := &runtimeEngineFake{repo: repo.NativeView(), nativeID: "native-unrelated"}
	adapter := runtimeio.New(runtime, cache, repo)
	app, err := New(ctx, adapter, adapter, store, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.PullImage(ctx, ref); err != nil {
		t.Fatal(err)
	}
	detail, err := app.InspectImage(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if detail.OS != "linux" || detail.Architecture != "amd64" {
		t.Fatalf("pulled image detail=%+v", detail)
	}
	list, err := app.ListImages(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("image list=%+v err=%v", list, err)
	}
	if err := app.RemoveImage(ctx, ref, false); err != nil {
		t.Fatal(err)
	}
	list, err = app.ListImages(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("image remove left catalog refs: %+v err=%v", list, err)
	}
	if len(runtime.calls) != 0 || cache.materializations != 0 {
		t.Fatalf("catalog operation contacted execution/cache runtime: runtime=%+v materializations=%d", runtime.calls, cache.materializations)
	}
}

func TestCreateForPreparedImageDoesNotWaitForConcurrentRegistryPull(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	baseRegistry := registry.New()
	var block atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/slow") {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
		baseRegistry.ServeHTTP(w, r)
	}))
	t.Cleanup(registryServer.Close)
	transport := registryServer.Client().Transport
	registryRef := strings.TrimPrefix(registryServer.URL, "http://") + "/team/waiting:slow"
	parsed, err := name.ParseReference(registryRef, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	registryImage, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	imageConfig, err := registryImage.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	imageConfig.OS, imageConfig.Architecture = "linux", "amd64"
	registryImage, err = mutate.ConfigFile(registryImage, imageConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, registryImage, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	store, err := images.Open(t.TempDir(), images.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	archive, platform := testsupport.OCIArchive(t)
	const preparedRef = "example.test/team/prepared:existing"
	if err := store.Import(context.Background(), archive, preparedRef, platform); err != nil {
		t.Fatal(err)
	}
	db := database.New(filepath.Join(t.TempDir(), "execution.db"))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	cache := &cacheFake{caps: sandbox.Capabilities{Runtime: "fake", Version: "1", Platform: sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture}}}
	runtime := &runtimeEngineFake{repo: repo.NativeView(), nativeID: "native-from-prepared"}
	adapter := runtimeio.New(runtime, cache, repo)
	app, err := New(context.Background(), adapter, adapter, store, repo)
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	pullDone := make(chan error, 1)
	go func() {
		pullDone <- store.Pull(context.Background(), registryRef, v1.Platform{OS: "linux", Architecture: "amd64"})
	}()
	defer func() { releaseOnce.Do(func() { close(release) }); <-pullDone }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow registry did not reach blocked manifest response")
	}
	createDone := make(chan error, 1)
	go func() {
		result, err := app.Create(context.Background(), sandbox.CreateOptions{Image: preparedRef})
		if err == nil && !strings.HasPrefix(string(result.ID), "sbx-") {
			err = fmt.Errorf("prepared create returned non-public ID %q", result.ID)
		}
		createDone <- err
	}()
	select {
	case err := <-createDone:
		if err != nil {
			t.Fatalf("Create(prepared image) during unrelated pull: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		pullErr := <-pullDone
		pullDone = make(chan error, 1)
		pullDone <- pullErr
		createErr := <-createDone
		t.Fatalf("prepared Create blocked behind unrelated registry I/O; after release create err=%v pull err=%v", createErr, pullErr)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-pullDone; err != nil {
		pullDone = make(chan error, 1)
		pullDone <- err
		t.Fatalf("background synthetic pull: %v", err)
	}
	pullDone = make(chan error, 1)
	pullDone <- nil
}

func TestLegacySandboxIDRemainsPublicAndServesAsNativeFallback(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	const legacyID = "legacy-native-container-42"
	if err := repo.Save(database.Sandbox{ID: legacyID, Name: "legacy"}); err != nil {
		t.Fatal(err)
	}
	detail, err := app.Inspect(context.Background(), sandbox.SandboxID(legacyID))
	if err != nil || detail.ID != legacyID {
		t.Fatalf("legacy inspect=%+v err=%v", detail, err)
	}
	if _, err := app.Start(context.Background(), sandbox.SandboxID(legacyID)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.calls) != 2 || runtime.calls[0].id != legacyID || runtime.calls[1].id != legacyID {
		t.Fatalf("legacy public ID was not passed as native fallback: %+v", runtime.calls)
	}
	stored, err := repo.FindByID(legacyID)
	if err != nil || stored == nil || stored.ID != legacyID || stored.NativeID != "" {
		t.Fatalf("legacy primary key/identity changed: row=%+v err=%v", stored, err)
	}
}

func TestEveryRuntimeOperationRejectsUnknownPublicIDBeforeBackendMutation(t *testing.T) {
	app, runtime, _, _, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1", Pause: true})
	ctx := context.Background()
	checks := []struct {
		name string
		call func() error
	}{
		{"start", func() error { _, err := app.Start(ctx, sandbox.SandboxID("native-alias")); return err }},
		{"restart", func() error { _, err := app.Restart(ctx, sandbox.SandboxID("native-alias")); return err }},
		{"stop", func() error { return app.Stop(ctx, sandbox.SandboxID("native-alias")) }},
		{"remove", func() error { return app.Remove(ctx, sandbox.SandboxID("native-alias")) }},
		{"renew", func() error { return app.RenewExpiration(ctx, sandbox.SandboxID("native-alias"), time.Minute) }},
		{"network", func() error { _, err := app.GetNetwork(ctx, sandbox.SandboxID("native-alias")); return err }},
		{"stats", func() error { _, err := app.Stats(ctx, sandbox.SandboxID("native-alias")); return err }},
		{"pause", func() error { return app.Pause(ctx, sandbox.SandboxID("native-alias")) }},
		{"resume", func() error { return app.Resume(ctx, sandbox.SandboxID("native-alias")) }},
		{"exec", func() error {
			_, err := app.ExecCommand(ctx, sandbox.SandboxID("native-alias"), sandbox.ProcessRequest{Command: "echo"})
			return err
		}},
		{"get command", func() error { _, err := app.GetCommand(ctx, sandbox.SandboxID("native-alias"), "cmd"); return err }},
		{"list commands", func() error { _, err := app.ListCommands(ctx, sandbox.SandboxID("native-alias")); return err }},
		{"kill", func() error { _, err := app.KillCommand(ctx, sandbox.SandboxID("native-alias"), "cmd", 9); return err }},
		{"wait", func() error { _, err := app.WaitCommand(ctx, sandbox.SandboxID("native-alias"), "cmd"); return err }},
		{"logs", func() error { _, err := app.GetCommandLogs(ctx, sandbox.SandboxID("native-alias"), "cmd"); return err }},
		{"stream", func() error {
			_, _, err := app.StreamCommandLogs(ctx, sandbox.SandboxID("native-alias"), "cmd")
			return err
		}},
		{"read", func() error { _, err := app.ReadFile(ctx, sandbox.SandboxID("native-alias"), "/tmp/x"); return err }},
		{"write", func() error { return app.WriteFile(ctx, sandbox.SandboxID("native-alias"), "/tmp/x", "x") }},
		{"delete file", func() error { return app.DeleteFile(ctx, sandbox.SandboxID("native-alias"), "/tmp/x") }},
		{"list dir", func() error { _, err := app.ListDir(ctx, sandbox.SandboxID("native-alias"), "/tmp"); return err }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, sandbox.ErrNotFound) {
				t.Fatalf("error=%v want ErrNotFound", err)
			}
		})
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("unknown public IDs reached execution runtime: %+v", runtime.calls)
	}
}

func TestRouteRejectsMissingStoppedAndNonTCPSandboxes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*database.Repository, *runtimeEngineFake)
	}{
		{name: "missing public ownership", prepare: func(*database.Repository, *runtimeEngineFake) {}},
		{name: "stopped", prepare: func(repo *database.Repository, runtime *runtimeEngineFake) {
			_ = repo.Save(database.Sandbox{ID: "sbx-owned", NativeID: runtime.nativeID, Name: "fixture"})
			stopped := false
			runtime.running = &stopped
		}},
		{name: "UDP main port", prepare: func(repo *database.Repository, runtime *runtimeEngineFake) {
			_ = repo.Save(database.Sandbox{ID: "sbx-owned", NativeID: runtime.nativeID, Name: "fixture"})
			runtime.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "53/udp", PortsMap: map[string]string{"53/udp": "39001"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
			tc.prepare(repo, runtime)
			if _, err := app.Route(context.Background(), "fixture"); err == nil {
				t.Fatal("invalid route unexpectedly succeeded")
			}
		})
	}
}

func TestServiceRejectsNonLinuxCapabilityBeforeApplicationConstruction(t *testing.T) {
	cache := &cacheFake{caps: sandbox.Capabilities{Platform: sandbox.Platform{OS: "darwin", Architecture: "arm64"}}}
	repoDB := database.New(filepath.Join(t.TempDir(), "db.sqlite"))
	repo := database.NewRepository(repoDB)
	adapter := runtimeio.New(&runtimeEngineFake{repo: repo.NativeView()}, cache, repo)
	if _, err := New(context.Background(), adapter, adapter, nil, repo); !errors.Is(err, sandbox.ErrUnsupported) {
		t.Fatalf("New() error=%v want unsupported", err)
	}
}

func TestServiceNewPropagatesCacheCapabilitiesFailure(t *testing.T) {
	cacheErr := errors.New("native capability probe failed")
	cache := &cacheFake{capsErr: cacheErr}
	db := database.New(filepath.Join(t.TempDir(), "db.sqlite"))
	repo := database.NewRepository(db)
	adapter := runtimeio.New(&runtimeEngineFake{repo: repo.NativeView()}, cache, repo)
	if _, err := New(context.Background(), adapter, adapter, nil, repo); !errors.Is(err, cacheErr) {
		t.Fatalf("New() error=%v want capabilities error", err)
	}
}

func TestRoutePropagatesRuntimeErrorsAndRejectsInvalidNetworkData(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantEmpty bool
		prepare   func(*database.Repository, *runtimeEngineFake)
	}{
		{name: "inspect error", prepare: func(repo *database.Repository, r *runtimeEngineFake) {
			_ = repo.Save(database.Sandbox{ID: "sbx-owned", NativeID: r.nativeID, Name: "fixture"})
			r.routingErr = errors.New("inspect unavailable")
		}},
		{name: "network error", prepare: func(repo *database.Repository, r *runtimeEngineFake) {
			_ = repo.Save(database.Sandbox{ID: "sbx-owned", NativeID: r.nativeID, Name: "fixture"})
			r.routingErr = errors.New("network unavailable")
		}},
		{name: "missing published main port", wantEmpty: true, prepare: func(repo *database.Repository, r *runtimeEngineFake) {
			_ = repo.Save(database.Sandbox{ID: "sbx-owned", NativeID: r.nativeID, Name: "fixture"})
			r.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
			tc.prepare(repo, runtime)
			got, err := app.Route(context.Background(), "fixture")
			if tc.wantEmpty {
				if err != nil || got != "" {
					t.Fatalf("missing published mapping route=%q err=%v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid backend route unexpectedly succeeded")
			}
		})
	}
}

func TestRouteUsesOneCoherentBackendSnapshotAndDoesNotCachePublishedPorts(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	if err := repo.Save(database.Sandbox{ID: "sbx-route", NativeID: runtime.nativeID, Name: "fixture"}); err != nil {
		t.Fatal(err)
	}
	runtime.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}}
	first, err := app.Route(context.Background(), "fixture")
	if err != nil || first != "39001" {
		t.Fatalf("initial coherent route=%q err=%v", first, err)
	}
	runtime.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39002"}}}
	second, err := app.Route(context.Background(), "fixture")
	if err != nil || second != "39002" {
		t.Fatalf("route reused a stale published port: port=%q err=%v", second, err)
	}
	if len(runtime.calls) != 2 || runtime.calls[0].op != "routing" || runtime.calls[1].op != "routing" {
		t.Fatalf("route must use one coherent runtime snapshot per resolution, calls=%+v", runtime.calls)
	}
	stopped := *runtime.routeValue
	stopped.Running = false
	runtime.routeValue = &stopped
	if _, err := app.Route(context.Background(), "fixture"); !errors.Is(err, sandbox.ErrNotRunning) {
		t.Fatalf("stopped route error=%v", err)
	}
}

func TestConcurrentRouteLookupsCoalesceInflightSnapshotWithoutCachingResult(t *testing.T) {
	app, engine, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	if err := repo.Save(database.Sandbox{ID: "sbx-route", NativeID: engine.nativeID, Name: "fixture"}); err != nil {
		t.Fatal(err)
	}
	block, entered := make(chan struct{}), make(chan struct{})
	engine.routingBlock, engine.routingEntered = block, entered
	const workers = 12
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	results := make(chan string, workers)
	errs := make(chan error, workers)
	ready.Add(workers)
	done.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			port, err := app.Route(context.Background(), "fixture")
			results <- port
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no live routing snapshot started")
	}
	time.Sleep(100 * time.Millisecond)
	close(block)
	done.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("coalesced route error: %v", err)
		}
	}
	for port := range results {
		if port != "39001" {
			t.Errorf("coalesced route returned port %q", port)
		}
	}
	routingCalls := 0
	for _, call := range engine.calls {
		if call.op == "routing" {
			routingCalls++
		}
	}
	if routingCalls != 1 {
		t.Fatalf("simultaneous route lookups issued %d backend snapshots, want one", routingCalls)
	}
	engine.routingBlock = nil
	engine.routingEntered = nil
	engine.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39002"}}}
	if port, err := app.Route(context.Background(), "fixture"); err != nil || port != "39002" {
		t.Fatalf("later route reused stale coalesced result: port=%q err=%v", port, err)
	}
}

func TestRouteCancellationStopsOnlyTheWaitingCaller(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	if err := repo.Save(database.Sandbox{ID: "sbx-cancel-route", NativeID: runtime.nativeID, Name: "fixture"}); err != nil {
		t.Fatal(err)
	}
	block, entered := make(chan struct{}), make(chan struct{})
	runtime.routingBlock, runtime.routingEntered = block, entered
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := app.Route(ctx, "fixture")
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(block)
		cancel()
		t.Fatal("routing snapshot did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			close(block)
			t.Fatalf("canceled route waiter error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		close(block)
		t.Fatal("route caller remained blocked after its context was canceled")
	}
	close(block)
}

func TestServicePropagatesRepositoryReadFailures(t *testing.T) {
	app, _, _, _, _, _, dbPath := openPreparedService(t, sandbox.Capabilities{Runtime: "fixture", Version: "1"})
	admin := database.New(dbPath)
	if err := admin.Exec("DROP TABLE sandboxes").Error; err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		call func() error
	}{
		{"route lookup", func() error { _, err := app.Route(context.Background(), "fixture"); return err }},
		{"list ownership join", func() error { _, err := app.List(context.Background()); return err }},
		{"inspect ownership lookup", func() error { _, err := app.Inspect(context.Background(), "sbx-gone"); return err }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("repository read failure was suppressed")
			}
		})
	}
}

func TestPauseResumeUnsupportedCapabilitiesDoNotReachRuntime(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1", Pause: false})
	if err := repo.Save(database.Sandbox{ID: "sbx-pause", NativeID: runtime.nativeID, Name: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := app.Pause(context.Background(), sandbox.SandboxID("sbx-pause")); !errors.Is(err, sandbox.ErrUnsupported) {
		t.Fatalf("unsupported pause err=%v", err)
	}
	if err := app.Resume(context.Background(), sandbox.SandboxID("sbx-pause")); !errors.Is(err, sandbox.ErrUnsupported) {
		t.Fatalf("unsupported resume err=%v", err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("unsupported lifecycle operations reached runtime: %+v", runtime.calls)
	}
}

func TestListAndInspectRejectMissingRuntimeOwnershipAndPropagateRuntimeFailures(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	runtime.listID = "unowned-native"
	if _, err := app.List(context.Background()); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("unowned list item error=%v", err)
	}
	runtime.listID = ""
	listErr := errors.New("runtime list failure")
	runtime.listErr = listErr
	if _, err := app.List(context.Background()); !errors.Is(err, listErr) {
		t.Fatalf("List() error=%v want runtime error", err)
	}
	runtime.listErr = nil
	if err := repo.Save(database.Sandbox{ID: "sbx-owned", NativeID: runtime.nativeID, Name: "fixture"}); err != nil {
		t.Fatal(err)
	}
	inspectErr := errors.New("runtime inspect failure")
	runtime.inspectErr = inspectErr
	if _, err := app.Inspect(context.Background(), sandbox.SandboxID("sbx-owned")); !errors.Is(err, inspectErr) {
		t.Fatalf("Inspect() error=%v want runtime error", err)
	}
}

var _ runtimeio.Engine = (*runtimeEngineFake)(nil)
var _ runtimeio.NativeCache = (*cacheFake)(nil)
