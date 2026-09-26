package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
	"opensbx/internal/database"
	"opensbx/models"
)

type dockerAPIFixture struct {
	mu          sync.Mutex
	fail        map[string]int
	requests    []string
	execOptions []string
	createBody  container.Config
	createHost  container.HostConfig
	containerID string
	running     bool
	paused      bool
	stdin       []byte
	execStdin   bool
	statsBody   string
	pullBody    string
	attachBody  string
	stopDone    chan struct{}
}

func newDockerFixture(t *testing.T) (*Client, *dockerAPIFixture) {
	t.Helper()
	fixture := &dockerAPIFixture{fail: map[string]int{}, containerID: "container-1", running: true, stopDone: make(chan struct{}, 8)}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	dockerHost := strings.TrimPrefix(server.URL, "http://")
	cli, err := moby.NewClientWithOpts(
		moby.WithHost("tcp://"+dockerHost),
		moby.WithVersion("1.53"),
		moby.WithHTTPClient(server.Client()),
	)
	if err != nil {
		t.Fatalf("create Docker SDK client: %v", err)
	}
	db := database.New(":memory:")
	dc := &Client{cli: cli, repo: database.NewRepository(db)}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		dc.Shutdown(ctx)
		_ = cli.Close()
	})
	return dc, fixture
}

func (f *dockerAPIFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1.53")
	key := r.Method + " " + path
	stopRequest := r.Method == http.MethodPost && path == "/containers/container-1/stop"
	defer func() {
		if stopRequest {
			select {
			case f.stopDone <- struct{}{}:
			default:
			}
		}
	}()
	f.mu.Lock()
	f.requests = append(f.requests, key)
	status := f.fail[key]
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"fixture error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && path == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case r.Method == http.MethodGet && path == "/images/json":
		_, _ = w.Write([]byte(`[{"Id":"sha256:image-1","RepoTags":["alpine:latest"],"Size":123}]`))
	case r.Method == http.MethodGet && (path == "/images/alpine/json" || path == "/images/alpine:latest/json"):
		_, _ = w.Write([]byte(`{"Id":"sha256:image-1","RepoTags":["alpine:latest"],"Size":123,"Created":"2026-01-01T00:00:00Z","Architecture":"amd64","Os":"linux"}`))
	case r.Method == http.MethodPost && path == "/containers/create":
		var payload struct {
			container.Config
			HostConfig container.HostConfig `json:"HostConfig"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.createBody, f.createHost = payload.Config, payload.HostConfig
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"Id":%q,"Warnings":[]}`, f.containerID)
	case r.Method == http.MethodGet && path == "/containers/json":
		_, _ = w.Write([]byte(`[{"Id":"container-1","Names":["/demo"],"Image":"alpine:latest","State":"running","Status":"Up","Ports":[{"PrivatePort":3000,"PublicPort":32768,"Type":"tcp"}]}]`))
	case r.Method == http.MethodGet && path == "/containers/container-1/json":
		f.mu.Lock()
		running, paused := f.running, f.paused
		f.mu.Unlock()
		status := "exited"
		if running {
			status = "running"
		}
		_, _ = fmt.Fprintf(w, `{"Id":"container-1","Name":"/demo","Config":{"Image":"alpine:latest"},"State":{"Status":%q,"Running":%t,"Paused":%t,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"},"HostConfig":{"Memory":1073741824,"NanoCpus":1000000000},"NetworkSettings":{"Ports":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"32768"}]}}}`, status, running, paused)
	case r.Method == http.MethodGet && path == "/containers/container-1/archive":
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	case r.Method == http.MethodGet && path == "/containers/container-1/stats":
		body := f.statsBody
		if body == "" {
			body = `{"cpu_stats":{"cpu_usage":{"total_usage":200},"system_cpu_usage":1000,"online_cpus":2},"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":500},"memory_stats":{"usage":50,"limit":100},"pids_stats":{"current":7}}`
		}
		_, _ = w.Write([]byte(body))
	case r.Method == http.MethodPost && path == "/containers/container-1/exec":
		body, _ := io.ReadAll(r.Body)
		var options struct {
			AttachStdin bool `json:"AttachStdin"`
		}
		_ = json.Unmarshal(body, &options)
		f.mu.Lock()
		f.execStdin = options.AttachStdin
		f.execOptions = append(f.execOptions, string(body))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"Id":"exec-1"}`))
	case r.Method == http.MethodPost && path == "/exec/exec-1/start":
		f.serveExecAttach(w, r)
	case r.Method == http.MethodGet && path == "/exec/exec-1/json":
		_, _ = w.Write([]byte(`{"ID":"exec-1","Running":false,"ExitCode":0}`))
	case r.Method == http.MethodPost && path == "/images/create":
		body := f.pullBody
		if body == "" {
			body = "{\"status\":\"done\"}\n"
		}
		_, _ = w.Write([]byte(body))
	case r.Method == http.MethodPost && path == "/containers/container-1/start":
		f.mu.Lock()
		f.running = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && (path == "/containers/container-1/stop" || path == "/containers/container-1/restart"):
		f.mu.Lock()
		f.running = path == "/containers/container-1/restart"
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/containers/container-1/pause":
		f.mu.Lock()
		f.paused = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/containers/container-1/unpause":
		f.mu.Lock()
		f.paused = false
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && path == "/images/alpine:latest":
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodDelete && path == "/containers/container-1":
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"unknown fixture route"}`))
	}
}

