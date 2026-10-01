package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

type call struct {
	args  []string
	input string
}

// scriptedRunner rejects every command unless the test explicitly handles its exact argv.
type scriptedRunner struct {
	t       *testing.T
	calls   []call
	run     func([]string, io.Reader, io.Writer, io.Writer) error
	startFn func([]string, io.Reader, io.Writer, io.Writer) (Process, error)
}

func (r *scriptedRunner) Run(_ context.Context, args []string, in io.Reader, out, stderr io.Writer) error {
	var b []byte
	if in != nil {
		b, _ = io.ReadAll(in)
	}
	r.calls = append(r.calls, call{append([]string(nil), args...), string(b)})
	if r.run == nil {
		r.t.Fatalf("unexpected CLI argv: %#v", args)
	}
	return r.run(args, strings.NewReader(string(b)), out, stderr)
}
func (r *scriptedRunner) Start(args []string, in io.Reader, out, stderr io.Writer) (Process, error) {
	r.calls = append(r.calls, call{append([]string(nil), args...), ""})
	if r.startFn == nil {
		r.t.Fatalf("unexpected CLI Start argv: %#v", args)
	}
	return r.startFn(args, in, out, stderr)
}

func testClient(t *testing.T, runner Runner) (*Client, *database.Repository) {
	t.Helper()
	db := database.New(t.TempDir() + "/sandbox.db")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	return New(repo, runner, time.Now), repo
}

func listJSON(id, state string, ports string) string {
	return `[{"Configuration":{"ID":"` + id + `","Labels":{"io.opensbx.managed":"` + id + `"},"Image":{"Reference":"node:24"},"Resources":{"CPUs":1,"MemoryInBytes":1073741824},"PublishedPorts":` + ports + `},"Status":{"State":"` + state + `","StartedDate":"2026-09-26T00:00:00Z"}}]`
}

func TestCreatePinsLocalImageUsesWholeCPUAndRollsBackNothingOnValidation(t *testing.T) {
	r := &scriptedRunner{t: t}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}):
			_, _ = io.WriteString(out, `[{"ID":"sha256:img","Configuration":{"Name":"node:24","Descriptor":{"Digest":"sha256:img"}},"Variants":[{"Platform":{"Architecture":"arm64","OS":"linux"},"Size":55}]}]`)
		case len(args) >= 1 && args[0] == "create":
			if args[len(args)-3] != "node:24" || args[len(args)-2] != "-c" || args[len(args)-1] != "exec sleep infinity" {
				t.Errorf("create argv tail = %#v", args)
			}
			if args[0] != "create" || !reflect.DeepEqual(args[1:4], []string{"--max-concurrent-downloads", "0", "--name"}) || !validID(args[4]) {
				t.Errorf("unowned create ID in argv %#v", args)
			}
			for i := 0; i < len(args); i++ {
				if args[i] == "--cpus" && args[i+1] != "2" {
					t.Errorf("cpus argv = %#v", args)
				}
			}
		case len(args) == 2 && args[0] == "start" && validID(args[1]):
		default:
			t.Fatalf("unrecognized CLI argv: %#v", args)
		}
		return nil
	}
	c, _ := testClient(t, r)
	_, err := c.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: "node:24", Resources: &runtimeio.ResourceLimits{CPUs: 2, Memory: 512}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("CLI calls = %d, want local inspect/create/start", len(r.calls))
	}
	before := len(r.calls)
	for _, cpu := range []float64{1.5, 0.5, 5, -1} {
		_, err := c.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: "node:24", Resources: &runtimeio.ResourceLimits{CPUs: cpu}})
		if err == nil {
			t.Errorf("fractional/out-of-range CPU %v accepted", cpu)
		}
	}
	if len(r.calls) != before {
		t.Fatal("invalid CPU request reached Apple CLI")
	}
}

