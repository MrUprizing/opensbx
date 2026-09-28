package service

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/proxy"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

func TestProxyFailsClosedForRunningSandboxWithPendingOrUnknownIntent(t *testing.T) {
	for _, operation := range []string{"delete", "stop", "create", "start", "restart", "future-operation"} {
		t.Run(operation, func(t *testing.T) {
			app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
			var pendingHits, healthyHits atomic.Int64
			guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/pending":
					pendingHits.Add(1)
					_, _ = io.WriteString(w, "pending guest")
				case "/healthy":
					healthyHits.Add(1)
					_, _ = io.WriteString(w, "healthy guest")
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(guest.Close)
			guestURL := strings.TrimPrefix(guest.URL, "http://")
			_, hostPort, err := net.SplitHostPort(guestURL)
			if err != nil {
				t.Fatal(err)
			}
			runtime.routeValue = &runtimeio.RoutingState{
				Running: true,
				Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": hostPort}},
			}
			pendingRow := database.Sandbox{ID: "sbx-pending", NativeID: "native-pending", RuntimeKind: "fake", Name: "pending", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": hostPort}}
			if operation == "create" {
				pendingRow.AttemptToken = "pending-create-token"
				if err := repo.BeginCreate(database.Operation{ID: "fake:create-pending", RuntimeKind: "fake", PublicID: pendingRow.ID, NativeID: pendingRow.NativeID, NativeName: pendingRow.NativeID, Token: pendingRow.AttemptToken, Kind: "create"}); err != nil {
					t.Fatal(err)
				}
			}
			for _, row := range []database.Sandbox{
				pendingRow,
				{ID: "sbx-healthy", NativeID: "native-healthy", RuntimeKind: "fake", Name: "healthy", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": hostPort}},
			} {
				if err := repo.CreateOwnership(row); err != nil {
					t.Fatal(err)
				}
			}
			if operation != "create" {
				if _, err := repo.BeginOperation("fake", "native-pending", operation, nil); err != nil {
					t.Fatal(err)
				}
			}

			// Management inspection remains available so operators can observe and
			// recover a quarantined resource while app traffic is blocked.
			managed, err := app.Inspect(context.Background(), sandbox.SandboxID("sbx-pending"))
			if err != nil || !managed.Running {
				t.Fatalf("management Inspect should remain available: detail=%+v err=%v", managed, err)
			}
			if current, err := repo.Operations("fake"); err != nil || len(current) != 1 {
				t.Fatalf("destructive intent was not retained: operations=%+v err=%v", current, err)
			}

			liveProxy := proxy.New(repo)
			liveProxy.SetResolver(app.Route)
			front := httptest.NewServer(liveProxy.Handler())
			t.Cleanup(front.Close)
			request := func(name, path string) (int, string, error) {
				req, err := http.NewRequest(http.MethodGet, front.URL+path, nil)
				if err != nil {
					return 0, "", err
				}
				req.Host = name + ".localhost"
				res, err := front.Client().Do(req)
				if err != nil {
					return 0, "", err
				}
				defer res.Body.Close()
				body, err := io.ReadAll(res.Body)
				return res.StatusCode, string(body), err
			}
			pendingStatus, pendingBody, pendingErr := request("pending", "/pending")
			healthyStatus, healthyBody, healthyErr := request("healthy", "/healthy")
			if healthyErr != nil || healthyStatus != http.StatusOK || healthyBody != "healthy guest" || healthyHits.Load() != 1 {
				t.Fatalf("unrelated healthy route was affected: status=%d body=%q hits=%d err=%v", healthyStatus, healthyBody, healthyHits.Load(), healthyErr)
			}
			if pendingErr != nil || pendingStatus < http.StatusBadRequest || pendingHits.Load() != 0 {
				t.Fatalf("pending %s forwarded to a running guest: status=%d body=%q hits=%d err=%v", operation, pendingStatus, pendingBody, pendingHits.Load(), pendingErr)
			}
			if operations, err := repo.Operations("fake"); err != nil || len(operations) != 1 || operations[0].Kind != operation {
				t.Fatalf("routing failure destroyed recoverable %s intent: operations=%+v err=%v", operation, operations, err)
			}
		})
	}
}

func TestRouteRejectsExpiredDeadlinesAndWrongRuntimeButAllowsLegacyOwnership(t *testing.T) {
	for _, scenario := range []string{"expired deadline", "wrong runtime", "legacy owner"} {
		t.Run(scenario, func(t *testing.T) {
			app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
			row := database.Sandbox{ID: "sbx-route-policy", NativeID: runtime.nativeID, RuntimeKind: "fake", Name: "policy", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "39001"}}
			switch scenario {
			case "expired deadline":
				deadline := time.Now().Add(-time.Second)
				row.ExpiresAt = &deadline
			case "wrong runtime":
				row.RuntimeKind = "other-runtime"
			case "legacy owner":
				row.RuntimeKind = ""
			}
			if err := repo.CreateOwnership(row); err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy owner" {
				port, err := app.Route(context.Background(), row.Name)
				if err != nil || port != "39001" {
					t.Fatalf("legacy owner without runtime/deadline should remain routable: port=%q err=%v", port, err)
				}
				return
			}
			if port, err := app.Route(context.Background(), row.Name); err == nil || port != "" {
				t.Fatalf("route admitted %s owner: port=%q err=%v", scenario, port, err)
			}
			for _, call := range runtime.calls {
				if call.op == "routing" {
					t.Fatalf("rejected %s owner still reached native routing: %+v", scenario, runtime.calls)
				}
			}
		})
	}
}