func (f *dockerAPIFixture) serveExecAttach(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if r.Header.Get("Upgrade") != "tcp" {
		_, _ = rw.WriteString("HTTP/1.1 400 Bad Request\r\n\r\n")
		_ = rw.Flush()
		return
	}
	f.mu.Lock()
	attachBody := f.attachBody
	f.mu.Unlock()
	f.mu.Lock()
	readStdin := f.execStdin
	f.mu.Unlock()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	_ = rw.Flush()
	if readStdin {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		input, _ := io.ReadAll(rw.Reader)
		f.mu.Lock()
		f.stdin = append(f.stdin, input...)
		f.mu.Unlock()
	}
	stdout := []byte("output\n")
	stderr := []byte("warning\n")
	if attachBody != "" {
		_, _ = rw.WriteString(attachBody)
		_ = rw.Flush()
		return
	}
	for _, stream := range []struct {
		id   byte
		data []byte
	}{{1, stdout}, {2, stderr}} {
		frame := make([]byte, 8+len(stream.data))
		frame[0] = stream.id
		binary.BigEndian.PutUint32(frame[4:8], uint32(len(stream.data)))
		copy(frame[8:], stream.data)
		_, _ = rw.Write(frame)
	}
	_ = rw.Flush()
}

func TestDockerAPIFixtureRejectsUnknownResourceIDsAndMutationPaths(t *testing.T) {
	fixture := &dockerAPIFixture{fail: map[string]int{}, containerID: "container-1", stopDone: make(chan struct{}, 1)}
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1.53/containers/other/exec"},
		{http.MethodPost, "/v1.53/containers/other/start"},
		{http.MethodPost, "/v1.53/containers/other/stop"},
		{http.MethodPost, "/v1.53/containers/other/restart"},
		{http.MethodPost, "/v1.53/containers/other/pause"},
		{http.MethodPost, "/v1.53/containers/other/unpause"},
		{http.MethodPost, "/v1.53/exec/other/start"},
		{http.MethodGet, "/v1.53/exec/other/json"},
		{http.MethodDelete, "/v1.53/containers/other"},
		{http.MethodDelete, "/v1.53/images/other:latest"},
		{http.MethodPost, "/v1.53/images/other/create"},
	} {
		t.Run(tc.method+"_"+strings.TrimPrefix(tc.path, "/v1.53"), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, nil)
			fixture.serveHTTP(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("fixture route %s %s returned %d, want 404", tc.method, tc.path, recorder.Code)
			}
		})
	}
}