func TestCreateRejectsImageMissAndHostileInputBeforeMutation(t *testing.T) {
	r := &scriptedRunner{t: t}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}) {
			t.Fatalf("unexpected CLI call for invalid input: %#v", args)
		}
		_, _ = io.WriteString(out, `[]`)
		return nil
	}
	c, _ := testClient(t, r)
	for _, req := range []runtimeio.CreateSandboxRequest{
		{Image: "-danger"}, {Image: "missing:latest"}, {Image: "node:24", Env: []string{"SECRET"}},
		{Image: "node:24", Env: []string{"BAD KEY=value"}}, {Image: "node:24", Ports: []string{"1/tcp"}},
	} {
		_, err := c.Create(context.Background(), req)
		if err == nil {
			t.Errorf("invalid or absent local image accepted: %+v", req)
		}
	}
	if len(r.calls) != 1 {
		t.Fatalf("invalid requests ran mutation commands: %#v", r.calls)
	}
}

func TestResourceArgumentValidationAndLoopbackPortReservations(t *testing.T) {
	ports, err := normalizePorts([]string{"3000", "3000/tcp", "53/udp"})
	if err != nil || !reflect.DeepEqual(ports, []string{"3000/tcp", "53/udp"}) {
		t.Fatalf("normalized ports = %v, %v", ports, err)
	}
	for _, bad := range [][]string{{"1/tcp"}, {"70000/tcp"}, {"80/sctp"}, {"80/tcp/extra"}, {" 80/tcp"}} {
		if _, err := normalizePorts(bad); err == nil {
			t.Errorf("invalid ports %v accepted", bad)
		}
	}
	args, err := envArgs([]string{"A=one=two", "_B=value"})
	if err != nil || !reflect.DeepEqual(args, []string{"--env", "A=one=two", "--env", "_B=value"}) {
		t.Fatalf("env argv = %v, %v", args, err)
	}
	for _, bad := range []string{"NAME", "=value", "1BAD=x", "A=x\x00y"} {
		if _, err := envArgs([]string{bad}); err == nil {
			t.Errorf("invalid environment %q accepted", bad)
		}
	}
	for _, invalid := range []string{"", "name with space", "A\nB"} {
		if validEnv(invalid, "v") {
			t.Errorf("invalid environment key %q accepted", invalid)
		}
	}
	mapping, release, err := reservePorts([]string{"3000/tcp", "53/udp"})
	if err != nil {
		t.Fatal(err)
	}
	if mapping["3000/tcp"] == "" || mapping["53/udp"] == "" || mapping["3000/tcp"] == mapping["53/udp"] {
		t.Fatalf("invalid loopback reservations: %v", mapping)
	}
	release()
}

func TestPortBoundsAndDuplicateLimitsFailClosed(t *testing.T) {
	if _, err := normalizePorts(make([]string, 129)); err == nil {
		t.Fatal("more than 128 published ports accepted")
	}
	for _, port := range []struct {
		host, container int
		proto           string
		count           int
	}{
		{1, 8080, "tcp", 1}, {65536, 8080, "tcp", 1}, {1234, 1, "tcp", 1}, {1234, 8080, "sctp", 1}, {1234, 8080, "tcp", 2},
	} {
		info := containerInfo{}
		info.Configuration.PublishedPorts = []struct {
			HostPort      int
			ContainerPort int
			Proto         string
			Count         int
		}{{HostPort: port.host, ContainerPort: port.container, Proto: port.proto, Count: port.count}}
		if _, err := networkPorts(info); err == nil {
			t.Errorf("invalid native port configuration accepted: %+v", port)
		}
	}
}

func TestDateStringAcceptsNativeTextAndFoundationEpochValues(t *testing.T) {
	var textual json.RawMessage = []byte(`"2026-09-26T12:00:00Z"`)
	if got := dateString(textual); got != "2026-09-26T12:00:00Z" {
		t.Fatalf("text date = %q", got)
	}
	var numeric json.RawMessage = []byte(`0`)
	if got := dateString(numeric); got != "2001-01-01T00:00:00Z" {
		t.Fatalf("Foundation epoch date = %q", got)
	}
	var invalid json.RawMessage = []byte(`null`)
	if got := dateString(invalid); got != "" {
		t.Fatalf("invalid date = %q, want empty", got)
	}
}