func TestRouteRejectsOwnershipDeletedOrReplacedDuringNativeSnapshot(t *testing.T) {
	for _, change := range []string{"deleted", "replaced"} {
		t.Run(change, func(t *testing.T) {
			app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
			const publicID = "sbx-route-owner-race"
			original := database.Sandbox{ID: publicID, NativeID: runtime.nativeID, RuntimeKind: "fake", AttemptToken: "original-token", Name: "owner-race", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "39001"}}
			if err := repo.CreateOwnership(original); err != nil {
				t.Fatal(err)
			}
			runtime.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}}
			entered, release := make(chan struct{}), make(chan struct{})
			runtime.routingEntered = entered
			runtime.routingBlock = release
			routeDone := make(chan struct {
				port string
				err  error
			}, 1)
			go func() {
				port, err := app.Route(context.Background(), original.Name)
				routeDone <- struct {
					port string
					err  error
				}{port, err}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("route did not reach the native snapshot barrier")
			}
			if change == "deleted" {
				if err := repo.Delete(publicID); err != nil {
					close(release)
					t.Fatal(err)
				}
			} else {
				replacement := original
				replacement.NativeID = "replacement-native"
				replacement.AttemptToken = "replacement-token"
				if err := repo.Save(replacement); err != nil {
					close(release)
					t.Fatal(err)
				}
			}
			close(release)
			select {
			case result := <-routeDone:
				if result.err == nil || result.port != "" {
					t.Fatalf("route admitted %s owner after native snapshot: port=%q err=%v", change, result.port, result.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("route did not return after native snapshot release")
			}
		})
	}
}

