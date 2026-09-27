package applecontainer

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/sandbox"
)

func TestLifecycleTransitionsInvalidateCacheAndNeverRecreateOnRestart(t *testing.T) {
	id := "opensbx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	var mu sync.Mutex
	state := "stopped"
	mutations := []string{}
	r := &scriptedRunner{t: t}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(id, state, `[{"HostPort":23456,"ContainerPort":3000,"Proto":"tcp","Count":1}]`))
		case len(args) == 2 && args[0] == "start" && args[1] == id:
			state = "running"
			mutations = append(mutations, "start")
		case len(args) == 2 && args[0] == "stop" && args[1] == id:
			state = "stopped"
			mutations = append(mutations, "stop")
		case len(args) == 3 && args[0] == "delete" && args[1] == "--force" && args[2] == id:
			mutations = append(mutations, "delete")
		default:
			t.Fatalf("unexpected lifecycle argv: %#v", args)
		}
		return nil
	}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id, Image: "node:24", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "23456"}}); err != nil {
		t.Fatal(err)
	}
	var invalidated []string
	c.SetCacheInvalidator(func(name string) { invalidated = append(invalidated, name) })
	if _, err := c.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Restart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mutations, []string{"start", "stop", "start", "delete"}) {
		t.Fatalf("runtime mutations = %v", mutations)
	}
	if !reflect.DeepEqual(invalidated, []string{id, id, id, id, id}) {
		t.Fatalf("cache invalidations = %v", invalidated)
	}
	row, err := repo.FindByID(id)
	if err != nil || row != nil {
		t.Fatalf("removed row = %+v, %v", row, err)
	}
}

func TestLifecycleErrorsAndUnsupportedPauseResumeDoNotMutateRuntime(t *testing.T) {
	id := "opensbx-ffffffffffffffffffffffffffffffff"
	r := &scriptedRunner{t: t}
	state := "running"
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(id, state, "[]"))
		case reflect.DeepEqual(args, []string{"stop", id}):
			state = "stopped"
		default:
			t.Fatalf("unexpected lifecycle CLI: %#v", args)
		}
		return nil
	}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	if err := c.Pause(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Pause error = %v", err)
	}
	if err := c.Resume(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Resume error = %v", err)
	}
	if err := c.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// The runner still reports running, so a duplicate stop reaches the CLI;
	// a stopped error is covered by a backend-reported stopped state below.
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			t.Fatalf("stopped lookup invoked mutation: %#v", args)
		}
		_, _ = io.WriteString(out, listJSON(id, state, "[]"))
		return nil
	}
	if err := c.Stop(context.Background(), id); !errors.Is(err, sandbox.ErrAlreadyStopped) {
		t.Fatalf("already stopped error = %v", err)
	}
	state = "running"
	if _, err := c.Start(context.Background(), id); !errors.Is(err, sandbox.ErrAlreadyRunning) {
		t.Fatalf("already running error = %v", err)
	}
}

func TestExpirationTimerStopsOwnedSandboxAndShutdownCancelsTimers(t *testing.T) {
	id := "opensbx-11111111111111111111111111111111"
	var mu sync.Mutex
	state := "running"
	stopped := make(chan struct{}, 1)
	r := &scriptedRunner{t: t}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(id, state, "[]"))
		case len(args) == 2 && args[0] == "stop" && args[1] == id:
			state = "stopped"
			stopped <- struct{}{}
		default:
			t.Fatalf("unexpected timer argv: %#v", args)
		}
		return nil
	}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.scheduleLocked(id, 1)
	c.mu.Unlock()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("TTL did not stop the sandbox")
	}
	c.mu.Lock()
	_, stillTracked := c.timers[id]
	c.mu.Unlock()
	if stillTracked {
		t.Fatal("expired timer remained active after stop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Shutdown(ctx)
	if !c.closing {
		t.Fatal("Shutdown did not close the backend")
	}
}

func TestListNetworkAndRenewExpirationReflectOwnedState(t *testing.T) {
	id := "opensbx-33333333333333333333333333333333"
	missing := "opensbx-44444444444444444444444444444444"
	r := &scriptedRunner{t: t}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			t.Fatalf("unexpected read-only lookup argv: %#v", args)
		}
		_, _ = io.WriteString(out, listJSON(id, "running", `[{"HostPort":43210,"ContainerPort":8080,"Proto":"tcp","Count":1}]`))
		return nil
	}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: "web", Image: "node:24", Port: "8080/tcp", Ports: database.JSONMap{"8080/tcp": "43210"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(database.Sandbox{ID: missing, Name: "gone", Image: "node:20"}); err != nil {
		t.Fatal(err)
	}
	network, err := c.GetNetwork(context.Background(), id)
	if err != nil || network.MainPort != "8080/tcp" || network.PortsMap["8080/tcp"] != "43210" {
		t.Fatalf("GetNetwork() = %+v, %v", network, err)
	}
	rows, err := c.List(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("List() = %+v, %v", rows, err)
	}
	if rows[0].ID != id || rows[0].State != "running" || rows[0].Name != "web" {
		t.Fatalf("live summary = %+v", rows[0])
	}
	if rows[1].ID != missing || rows[1].State != "removed" {
		t.Fatalf("missing summary = %+v", rows[1])
	}
	if err := c.RenewExpiration(context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	if err := c.RenewExpiration(context.Background(), id, 0); err == nil {
		t.Fatal("zero timeout was accepted")
	}
	c.mu.Lock()
	entry := c.timers[id]
	c.clearTimer(id)
	c.mu.Unlock()
	if entry == nil {
		t.Fatal("renewal did not schedule expiration")
	}
}
