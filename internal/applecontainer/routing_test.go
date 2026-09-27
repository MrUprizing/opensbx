package applecontainer

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"opensbx/internal/database"
	"opensbx/internal/sandbox"
)

func TestRoutingUsesOneOwnedInventorySnapshotForStateAndPublishedPorts(t *testing.T) {
	const publicID = "sbx-0123456789abcdef0123456789abcdef"
	const nativeID = "opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			t.Fatalf("Routing made a separate/inconsistent inspection request: %v", args)
		}
		_, _ = io.WriteString(out, listJSON(nativeID, "running", `[{"HostPort":39001,"ContainerPort":3000,"Proto":"tcp","Count":1}]`))
		return nil
	}
	client, repo := testClient(t, runner)
	if err := repo.Save(database.Sandbox{ID: publicID, NativeID: nativeID, Name: "demo", Port: "3000/tcp"}); err != nil {
		t.Fatal(err)
	}
	state, err := client.Routing(context.Background(), nativeID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.Network.MainPort != "3000/tcp" || state.Network.PortsMap["3000/tcp"] != "39001" {
		t.Fatalf("single-snapshot route=%+v", state)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("Routing issued %d native inventory calls: %+v", len(runner.calls), runner.calls)
	}
}

func TestRoutingRejectsUnownedNativeIDAndMalformedPublishedPorts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		saveOwnership bool
		ports         string
		wantErr       error
		wantCalls     int
	}{
		{name: "unowned runtime entry", ports: `[]`, wantErr: sandbox.ErrNotFound, wantCalls: 0},
		{name: "malformed published port", saveOwnership: true, ports: `[{"HostPort":70000,"ContainerPort":3000,"Proto":"tcp","Count":1}]`, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const nativeID = "opensbx-ffffffffffffffffffffffffffffffff"
			runner := &scriptedRunner{t: t, run: func(args []string, _ io.Reader, out, _ io.Writer) error {
				if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
					t.Fatalf("unexpected routing command %v", args)
				}
				_, _ = io.WriteString(out, listJSON(nativeID, "running", tc.ports))
				return nil
			}}
			client, repo := testClient(t, runner)
			if tc.saveOwnership {
				if err := repo.Save(database.Sandbox{ID: "sbx-ffffffffffffffffffffffffffffffff", NativeID: nativeID, Name: "owned", Port: "3000/tcp"}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := client.Routing(context.Background(), nativeID)
			if err == nil {
				t.Fatal("invalid or unowned route succeeded")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("route error=%v want %v", err, tc.wantErr)
			}
			if len(runner.calls) != tc.wantCalls {
				t.Fatalf("routing snapshot calls=%v want %d", runner.calls, tc.wantCalls)
			}
		})
	}
}

func TestDiscardCreatedTouchesOnlyExactOwnedRuntimeID(t *testing.T) {
	const nativeID = "opensbx-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const siblingID = "opensbx-cccccccccccccccccccccccccccccccc"
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, `[{"Configuration":{"ID":"`+nativeID+`","Labels":{"io.opensbx.managed":"`+nativeID+`"},"PublishedPorts":[]},"Status":{"State":"stopped"}},{"Configuration":{"ID":"`+siblingID+`","Labels":{"io.opensbx.managed":"`+siblingID+`"},"PublishedPorts":[]},"Status":{"State":"running"}}]`)
		case reflect.DeepEqual(args, []string{"delete", "--force", nativeID}):
		default:
			t.Fatalf("DiscardCreated touched an unexpected resource: %v", args)
		}
		return nil
	}
	client, repo := testClient(t, runner)
	if err := repo.Save(database.Sandbox{ID: "sbx-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", NativeID: nativeID, Name: "created"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(database.Sandbox{ID: siblingID, NativeID: siblingID, Name: "sibling"}); err != nil {
		t.Fatal(err)
	}
	client.scheduleLocked(nativeID, 600)
	if err := client.DiscardCreated(context.Background(), nativeID); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[1].args, []string{"delete", "--force", nativeID}) {
		t.Fatalf("exact compensation command calls=%v", runner.calls)
	}
	if _, ok := client.timers[nativeID]; ok {
		t.Fatal("discard left a live TTL timer")
	}
	if _, err := repo.FindByID("sbx-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatalf("adapter transaction should own DB rollback, lookup error=%v", err)
	}
	if row, err := repo.FindByID(siblingID); err != nil || row == nil {
		t.Fatalf("discard removed unrelated sibling row: row=%+v err=%v", row, err)
	}
}

func TestDiscardCreatedRejectsLabelMismatchAndNeverDeletesUnverifiedResource(t *testing.T) {
	const id = "opensbx-dddddddddddddddddddddddddddddddd"
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			t.Fatalf("unverified rollback issued destructive command: %v", args)
		}
		_, _ = io.WriteString(out, strings.Replace(listJSON(id, "stopped", "[]"), ownerLabel+"\":\""+id, ownerLabel+"\":\"opensbx-wrong-owner", 1))
		return nil
	}
	client, _ := testClient(t, runner)
	if err := client.DiscardCreated(context.Background(), id); err == nil || !strings.Contains(err.Error(), "ownership mismatch") {
		t.Fatalf("label-mismatch discard error=%v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("ownership mismatch attempted extra native operations: %v", runner.calls)
	}
}

func TestDiscardCreatedCancelsOnlyAttachedCommandsAndClearsOwnedTracking(t *testing.T) {
	const nativeID = "opensbx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const publicID = "sbx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(nativeID, "stopped", "[]"))
		case reflect.DeepEqual(args, []string{"delete", "--force", nativeID}):
		default:
			t.Fatalf("unexpected discard argv %v", args)
		}
		return nil
	}
	client, repo := testClient(t, runner)
	if err := repo.Save(database.Sandbox{ID: publicID, NativeID: nativeID, Name: "owned-sandbox"}); err != nil {
		t.Fatal(err)
	}
	client.scheduleLocked(nativeID, 600)
	client.finished[nativeID] = "old-finish"
	process := newControlledProcess()
	client.commands["cmd-attached"] = &runningCommand{sandboxID: nativeID, process: process, done: make(chan struct{})}
	var invalidated []string
	client.SetCacheInvalidator(func(name string) { invalidated = append(invalidated, name) })
	if err := client.DiscardCreated(context.Background(), nativeID); err != nil {
		t.Fatal(err)
	}
	if !process.isKilled() {
		t.Fatal("compensation left attached guest CLI process running")
	}
	if _, ok := client.timers[nativeID]; ok {
		t.Fatal("compensation left expiration timer")
	}
	if _, ok := client.finished[nativeID]; ok {
		t.Fatal("compensation left finished metadata")
	}
	if _, ok := client.commands["cmd-attached"]; ok {
		t.Fatal("compensation left command in ownership map")
	}
	if !reflect.DeepEqual(invalidated, []string{nativeID}) {
		t.Fatalf("compensation invalidated routes for %v", invalidated)
	}
}