func TestSingleflightSeparatesReplacementOwnershipSnapshots(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	const publicID = "sbx-route-key"
	original := database.Sandbox{ID: publicID, NativeID: "native-old", RuntimeKind: "fake", AttemptToken: "old-token", Name: "keyed", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "39001"}}
	if err := repo.CreateOwnership(original); err != nil {
		t.Fatal(err)
	}
	runtime.routeValues = map[string]runtimeio.RoutingState{
		"native-old": {Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}},
		"native-new": {Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39002"}}},
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	barrier := &oneShotRouteBarrier{Runtime: app.Runtime, entered: entered, release: release}
	app.Runtime = barrier
	firstDone := make(chan struct {
		port string
		err  error
	}, 1)
	go func() {
		port, err := app.Route(context.Background(), "keyed")
		firstDone <- struct {
			port string
			err  error
		}{port, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		unblock()
		t.Fatal("original route did not hold its backend snapshot")
	}
	replacement := original
	replacement.NativeID = "native-new"
	replacement.AttemptToken = "new-token"
	if err := repo.Save(replacement); err != nil {
		unblock()
		t.Fatal(err)
	}
	secondDone := make(chan struct {
		port string
		err  error
	}, 1)
	go func() {
		port, err := app.Route(context.Background(), "keyed")
		secondDone <- struct {
			port string
			err  error
		}{port, err}
	}()
	select {
	case result := <-secondDone:
		if result.err != nil || result.port != "39002" {
			unblock()
			t.Fatalf("replacement owner did not get its own live snapshot: port=%q err=%v", result.port, result.err)
		}
	case <-time.After(time.Second):
		unblock()
		<-firstDone
		select {
		case <-secondDone:
		case <-time.After(time.Second):
			t.Fatal("replacement waiter remained stuck after releasing the original snapshot")
		}
		t.Fatal("replacement owner joined the stale singleflight snapshot instead of resolving independently")
	}
	unblock()
	select {
	case result := <-firstDone:
		if result.err == nil || result.port != "" {
			t.Fatalf("stale original owner route was admitted after replacement: port=%q err=%v", result.port, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("original route did not finish after releasing stale snapshot")
	}
}

type oneShotRouteBarrier struct {
	sandbox.Runtime
	entered chan struct{}
	release chan struct{}
	blocked atomic.Bool
}

func (r *oneShotRouteBarrier) Routing(ctx context.Context, id sandbox.SandboxID) (sandbox.Route, error) {
	route, err := r.Runtime.Routing(ctx, id)
	if err != nil {
		return sandbox.Route{}, err
	}
	if r.blocked.CompareAndSwap(false, true) {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return sandbox.Route{}, ctx.Err()
		}
	}
	return route, nil
}

func TestRouteRechecksDestructiveIntentAfterSingleflightBackendSnapshot(t *testing.T) {
	app, runtime, _, repo, _, _, _ := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	const publicID = "sbx-racing-route"
	if err := repo.CreateOwnership(database.Sandbox{ID: publicID, NativeID: runtime.nativeID, RuntimeKind: "fake", Name: "racing", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "39001"}}); err != nil {
		t.Fatal(err)
	}
	runtime.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}}
	entered, release := make(chan struct{}), make(chan struct{})
	runtime.routingEntered = entered
	runtime.routingBlock = release
	routeDone := make(chan struct {
		port string
		err  error
	}, 1)
	go func() {
		port, err := app.Route(context.Background(), "racing")
		routeDone <- struct {
			port string
			err  error
		}{port, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("route did not reach the backend snapshot barrier")
	}
	if _, err := repo.BeginOperation("fake", runtime.nativeID, "delete", nil); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case result := <-routeDone:
		if result.err == nil || result.port != "" {
			t.Fatalf("singleflight route admitted a snapshot after delete intent was inserted: port=%q err=%v", result.port, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("route did not return after releasing the backend snapshot")
	}
}

func TestRouteFailsClosedWhenIntentLookupFailsAfterBackendSnapshot(t *testing.T) {
	app, runtime, _, repo, _, _, dbPath := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	if err := repo.CreateOwnership(database.Sandbox{ID: "sbx-route-db-failure", NativeID: runtime.nativeID, RuntimeKind: "fake", Name: "db-failure", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "39001"}}); err != nil {
		t.Fatal(err)
	}
	runtime.routeValue = &runtimeio.RoutingState{Running: true, Network: runtimeio.SandboxNetwork{MainPort: "3000/tcp", PortsMap: map[string]string{"3000/tcp": "39001"}}}
	entered, release := make(chan struct{}), make(chan struct{})
	runtime.routingEntered = entered
	runtime.routingBlock = release
	routeDone := make(chan error, 1)
	go func() {
		port, err := app.Route(context.Background(), "db-failure")
		if err == nil {
			err = fmt.Errorf("route unexpectedly returned port %q after intent lookup became unavailable", port)
		}
		routeDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("route did not reach the backend snapshot barrier")
	}
	adminDB := database.New(dbPath)
	adminSQL, err := adminDB.DB()
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminSQL.Close() })
	if err := adminDB.Exec("DROP TABLE operations").Error; err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-routeDone:
		if err == nil || !strings.Contains(err.Error(), "operations") {
			t.Fatalf("failed pending-intent lookup was ignored: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("route did not finish after releasing its backend snapshot")
	}
}

func TestRouteFailsClosedWhenIntentLookupFailsBeforeBackendSnapshot(t *testing.T) {
	app, runtime, _, repo, _, _, dbPath := openPreparedService(t, sandbox.Capabilities{Runtime: "fake", Version: "1"})
	if err := repo.CreateOwnership(database.Sandbox{ID: "sbx-route-precheck", NativeID: runtime.nativeID, RuntimeKind: "fake", Name: "fixture", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "39001"}}); err != nil {
		t.Fatal(err)
	}
	adminDB := database.New(dbPath)
	adminSQL, err := adminDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminSQL.Close() })
	if err := adminDB.Exec("DROP TABLE operations").Error; err != nil {
		t.Fatal(err)
	}
	if port, err := app.Route(context.Background(), "fixture"); err == nil || port != "" {
		t.Fatalf("route ignored pre-snapshot intent-read failure: port=%q err=%v", port, err)
	}
	for _, call := range runtime.calls {
		if call.op == "routing" {
			t.Fatalf("intent DB failure reached native routing: %+v", runtime.calls)
		}
	}
}