func TestDockerClientCreateInspectListAndLifecycle(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	created, err := dc.Create(ctx, models.CreateSandboxRequest{
		Image: "alpine:latest", Ports: []string{"3000"}, Timeout: 600,
		Resources: &models.ResourceLimits{Memory: 512, CPUs: 1.5}, Env: []string{"MODE=test"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if created.ID != "container-1" || created.Name == "" || len(created.Ports) != 1 || created.Ports[0] != "3000/tcp" {
		t.Fatalf("Create() response = %+v", created)
	}
	fixture.mu.Lock()
	if fixture.createBody.Image != "alpine:latest" || fixture.createBody.Env[0] != "MODE=test" {
		t.Errorf("container config did not retain requested image/env: %+v", fixture.createBody)
	}
	if fixture.createHost.Resources.Memory != 512*1024*1024 || fixture.createHost.Resources.NanoCPUs != 1500000000 {
		t.Errorf("resource limits not converted to Docker units: %+v", fixture.createHost.Resources)
	}
	fixture.mu.Unlock()

	detail, err := dc.Inspect(ctx, created.ID)
	if err != nil || !detail.Running || detail.Name != "demo" || detail.Resources.Memory != 1024 || detail.Resources.CPUs != 1 {
		t.Fatalf("Inspect() = %+v, %v", detail, err)
	}
	list, err := dc.List(ctx)
	if err != nil || len(list) != 1 || list[0].State != "running" || list[0].Name != "demo" {
		t.Fatalf("List() = %+v, %v", list, err)
	}
	network, err := dc.GetNetwork(ctx, created.ID)
	if err != nil || network.MainPort != "3000/tcp" || network.PortsMap["3000/tcp"] != "32768" {
		t.Fatalf("GetNetwork() = %+v, %v", network, err)
	}
	fixture.mu.Lock()
	fixture.running = false
	fixture.mu.Unlock()
	started, err := dc.Start(ctx, created.ID)
	if err != nil || started.Status != "started" || len(started.Ports) != 1 || started.ExpiresAt == nil {
		t.Fatalf("Start() = %+v, %v", started, err)
	}
	if err := dc.Stop(ctx, created.ID); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	restarted, err := dc.Restart(ctx, created.ID)
	if err != nil || restarted.Status != "restarted" || restarted.ExpiresAt == nil {
		t.Fatalf("Restart() = %+v, %v", restarted, err)
	}
	if err := dc.Pause(ctx, created.ID); err != nil {
		t.Fatalf("Pause() error: %v", err)
	}
	if err := dc.Resume(ctx, created.ID); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}
	if err := dc.RenewExpiration(ctx, created.ID, 120); err != nil {
		t.Fatalf("RenewExpiration() error: %v", err)
	}
	if err := dc.Remove(ctx, created.ID); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if got, err := dc.repo.FindByID(created.ID); err != nil || got != nil {
		t.Fatalf("sandbox record after Remove() = %v, %v; want nil, nil", got, err)
	}
}

func TestDockerClientEmptyListAndNetworkMainPortFallback(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	items, err := dc.List(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("List() for empty DB = %v, %v", items, err)
	}
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo", Ports: database.JSONMap{"3000/tcp": "32768"}}); err != nil {
		t.Fatal(err)
	}
	fixture.fail["GET /containers/json"] = http.StatusInternalServerError
	if _, err := dc.List(context.Background()); err == nil {
		t.Fatal("List() should surface Docker list errors")
	}
	delete(fixture.fail, "GET /containers/json")
	network, err := dc.GetNetwork(context.Background(), "container-1")
	if err != nil || network.MainPort != "3000/tcp" {
		t.Fatalf("GetNetwork() single-port fallback = %+v, %v", network, err)
	}
}

func TestDockerClientMapsDaemonErrorsAndInvalidImage(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	fixture.fail["GET /images/missing/json"] = http.StatusNotFound
	if _, err := dc.Create(ctx, models.CreateSandboxRequest{Image: "missing"}); err != ErrImageNotFound {
		t.Fatalf("Create() missing image error = %v, want ErrImageNotFound", err)
	}
	fixture.fail["GET /containers/missing/json"] = http.StatusNotFound
	if _, err := dc.Inspect(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("Inspect() not-found error = %v, want ErrNotFound", err)
	}
	if err := dc.Stop(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("Stop() not-found error = %v, want ErrNotFound", err)
	}
	if _, err := dc.GetNetwork(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("GetNetwork() missing DB record error = %v, want ErrNotFound", err)
	}
	fixture.fail["GET /images/broken/json"] = http.StatusInternalServerError
	if _, err := dc.ImageExists(ctx, "broken"); err == nil {
		t.Fatal("ImageExists() should preserve non-not-found daemon errors")
	}
}

func TestDockerClientImageOperations(t *testing.T) {
	dc, _ := newDockerFixture(t)
	ctx := context.Background()
	image, err := dc.InspectImage(ctx, "alpine:latest")
	if err != nil || image.ID != "sha256:image-1" || image.Architecture != "amd64" || image.OS != "linux" {
		t.Fatalf("InspectImage() = %+v, %v", image, err)
	}
	images, err := dc.ListImages(ctx)
	if err != nil || len(images) != 1 || images[0].ID != "sha256:image-1" {
		t.Fatalf("ListImages() = %+v, %v", images, err)
	}
	if exists, err := dc.ImageExists(ctx, "alpine:latest"); err != nil || !exists {
		t.Fatalf("ImageExists() = %v, %v; want true", exists, err)
	}
	if err := dc.RemoveImage(ctx, "alpine:latest", true); err != nil {
		t.Fatalf("RemoveImage() error: %v", err)
	}
}

func TestDockerClientStatsAndCommandPersistence(t *testing.T) {
	dc, _ := newDockerFixture(t)
	ctx := context.Background()
	stats, err := dc.Stats(ctx, "container-1")
	if err != nil || stats.CPU != 40 || stats.Memory.Usage != 50 || stats.Memory.Percent != 50 || stats.PIDs != 7 {
		t.Fatalf("Stats() = %+v, %v", stats, err)
	}
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := dc.repo.SaveCommand(database.Command{ID: "cmd-1", SandboxID: "container-1", Name: "echo", Args: `["hi"]`, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := dc.GetCommand(ctx, "container-1", "cmd-1")
	if err != nil || got.Name != "echo" || len(got.Args) != 1 || got.Args[0] != "hi" {
		t.Fatalf("GetCommand() = %+v, %v", got, err)
	}
	if _, err := dc.GetCommand(ctx, "container-1", "absent"); err != ErrCommandNotFound {
		t.Fatalf("GetCommand() unknown id error = %v", err)
	}
	if _, err := dc.GetCommand(ctx, "other-sandbox", "cmd-1"); err != ErrCommandNotFound {
		t.Fatalf("GetCommand() wrong sandbox error = %v", err)
	}
	commands, err := dc.ListCommands(ctx, "container-1")
	if err != nil || len(commands) != 1 || commands[0].ID != "cmd-1" {
		t.Fatalf("ListCommands() = %+v, %v", commands, err)
	}
	finished, err := dc.WaitCommand(ctx, "container-1", "cmd-1")
	if err != nil || finished.Name != "echo" {
		t.Fatalf("WaitCommand() for already completed record = %+v, %v", finished, err)
	}
	if err := dc.repo.SaveCommand(database.Command{ID: "bad-args", SandboxID: "container-1", Args: "not-json"}); err != nil {
		t.Fatal(err)
	}
	badArgs, err := dc.GetCommand(ctx, "container-1", "bad-args")
	if err != nil || len(badArgs.Args) != 0 {
		t.Fatalf("GetCommand() invalid stored args = %+v, %v", badArgs, err)
	}
}

func TestDockerClientExecWaitLogsAndFileOperations(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	started, err := dc.ExecCommand(ctx, "container-1", models.ExecCommandRequest{Command: "echo", Args: []string{"hello"}, Cwd: "/work", Env: map[string]string{"K": "V"}})
	if err != nil || started.ID == "" || started.Name != "echo" {
		t.Fatalf("ExecCommand() = %+v, %v", started, err)
	}
	completed, err := dc.WaitCommand(ctx, "container-1", started.ID)
	if err != nil || completed.ExitCode == nil || *completed.ExitCode != 0 {
		t.Fatalf("WaitCommand() = %+v, %v", completed, err)
	}
	logs, err := dc.GetCommandLogs(ctx, "container-1", started.ID)
	if err != nil || logs.Stdout != "output\n" || logs.Stderr != "warning\n" {
		t.Fatalf("GetCommandLogs() = %+v, %v", logs, err)
	}
	stdout, stderr, err := dc.StreamCommandLogs(ctx, "container-1", started.ID)
	if err != nil {
		t.Fatalf("StreamCommandLogs() error: %v", err)
	}
	defer stdout.Close()
	defer stderr.Close()
	out, _ := io.ReadAll(stdout)
	errOut, _ := io.ReadAll(stderr)
	if string(out) != "output\n" || string(errOut) != "warning\n" {
		t.Fatalf("stream logs stdout/stderr = %q/%q", out, errOut)
	}

	if content, err := dc.ReadFile(ctx, "container-1", "/work/a.txt"); err != nil || content != "output\n" {
		t.Fatalf("ReadFile() = %q, %v", content, err)
	}
	if err := dc.WriteFile(ctx, "container-1", "/work/a.txt", "written bytes"); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	if err := dc.DeleteFile(ctx, "container-1", "/work/a.txt"); err != nil {
		t.Fatalf("DeleteFile() error: %v", err)
	}
	if output, err := dc.ListDir(ctx, "container-1", "/work"); err != nil || output != "output\n" {
		t.Fatalf("ListDir() = %q, %v", output, err)
	}
	fixture.mu.Lock()
	input := append([]byte(nil), fixture.stdin...)
	fixture.mu.Unlock()
	if !bytes.Contains(input, []byte("written bytes")) {
		fixture.mu.Lock()
		requests := append([]string(nil), fixture.requests...)
		execOptions := append([]string(nil), fixture.execOptions...)
		fixture.mu.Unlock()
		t.Fatalf("Docker exec fixture did not receive file content on stdin: %q; requests: %v; exec options: %v", input, requests, execOptions)
	}
}

func TestDockerClientCommandStateErrorsAndContextCancellation(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	fixture.running = false
	if _, err := dc.ExecCommand(ctx, "container-1", models.ExecCommandRequest{Command: "echo"}); err != ErrNotRunning {
		t.Fatalf("ExecCommand() stopped sandbox error = %v", err)
	}
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := dc.repo.SaveCommand(database.Command{ID: "cmd-1", SandboxID: "container-1", Name: "sleep"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.KillCommand(ctx, "container-1", "absent", 15); err != ErrCommandNotFound {
		t.Fatalf("KillCommand() unknown id error = %v", err)
	}
	if _, err := dc.KillCommand(ctx, "container-1", "cmd-1", 15); err != ErrCommandFinished {
		t.Fatalf("KillCommand() without live process error = %v", err)
	}
	stdout := newRingBuffer(32)
	stderr := newRingBuffer(32)
	stdout.Write([]byte("live"))
	dc.commands.Store("cmd-live", &runningCommand{sandboxID: "container-1", stdout: stdout, stderr: stderr, done: make(chan struct{}), cancel: func() {}})
	if _, _, err := dc.StreamCommandLogs(ctx, "elsewhere", "cmd-live"); err != ErrCommandNotFound {
		t.Fatalf("StreamCommandLogs() wrong sandbox error = %v", err)
	}
	if _, err := dc.GetCommandLogs(ctx, "elsewhere", "cmd-live"); err != ErrCommandNotFound {
		t.Fatalf("GetCommandLogs() wrong sandbox error = %v", err)
	}
	if _, _, err := dc.StreamCommandLogs(ctx, "container-1", "absent"); err != ErrCommandNotFound {
		t.Fatalf("StreamCommandLogs() missing id error = %v", err)
	}
	if _, err := dc.GetCommandLogs(ctx, "container-1", "absent"); err != ErrCommandNotFound {
		t.Fatalf("GetCommandLogs() missing id error = %v", err)
	}
	reader, _, err := dc.StreamCommandLogs(ctx, "container-1", "cmd-live")
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4)
	if n, err := reader.Read(data); err != nil || string(data[:n]) != "live" {
		t.Fatalf("StreamCommandLogs() live data = %q, %v", data[:n], err)
	}
	_ = reader.Close()

	ctxCanceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := dc.WaitCommand(ctxCanceled, "container-1", "cmd-live"); err == nil {
		t.Fatal("WaitCommand() should honor a canceled context")
	}
	command, _ := dc.commands.Load("cmd-live")
	close(command.(*runningCommand).done)
	if _, err := dc.WaitCommand(ctx, "other-sandbox", "cmd-live"); err != ErrCommandNotFound {
		t.Fatalf("WaitCommand() wrong sandbox error = %v", err)
	}

	fixture.mu.Lock()
	fixture.fail["GET /containers/container-1/json"] = http.StatusNotFound
	fixture.mu.Unlock()
	if _, err := dc.ExecCommand(ctx, "container-1", models.ExecCommandRequest{Command: "echo"}); err != ErrNotFound {
		t.Fatalf("ExecCommand() missing sandbox error = %v", err)
	}
}

func TestDockerClientPullImageAndOperationFailures(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	if err := dc.PullImage(ctx, "alpine:latest"); err != nil {
		t.Fatalf("PullImage() error: %v", err)
	}
	fixture.mu.Lock()
	fixture.fail["GET /images/bad/json"] = http.StatusInternalServerError
	fixture.fail["GET /containers/container-1/stats"] = http.StatusInternalServerError
	fixture.fail["POST /containers/container-1/start"] = http.StatusInternalServerError
	fixture.fail["DELETE /images/alpine:latest"] = http.StatusNotFound
	fixture.mu.Unlock()
	if err := dc.PullImage(ctx, "bad"); err == nil {
		t.Fatal("PullImage() should propagate local image verification failures")
	}
	if _, err := dc.Stats(ctx, "container-1"); err == nil {
		t.Fatal("Stats() should propagate Docker API errors")
	}
	if _, err := dc.Start(ctx, "container-1"); err == nil {
		t.Fatal("Start() should propagate Docker API errors")
	}
	if err := dc.RemoveImage(ctx, "alpine:latest", false); err != ErrNotFound {
		t.Fatalf("RemoveImage() not-found error = %v", err)
	}
}

func TestDockerClientShutdownCancelsTimersAndCommands(t *testing.T) {
	dc, _ := newDockerFixture(t)
	dc.scheduleStop("container-1", 60)
	commandCanceled := false
	dc.commands.Store("cmd-shutdown", &runningCommand{sandboxID: "container-1", cancel: func() { commandCanceled = true }})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dc.Shutdown(ctx)
	if dc.getTimerEntry("container-1") != nil {
		t.Fatal("Shutdown() should remove scheduled timers")
	}
	if !commandCanceled {
		t.Fatal("Shutdown() should cancel running commands")
	}
}

func TestDockerClientPingCacheInvalidationAndStateConflicts(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	if err := dc.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}
	var invalidated []string
	dc.SetCacheInvalidator(func(name string) { invalidated = append(invalidated, name) })
	dc.invalidateCache("missing")
	if len(invalidated) != 0 {
		t.Fatalf("invalidateCache() notified for an unknown id: %v", invalidated)
	}
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	dc.invalidateCache("container-1")
	if len(invalidated) != 1 || invalidated[0] != "demo" {
		t.Fatalf("invalidateCache() names = %v, want [demo]", invalidated)
	}
	if _, err := dc.Start(context.Background(), "container-1"); err != ErrAlreadyRunning {
		t.Fatalf("Start() running sandbox error = %v", err)
	}
	if err := dc.Stop(context.Background(), "container-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if err := dc.Stop(context.Background(), "container-1"); err != ErrAlreadyStopped {
		t.Fatalf("Stop() stopped sandbox error = %v", err)
	}
	if err := dc.Pause(context.Background(), "container-1"); err != ErrNotRunning {
		t.Fatalf("Pause() stopped sandbox error = %v", err)
	}
	fixture.mu.Lock()
	fixture.running, fixture.paused = true, true
	fixture.mu.Unlock()
	if err := dc.Pause(context.Background(), "container-1"); err != ErrAlreadyPaused {
		t.Fatalf("Pause() already-paused error = %v", err)
	}
	if err := dc.Resume(context.Background(), "container-1"); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}
	if err := dc.Resume(context.Background(), "container-1"); err != ErrNotPaused {
		t.Fatalf("Resume() not-paused error = %v", err)
	}
	if err := dc.RenewExpiration(context.Background(), "container-1", 45); err != nil {
		t.Fatalf("RenewExpiration() error: %v", err)
	}
	if dc.getTimerEntry("container-1") == nil {
		t.Fatal("RenewExpiration() did not install a timer")
	}
}

func TestDockerClientCreateAndLifecycleSurfaceDockerFailures(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	fixture.mu.Lock()
	fixture.fail["GET /images/broken/json"] = http.StatusInternalServerError
	fixture.fail["POST /containers/create"] = http.StatusInternalServerError
	fixture.fail["POST /containers/container-1/start"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	if _, err := dc.Create(ctx, models.CreateSandboxRequest{Image: "broken"}); err == nil {
		t.Fatal("Create() should return image-inspect failures")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "GET /images/broken/json")
	fixture.mu.Unlock()
	if _, err := dc.Create(ctx, models.CreateSandboxRequest{Image: "alpine"}); err == nil {
		t.Fatal("Create() should return container-create failures")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "POST /containers/create")
	fixture.mu.Unlock()
	if _, err := dc.Create(ctx, models.CreateSandboxRequest{Image: "alpine"}); err == nil {
		t.Fatal("Create() should return container-start failures")
	}

	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.fail["POST /containers/container-1/stop"] = http.StatusInternalServerError
	fixture.fail["POST /containers/container-1/pause"] = http.StatusInternalServerError
	fixture.fail["POST /containers/container-1/unpause"] = http.StatusInternalServerError
	fixture.fail["POST /containers/container-1/restart"] = http.StatusInternalServerError
	fixture.fail["GET /containers/container-1/json"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	if err := dc.Stop(ctx, "container-1"); err == nil {
		t.Fatal("Stop() should propagate daemon failures")
	}
	if err := dc.Pause(ctx, "container-1"); err == nil {
		t.Fatal("Pause() should propagate daemon failures")
	}
	if err := dc.Resume(ctx, "container-1"); err == nil {
		t.Fatal("Resume() should propagate daemon failures")
	}
	if _, err := dc.Restart(ctx, "container-1"); err == nil {
		t.Fatal("Restart() should propagate daemon failures")
	}
	if err := dc.RenewExpiration(ctx, "container-1", 5); err == nil {
		t.Fatal("RenewExpiration() should propagate inspect failures")
	}
}

func TestDockerClientRemoveAndImageInspectionErrorBranches(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.fail["DELETE /containers/container-1"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	if err := dc.Remove(ctx, "container-1"); err == nil {
		t.Fatal("Remove() should return non-not-found daemon errors")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "DELETE /containers/container-1")
	fixture.fail["DELETE /containers/container-1"] = http.StatusNotFound
	fixture.fail["GET /images/missing/json"] = http.StatusNotFound
	fixture.fail["GET /images/broken/json"] = http.StatusInternalServerError
	fixture.fail["GET /images/json"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	if err := dc.Remove(ctx, "container-1"); err != nil {
		t.Fatalf("Remove() should treat Docker not-found as already removed: %v", err)
	}
	if _, err := dc.InspectImage(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("InspectImage() not-found error = %v", err)
	}
	if _, err := dc.InspectImage(ctx, "broken"); err == nil {
		t.Fatal("InspectImage() should preserve non-not-found errors")
	}
	if _, err := dc.ListImages(ctx); err == nil {
		t.Fatal("ListImages() should surface Docker errors")
	}
}

func TestDockerClientStatsAndExecErrorBranches(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	fixture.mu.Lock()
	fixture.statsBody = "not-json"
	fixture.mu.Unlock()
	if _, err := dc.Stats(ctx, "container-1"); err == nil || !strings.Contains(err.Error(), "decode stats") {
		t.Fatalf("Stats() invalid JSON error = %v", err)
	}
	fixture.mu.Lock()
	fixture.statsBody = ""
	fixture.running = true
	fixture.fail["POST /containers/container-1/exec"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	if _, err := dc.ExecCommand(ctx, "container-1", models.ExecCommandRequest{Command: "echo"}); err == nil {
		t.Fatal("ExecCommand() should surface exec-create failures")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "POST /containers/container-1/exec")
	fixture.fail["POST /exec/exec-1/start"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	command, err := dc.ExecCommand(ctx, "container-1", models.ExecCommandRequest{Command: "echo"})
	if err != nil {
		t.Fatalf("ExecCommand() should return a command even if attach fails asynchronously: %v", err)
	}
	finished, err := dc.WaitCommand(ctx, "container-1", command.ID)
	if err != nil || finished.ExitCode == nil || *finished.ExitCode != -1 {
		t.Fatalf("WaitCommand() after attach failure = %+v, %v", finished, err)
	}
	if err := dc.WriteFile(ctx, "container-1", "/x", "y"); err == nil {
		t.Fatal("WriteFile() should return exec-create failures")
	}
}

func TestDockerClientKillRunningCommandAndPullStreamErrors(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := dc.repo.SaveCommand(database.Command{ID: "cmd-live", SandboxID: "container-1", Name: "sleep", Args: `["30"]`}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := newRingBuffer(32), newRingBuffer(32)
	dc.commands.Store("cmd-live", &runningCommand{execID: "exec-1", sandboxID: "container-1", cmd: []string{"sleep", "30"}, cancel: func() {}, stdout: stdout, stderr: stderr, done: make(chan struct{})})
	if _, err := dc.KillCommand(ctx, "container-1", "cmd-live", 15); err != nil {
		t.Fatalf("KillCommand() running command error: %v", err)
	}
	fixture.mu.Lock()
	fixture.pullBody = "{\"errorDetail\":{\"message\":\"registry denied\"}}\n"
	fixture.mu.Unlock()
	if err := dc.PullImage(ctx, "private/image"); err == nil || !strings.Contains(err.Error(), "registry denied") {
		t.Fatalf("PullImage() inline registry error = %v", err)
	}
	fixture.mu.Lock()
	fixture.pullBody = "invalid-json\n"
	fixture.mu.Unlock()
	if err := dc.PullImage(ctx, "invalid-stream"); err == nil {
		t.Fatal("PullImage() should return malformed stream errors")
	}
}

func TestDockerClientTimerExpiryStopsContainer(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	dc.scheduleStop("container-1", -1)
	select {
	case <-fixture.stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the timer-triggered Docker stop request to complete")
	}
	if dc.getTimerEntry("container-1") != nil {
		t.Fatal("expired timer remained in tracking map after stop request")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !containsString(fixture.requests, "POST /containers/container-1/stop") {
		t.Fatalf("timer expiration did not stop container; requests: %v", fixture.requests)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