func TestFileOperationsUseLiteralGuestArgumentsAndPropagateFailures(t *testing.T) {
	id := "opensbx-0123456789abcdef0123456789abcdef"
	r := &scriptedRunner{t: t}
	var listed = listJSON(id, "running", "[]")
	r.run = func(args []string, in io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listed)
		case len(args) == 8 && args[0] == "exec" && args[1] == "--interactive" && args[2] == id && args[3] == "/bin/sh" && args[4] == "-c" && args[6] == "opensbx-file" && strings.HasSuffix(args[5], `cat > "$1"`):
			if args[7] != "-strange;$(touch nope)" {
				t.Errorf("path was not passed as a positional argument: %#v", args)
			}
			if args[5] != `case "$1" in /*) ;; *) set -- "./$1";; esac; mkdir -p "$(dirname "$1")" && cat > "$1"` {
				t.Errorf("unexpected fixed guest script %q", args[5])
			}
			b, _ := io.ReadAll(in)
			if string(b) != "" {
				t.Errorf("empty write sent %q", b)
			}
		case len(args) == 8 && args[0] == "exec" && args[1] == "--interactive" && args[2] == id && args[3] == "/bin/sh" && args[4] == "-c" && args[6] == "opensbx-file" && args[5] == `case "$1" in /*) ;; *) set -- "./$1";; esac; exec cat "$1"`:
			_, _ = io.WriteString(out, "file data")
		default:
			t.Fatalf("unexpected CLI argv: %#v", args)
		}
		return nil
	}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id, Image: "node:24"}); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteFile(context.Background(), id, "-strange;$(touch nope)", ""); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadFile(context.Background(), id, "-strange;$(touch nope)")
	if err != nil || got != "file data" {
		t.Fatalf("ReadFile() = %q, %v", got, err)
	}
	if err := c.WriteFile(context.Background(), id, "x", strings.Repeat("x", outputLimit+1)); err == nil {
		t.Fatal("oversize file accepted")
	}
}

func TestLookupEnforcesRepositoryOwnershipAndExactCLIID(t *testing.T) {
	id := "opensbx-0123456789abcdef0123456789abcdef"
	r := &scriptedRunner{t: t}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			t.Fatalf("unexpected lookup argv %#v", args)
		}
		_, _ = io.WriteString(out, `[ {"Configuration":{"ID":"other","Labels":{"io.opensbx.managed":"other"},"Resources":{"CPUs":1,"MemoryInBytes":100}},"Status":{"State":"running"}} ]`)
		return nil
	}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Inspect(context.Background(), id); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("unlisted owned ID error = %v, want not found", err)
	}
	if _, err := c.Inspect(context.Background(), "../../etc/passwd"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("invalid ID error = %v", err)
	}
}

func TestAppleHealthRequiresExactServiceAndVersionSchema(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		wantErr       bool
	}{
		{"valid", `{"status":"running","client":{"version":"1.4.1"},"server":{"version":"1.4.1"}}`, false},
		{"stopped", `{"status":"stopped","client":{"version":"1.4.1"},"server":{"version":"1.4.1"}}`, true},
		{"wrong version", `{"status":"running","client":{"version":"1.4.0"},"server":{"version":"1.4.1"}}`, true},
		{"malformed", `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedRunner{t: t, run: func(args []string, _ io.Reader, out, _ io.Writer) error {
				if !reflect.DeepEqual(args, []string{"system", "status", "--format", "json"}) {
					t.Fatalf("unexpected health argv %#v", args)
				}
				_, _ = io.WriteString(out, tc.payload)
				return nil
			}}
			c, _ := testClient(t, r)
			err := c.Ping(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Ping() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.name == "stopped" && (err == nil || !strings.Contains(err.Error(), "container system start")) {
				t.Fatalf("stopped-service error = %v, want a container system start instruction", err)
			}
			if tc.name == "wrong version" && (err == nil || !strings.Contains(err.Error(), "https://github.com/apple/container/releases")) {
				t.Fatalf("wrong-version error = %v, want official upgrade guidance", err)
			}
		})
	}
}

func TestImageReferenceNormalizationAndArchitectureSelection(t *testing.T) {
	ref, err := imageRef("docker.io/library/node")
	if err != nil || ref != "docker.io/library/node:latest" {
		t.Fatalf("normalized image = %q, %v", ref, err)
	}
	for _, bad := range []string{"", "-option", "node\nlatest", "node\x00tag", "registry::malformed"} {
		if _, err := imageRef(bad); err == nil {
			t.Errorf("invalid image reference %q accepted", bad)
		}
	}
	var im imageInfo
	_ = json.Unmarshal([]byte(`{"Variants":[{"Platform":{"Architecture":"amd64","OS":"linux"}},{"Platform":{"Architecture":"arm64","OS":"linux"}}]}`), &im)
	v, err := nativeVariant(im)
	if err != nil || v.Platform.Architecture != "arm64" {
		t.Fatalf("native variant = %+v, %v", v, err)
	}
}

func TestClientUsesInjectedClockForDeterministicLifecycleTiming(t *testing.T) {
	clock := time.Date(2026, time.September, 26, 12, 30, 0, 0, time.UTC)
	_, repo := testClient(t, &scriptedRunner{t: t})
	client := New(repo, &scriptedRunner{t: t}, func() time.Time { return clock })
	if got := client.now(); !got.Equal(clock) {
		t.Fatalf("injected clock=%s want %s", got, clock)
	}
}

func TestReservePortsAllocatesAndReleasesLoopbackTCPAndUDPLeases(t *testing.T) {
	ports, release, err := reservePorts([]string{"3000/tcp", "53/udp"})
	if err != nil {
		t.Fatal(err)
	}
	for _, guest := range []string{"3000/tcp", "53/udp"} {
		port := ports[guest]
		if port == "" {
			t.Fatalf("no host lease for %s: %v", guest, ports)
		}
		if _, err := net.LookupPort("tcp", port); err != nil {
			t.Errorf("non-numeric host lease for %s: %q", guest, port)
		}
	}
	release()
}

func TestNativeImageInventoryPropagatesCLIFailure(t *testing.T) {
	runner := &scriptedRunner{t: t}
	runner.run = func([]string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("image inventory unavailable")
	}
	client, _ := testClient(t, runner)
	if _, err := client.images(context.Background()); err == nil || !strings.Contains(err.Error(), "CLI execution failed") || strings.Contains(err.Error(), "inventory unavailable") {
		t.Fatalf("image inventory error leaked native diagnostics or lost failure status: %v", err)
	}
}

func TestNativeImageInventoryRejectsMalformedIncompleteAndAmbiguousRows(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		ref           string
		want          string
	}{
		{name: "invalid JSON", payload: `not-json`, ref: "node:latest", want: "invalid Apple image list JSON"},
		{name: "incomplete record", payload: `[{}]`, ref: "node:latest", want: "incomplete Apple image JSON"},
		{name: "ambiguous digest alias", payload: `[{"ID":"sha256:same","Configuration":{"Name":"node:one","Descriptor":{"Digest":"sha256:same"}}},{"ID":"sha256:same","Configuration":{"Name":"node:two","Descriptor":{"Digest":"sha256:same"}}}]`, ref: "sha256:same", want: "multiple references"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{t: t}
			runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
				if !reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}) {
					t.Fatalf("unexpected inventory command %v", args)
				}
				_, _ = io.WriteString(out, tc.payload)
				return nil
			}
			client, _ := testClient(t, runner)
			if _, err := client.findImage(context.Background(), tc.ref); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("findImage error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestNativeVariantRejectsPlatformsOutsideLinuxArm64(t *testing.T) {
	var image imageInfo
	_ = json.Unmarshal([]byte(`{"Variants":[{"Platform":{"Architecture":"amd64","OS":"linux"}},{"Platform":{"Architecture":"arm64","OS":"darwin"}}]}`), &image)
	if _, err := nativeVariant(image); err == nil || !strings.Contains(err.Error(), "no locally available linux/arm64 variant") {
		t.Fatalf("nativeVariant wrong-platform error=%v", err)
	}
}

func TestCreateFailureDeletesOnlyTheNewOwnedContainer(t *testing.T) {
	r := &scriptedRunner{t: t}
	created := make([]string, 0, 3)
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}):
			_, _ = io.WriteString(out, `[{"ID":"sha256:img","Configuration":{"Name":"node:24","Descriptor":{"Digest":"sha256:img"}},"Variants":[{"Platform":{"Architecture":"arm64","OS":"linux"},"Size":55}]}]`)
		case len(args) == 18 && args[0] == "create":
			id := args[4]
			if !validID(id) || !reflect.DeepEqual(args[1:4], []string{"--max-concurrent-downloads", "0", "--name"}) {
				t.Fatalf("create did not use a generated ID: %#v", args)
			}
			created = append(created, id)
		case len(args) == 2 && args[0] == "start":
			if len(created) == 0 || args[1] != created[len(created)-1] {
				t.Fatalf("start targeted unrelated resource: %#v", args)
			}
			return errors.New("simulated start failure")
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			id := created[len(created)-1]
			_, _ = io.WriteString(out, listJSON(id, "stopped", "[]"))
		case len(args) == 3 && reflect.DeepEqual(args[:2], []string{"delete", "--force"}):
			if args[2] != created[len(created)-1] {
				t.Fatalf("rollback targeted unrelated container: %#v", args)
			}
		default:
			t.Fatalf("unexpected create/rollback argv: %#v", args)
		}
		return nil
	}
	c, _ := testClient(t, r)
	if _, err := c.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: "node:24"}); err == nil {
		t.Fatal("create unexpectedly succeeded after start failures")
	}
	if len(created) != 3 {
		t.Fatalf("create retries = %d, want exactly 3", len(created))
	}
	for _, id := range created {
		if !validID(id) {
			t.Errorf("invalid generated ID %q", id)
		}
	}
}

func TestCreatePinsCanonicalOpenSBXCacheReferenceWithoutNativePull(t *testing.T) {
	const ref = "opensbx.invalid/cache@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	runner := &scriptedRunner{t: t}
	var nativeID string
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}):
			_, _ = io.WriteString(out, `[{"ID":"sha256:native-index","Configuration":{"Name":"`+ref+`","Descriptor":{"Digest":"sha256:native-index"}},"Variants":[{"Platform":{"Architecture":"arm64","OS":"linux"},"Size":1}]}]`)
		case len(args) == 18 && args[0] == "create":
			if !reflect.DeepEqual(args[1:4], []string{"--max-concurrent-downloads", "0", "--name"}) {
				t.Fatalf("create download guard/identity args=%v", args)
			}
			nativeID = args[4]
			if !validID(nativeID) || args[len(args)-3] != ref {
				t.Fatalf("create did not pin exact cache reference: %#v", args)
			}
		case len(args) == 2 && args[0] == "start" && args[1] == nativeID:
		default:
			t.Fatalf("unexpected native command; image pull must not be invoked: %#v", args)
		}
		return nil
	}
	client, _ := testClient(t, runner)
	created, err := client.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: ref})
	if err != nil {
		t.Fatal(err)
	}
	if nativeID == "" || created.ID != nativeID {
		t.Fatalf("Create response=%+v native ID=%q", created, nativeID)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("native calls=%v, want inventory/create/start only", runner.calls)
	}
}

func TestCreateClassifiesMissingVminitOfflineAndDoesNotRetry(t *testing.T) {
	const imageRef = "opensbx.invalid/cache@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const diagnostic = "Error: maximum number of concurrent downloads must be greater than 0, got 0"
	runner := &scriptedRunner{t: t}
	createAttempts := 0
	runner.run = func(args []string, _ io.Reader, out, stderr io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}):
			_, _ = io.WriteString(out, `[{"ID":"sha256:native","Configuration":{"Name":"`+imageRef+`","Descriptor":{"Digest":"sha256:native"}},"Variants":[{"Platform":{"Architecture":"arm64","OS":"linux"},"Size":1}]}]`)
		case len(args) == 18 && args[0] == "create":
			createAttempts++
			_, _ = io.WriteString(stderr, diagnostic)
			return exitStatusError(1)
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, `[]`)
		default:
			t.Fatalf("unexpected CLI command after offline classification: %#v", args)
		}
		return nil
	}
	client, _ := testClient(t, runner)
	_, err := client.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: imageRef})
	if !errors.Is(err, errOfflineImageUnavailable) {
		t.Fatalf("offline sentinel=%v want %v", err, errOfflineImageUnavailable)
	}
	if strings.Contains(err.Error(), diagnostic) {
		t.Fatalf("native diagnostic leaked through safe offline error: %v", err)
	}
	if createAttempts != 1 {
		t.Fatalf("offline create attempts=%d want single attempt", createAttempts)
	}
}
