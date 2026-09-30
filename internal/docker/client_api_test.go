package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
	"opensbx/internal/database"
	"opensbx/internal/images"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

type dockerAPIFixture struct {
	mu                sync.Mutex
	t                 *testing.T
	fail              map[string]int
	failAfterMutation map[string]int
	// Sequential responses allow a preflight inspect to succeed and the
	// post-mutation inspect to fail without changing state from a test goroutine.
	responses             map[string][]int
	requests              []string
	execOptions           []string
	execCreates           []dockerExecCreate
	execRecords           map[string]dockerExecRecord
	execCount             int
	containerEnv          []string
	guestProcesses        map[int]fakeGuestProcess
	signalTargets         []int
	signalHelperStdout    string
	signalHelperStderr    string
	probeExitCode         int
	probeStdout           string
	probeStderr           string
	probeCreateStatus     int
	probeCreateError      string
	probeStartStatus      int
	probeBlock            bool
	probeAttachEntered    chan struct{}
	payloadPreamble       string
	signalNoAck           bool
	createBody            container.Config
	createHost            container.HostConfig
	containerID           string
	containerLabels       map[string]string
	containerHostPort     string
	running               bool
	paused                bool
	stdin                 []byte
	execStdin             bool
	statsBody             string
	pullBody              string
	attachBody            string
	stopDone              chan struct{}
	blockStopRequests     bool
	stopEntered           chan struct{}
	cacheDigest           string
	cacheTag              string
	cacheNativeID         string
	cacheLoaded           bool
	loadedArchive         []byte
	cacheSavedArchive     []byte
	cacheLoadBody         string
	cacheInspectID        string
	cacheSaveMode         string
	cacheSaveEntered      chan struct{}
	cacheSaveRelease      chan struct{}
	execStartup           *execStartupFixture
	execCreateEntered     chan struct{}
	releaseExecCreate     chan struct{}
	execCreateBarrierUsed bool
}

type dockerExecCreate struct {
	ID         string
	Kind       string
	Cmd        []string
	Env        []string
	WorkingDir string
}

type dockerExecRecord struct {
	Kind     string
	Nonce    string
	ExitCode int
	Cmd      []string
}

type fakeGuestProcess struct {
	Start   uint64
	Running bool
}

type execStartupFixture struct {
	payloadStartEntered          chan struct{}
	releasePayloadStart          chan struct{}
	payloadStarted               chan struct{}
	payloadStartFailed           chan struct{}
	finishPayload                chan struct{}
	startEnteredOnce             sync.Once
	startedOnce                  sync.Once
	startFailedOnce              sync.Once
	finishOnce                   sync.Once
	identityEnteredOnce          sync.Once
	releaseIdentityOnce          sync.Once
	signalAttachOnce             sync.Once
	releaseStartOnce             sync.Once
	runningReleaseOnce           sync.Once
	pidReleaseOnce               sync.Once
	releaseSignalOnce            sync.Once
	payloadStartFailure          bool
	payloadWaitForRunningRelease bool
	payloadFinishOnStart         bool
	blockSignalAttach            bool
	payloadWaitForPIDRelease     bool
	payloadRunning               bool
	payloadWasStarted            bool
	payloadPID                   int
	payloadExitCode              int
	payloadInspectCalls          int
	signalExecExitCode           int
	signalExecCreates            int
	prematureSignals             int
	signalAttachEntered          chan struct{}
	releaseSignalAttach          chan struct{}
	payloadRunningReady          chan struct{}
	releasePayloadRunning        chan struct{}
	payloadPIDReady              chan struct{}
	releasePayloadPID            chan struct{}
	payloadPreamble              string
	payloadPreambleChunks        [][]byte
	payloadStderrTail            []byte
	guestPID                     int
	guestStart                   uint64
	blockPayloadIdentity         bool
	identityEntered              chan struct{}
	releaseIdentity              chan struct{}
}

func newDockerFixture(t *testing.T) (*Client, *dockerAPIFixture) {
	t.Helper()
	fixture := &dockerAPIFixture{t: t, fail: map[string]int{}, containerID: "container-1", running: true, stopDone: make(chan struct{}, 8), execRecords: make(map[string]dockerExecRecord), guestProcesses: make(map[int]fakeGuestProcess)}
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

func writeTruncatedImageSave(t *testing.T, w http.ResponseWriter, body []byte, declared int64) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("Docker fixture response writer does not support hijacking")
		return
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("hijack Docker image-save response: %v", err)
		return
	}
	_, _ = fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/x-tar\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", declared)
	_, _ = buffered.Write(body)
	_ = buffered.Flush()
	_ = connection.Close()
}

func writeBlockedImageSave(t *testing.T, w http.ResponseWriter, body []byte, entered, release chan struct{}) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("Docker fixture response writer does not support hijacking")
		return
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("hijack blocked Docker image-save response: %v", err)
		return
	}
	_, _ = fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/x-tar\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
	cut := len(body) / 2
	_, _ = buffered.Write(body[:cut])
	_ = buffered.Flush()
	if entered != nil {
		close(entered)
	}
	if release != nil {
		<-release
	}
	_, _ = buffered.Write(body[cut:])
	_ = buffered.Flush()
	_ = connection.Close()
}

func classifyDockerFixtureExec(cmd []string) string {
	if len(cmd) >= 4 && cmd[0] == "/bin/sh" && cmd[1] == "-c" {
		switch cmd[3] {
		case "opensbx-probe":
			return "probe"
		case "opensbx-command":
			return "payload"
		case "opensbx-signal":
			return "signal"
		}
	}
	return "direct"
}

func dockerOutputFrame(stream byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
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
	if sequence := f.responses[key]; len(sequence) > 0 {
		status = sequence[0]
		f.responses[key] = sequence[1:]
	}
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
	case r.Method == http.MethodGet && path == "/info":
		_, _ = w.Write([]byte(`{"OSType":"linux","Architecture":"aarch64","ServerVersion":"27.1.2"}`))
	case r.Method == http.MethodGet && path == "/images/json":
		_, _ = w.Write([]byte(`[{"Id":"sha256:image-1","RepoTags":["alpine:latest"],"Size":123}]`))
	case r.Method == http.MethodGet && (path == "/images/alpine/json" || path == "/images/alpine:latest/json"):
		_, _ = w.Write([]byte(`{"Id":"sha256:image-1","RepoTags":["alpine:latest"],"Size":123,"Created":"2026-01-01T00:00:00Z","Architecture":"amd64","Os":"linux"}`))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		f.mu.Lock()
		loaded, tag, digest, nativeID := f.cacheLoaded, f.cacheTag, f.cacheDigest, f.cacheNativeID
		f.mu.Unlock()
		if decoded, err := url.PathUnescape(id); err == nil {
			id = decoded
		}
		if (id != tag && id != digest && id != nativeID) || !loaded {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			break
		}
		if f.cacheInspectID != "" {
			nativeID = f.cacheInspectID
		}
		_, _ = fmt.Fprintf(w, `{"Id":%q,"RepoTags":[%q],"Architecture":"amd64","Os":"linux"}`, nativeID, tag)
	case r.Method == http.MethodPost && path == "/images/load":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.cacheLoaded, f.loadedArchive = true, body
		loadBody := f.cacheLoadBody
		f.mu.Unlock()
		if loadBody != "" {
			_, _ = io.WriteString(w, loadBody)
			break
		}
		_, _ = w.Write([]byte("{\"stream\":\"Loaded image\"}\n"))
	case r.Method == http.MethodGet && path == "/images/get":
		f.mu.Lock()
		loaded, archive, mode, entered, release := f.cacheLoaded, f.cacheSavedArchive, f.cacheSaveMode, f.cacheSaveEntered, f.cacheSaveRelease
		if len(archive) == 0 {
			archive = f.loadedArchive
		}
		f.mu.Unlock()
		if !loaded {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			break
		}
		w.Header().Set("Content-Type", "application/x-tar")
		switch mode {
		case "status-error":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"fixture image-save error"}`))
		case "truncated-reader":
			writeTruncatedImageSave(f.t, w, archive, int64(len(archive)+128))
		case "oversized-advertised":
			writeTruncatedImageSave(f.t, w, nil, images.MaxArchiveBytes+1)
		case "block-reader":
			writeBlockedImageSave(f.t, w, archive, entered, release)
		default:
			_, _ = w.Write(archive)
		}
	case r.Method == http.MethodPost && path == "/containers/create":
		var payload struct {
			container.Config
			HostConfig container.HostConfig `json:"HostConfig"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.createBody, f.createHost = payload.Config, payload.HostConfig
		f.containerLabels = payload.Config.Labels
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"Id":%q,"Warnings":[]}`, f.containerID)
	case r.Method == http.MethodGet && path == "/containers/json":
		_, _ = w.Write([]byte(`[{"Id":"container-1","Names":["/demo"],"Image":"alpine:latest","State":"running","Status":"Up","Ports":[{"PrivatePort":3000,"PublicPort":32768,"Type":"tcp"}]}]`))
	case r.Method == http.MethodGet && path == "/containers/container-1/json":
		f.mu.Lock()
		running, paused, labels, hostPort, env := f.running, f.paused, f.containerLabels, f.containerHostPort, append([]string(nil), f.containerEnv...)
		f.mu.Unlock()
		if hostPort == "" {
			hostPort = "32768"
		}
		labelsJSON, _ := json.Marshal(labels)
		envJSON, _ := json.Marshal(env)
		status := "exited"
		if running {
			status = "running"
		}
		_, _ = fmt.Fprintf(w, `{"Id":"container-1","Name":"/demo","Config":{"Image":"alpine:latest","Labels":%s,"Env":%s},"State":{"Status":%q,"Running":%t,"Paused":%t,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"},"HostConfig":{"Memory":1073741824,"NanoCpus":1000000000},"NetworkSettings":{"Ports":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}}}`, labelsJSON, envJSON, status, running, paused, hostPort)
	case r.Method == http.MethodGet && path == "/containers/container-2/json":
		_, _ = w.Write([]byte(`{"Id":"container-2","Name":"/demo-two","Config":{"Image":"alpine:latest","Labels":{}},"State":{"Status":"running","Running":true,"Paused":false,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"},"HostConfig":{"Memory":1073741824,"NanoCpus":1000000000},"NetworkSettings":{"Ports":{}}}`))
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
			AttachStdin bool     `json:"AttachStdin"`
			Cmd         []string `json:"Cmd"`
			Env         []string `json:"Env"`
			WorkingDir  string   `json:"WorkingDir"`
		}
		_ = json.Unmarshal(body, &options)
		kind := classifyDockerFixtureExec(options.Cmd)
		if kind == "payload" || kind == "direct" {
			f.mu.Lock()
			var entered, release chan struct{}
			if !f.execCreateBarrierUsed {
				f.execCreateBarrierUsed = true
				entered, release = f.execCreateEntered, f.releaseExecCreate
			}
			f.mu.Unlock()
			if entered != nil {
				close(entered)
				<-release
			}
		}
		if kind == "probe" {
			f.mu.Lock()
			probeCreateStatus, probeCreateError := f.probeCreateStatus, f.probeCreateError
			if probeCreateStatus != 0 {
				f.execOptions = append(f.execOptions, string(body))
				f.execCreates = append(f.execCreates, dockerExecCreate{Kind: kind, Cmd: append([]string(nil), options.Cmd...), Env: append([]string(nil), options.Env...), WorkingDir: options.WorkingDir})
			}
			f.mu.Unlock()
			if probeCreateStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(probeCreateStatus)
				message := probeCreateError
				if message == "" {
					message = "fixture probe create failure"
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
				return
			}
		}
		execID := "exec-1"
		f.mu.Lock()
		f.execStdin = options.AttachStdin
		f.execOptions = append(f.execOptions, string(body))
		f.execCount++
		switch kind {
		case "probe":
			execID = "exec-probe"
		case "payload":
			if f.execStartup != nil {
				execID = "exec-payload"
			}
		case "signal":
			execID = "exec-signal"
			if f.execStartup != nil {
				f.execStartup.signalExecCreates++
				if !f.execStartup.payloadRunning {
					f.execStartup.prematureSignals++
				}
			}
		}
		nonce := ""
		if (kind == "probe" || kind == "payload") && len(options.Cmd) > 4 {
			nonce = options.Cmd[4]
		}
		f.execCreates = append(f.execCreates, dockerExecCreate{ID: execID, Kind: kind, Cmd: append([]string(nil), options.Cmd...), Env: append([]string(nil), options.Env...), WorkingDir: options.WorkingDir})
		f.execRecords[execID] = dockerExecRecord{Kind: kind, Nonce: nonce, Cmd: append([]string(nil), options.Cmd...)}
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"Id":%q}`, execID)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/exec/") && strings.HasSuffix(path, "/start"):
		execID := strings.TrimSuffix(strings.TrimPrefix(path, "/exec/"), "/start")
		f.serveExecStart(w, r, execID)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/exec/") && strings.HasSuffix(path, "/json"):
		execID := strings.TrimSuffix(strings.TrimPrefix(path, "/exec/"), "/json")
		f.serveExecInspect(w, execID)
	case r.Method == http.MethodPost && path == "/images/create":
		body := f.pullBody
		if body == "" {
			body = "{\"status\":\"done\"}\n"
		}
		_, _ = w.Write([]byte(body))
	case r.Method == http.MethodPost && path == "/containers/container-1/start":
		f.mu.Lock()
		f.running = true
		failure := f.failAfterMutation[key]
		f.mu.Unlock()
		if failure != 0 {
			writeDockerFixtureFailure(w, failure)
			break
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/containers/container-1/stop" && f.blockStopRequests:
		f.mu.Lock()
		entered := f.stopEntered
		f.mu.Unlock()
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		<-r.Context().Done()
	case r.Method == http.MethodPost && path == "/containers/container-2/stop":
		if f.blockStopRequests {
			f.mu.Lock()
			entered := f.stopEntered
			f.mu.Unlock()
			if entered != nil {
				select {
				case entered <- struct{}{}:
				default:
				}
			}
			<-r.Context().Done()
			break
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/containers/container-1/restart":
		f.mu.Lock()
		f.running = true
		failure := f.failAfterMutation[key]
		f.mu.Unlock()
		if failure != 0 {
			writeDockerFixtureFailure(w, failure)
			break
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/containers/container-1/stop":
		f.mu.Lock()
		f.running = false
		failure := f.failAfterMutation[key]
		f.mu.Unlock()
		if failure != 0 {
			writeDockerFixtureFailure(w, failure)
			break
		}
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

func writeDockerFixtureFailure(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"message":"fixture post-mutation error"}`))
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

func (f *dockerAPIFixture) serveExecStart(w http.ResponseWriter, r *http.Request, execID string) {
	f.mu.Lock()
	trace := f.execStartup
	record, known := f.execRecords[execID]
	f.mu.Unlock()
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"exec not found"}`))
		return
	}
	switch record.Kind {
	case "probe":
		f.mu.Lock()
		startStatus := f.probeStartStatus
		f.mu.Unlock()
		if startStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(startStatus)
			_, _ = w.Write([]byte(`{"message":"fixture identity probe ExecStart failure"}`))
			return
		}
		f.serveIdentityProbeAttach(w, r, execID, record)
	case "payload":
		if trace != nil {
			f.serveStartupPayload(w, r, trace, record)
		} else {
			f.serveWrappedPayloadAttach(w, r, execID, record)
		}
	case "signal":
		f.mu.Lock()
		blockSignalAttach := trace != nil && trace.blockSignalAttach
		f.mu.Unlock()
		if blockSignalAttach {
			f.serveBlockedSignalAttach(w, r, trace, record)
		} else {
			f.serveIdentitySignalAttach(w, r, execID, record, trace)
		}
	case "direct":
		f.serveExecAttach(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"exec not found"}`))
	}
}

func (f *dockerAPIFixture) hijackExecAttach(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, bool) {
	_, _ = io.Copy(io.Discard, r.Body)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return nil, nil, false
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, false
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	_ = rw.Flush()
	return conn, rw, true
}

func writeExecFrames(rw *bufio.ReadWriter, stdout, stderr []byte) {
	for _, stream := range []struct {
		id   byte
		data []byte
	}{{1, stdout}, {2, stderr}} {
		if len(stream.data) == 0 {
			continue
		}
		_, _ = rw.Write(dockerOutputFrame(stream.id, stream.data))
	}
	_ = rw.Flush()
}

func (f *dockerAPIFixture) serveIdentityProbeAttach(w http.ResponseWriter, r *http.Request, execID string, record dockerExecRecord) {
	conn, rw, ok := f.hijackExecAttach(w, r)
	if !ok {
		return
	}
	defer conn.Close()
	f.mu.Lock()
	stdout, stderr, exitCode := f.probeStdout, f.probeStderr, f.probeExitCode
	blockProbe, entered := f.probeBlock, f.probeAttachEntered
	f.mu.Unlock()
	if blockProbe {
		if entered != nil {
			close(entered)
		}
		<-r.Context().Done()
		return
	}
	if stdout == "" && exitCode == 0 {
		stdout = fmt.Sprintf("OPENSBX_PROBE %s 2222 987654\n", record.Nonce)
	}
	f.mu.Lock()
	probe := f.execRecords[execID]
	probe.ExitCode = exitCode
	f.execRecords[execID] = probe
	f.mu.Unlock()
	writeExecFrames(rw, []byte(stdout), []byte(stderr))
}

func (f *dockerAPIFixture) serveWrappedPayloadAttach(w http.ResponseWriter, r *http.Request, execID string, record dockerExecRecord) {
	conn, rw, ok := f.hijackExecAttach(w, r)
	if !ok {
		return
	}
	defer conn.Close()
	line := f.payloadIdentityLine(record.Nonce, 4321, 987654)
	writeExecFrames(rw, []byte("output\n"), append([]byte(line), []byte("warning\n")...))
	f.mu.Lock()
	process := f.execRecords[execID]
	process.ExitCode = 0
	f.execRecords[execID] = process
	f.guestProcesses[4321] = fakeGuestProcess{Start: 987654, Running: false}
	f.mu.Unlock()
}

func (f *dockerAPIFixture) payloadIdentityLine(nonce string, pid int, start uint64) string {
	f.mu.Lock()
	override := f.payloadPreamble
	f.mu.Unlock()
	if override != "" {
		return override
	}
	return fmt.Sprintf("OPENSBX_EXEC %s %d %d\n", nonce, pid, start)
}

func fakeGuestIdentity(trace *execStartupFixture) (int, uint64) {
	pid, start := trace.guestPID, trace.guestStart
	if pid == 0 {
		pid = 4321
	}
	if start == 0 {
		start = 987654
	}
	return pid, start
}

func setFakeGuestIdentity(rc *runningCommand, pid int, start uint64, err error) {
	rc.identityReady = make(chan struct{})
	rc.guestPID, rc.guestStart, rc.identityErr = pid, start, err
	close(rc.identityReady)
}

func (f *dockerAPIFixture) serveIdentitySignalAttach(w http.ResponseWriter, r *http.Request, execID string, record dockerExecRecord, trace *execStartupFixture) {
	conn, rw, ok := f.hijackExecAttach(w, r)
	if !ok {
		return
	}
	defer conn.Close()
	if len(record.Cmd) != 7 {
		f.t.Errorf("identity signal helper argv shape = %#v", record.Cmd)
		return
	}
	pid, pidErr := strconv.Atoi(record.Cmd[4])
	start, startErr := strconv.ParseUint(record.Cmd[5], 10, 64)
	signal, signalErr := strconv.Atoi(record.Cmd[6])
	f.mu.Lock()
	traceExit := 0
	if trace != nil {
		traceExit = trace.signalExecExitCode
	}
	noAck := f.signalNoAck
	stdoutOverride, stderrOverride := f.signalHelperStdout, f.signalHelperStderr
	process := f.guestProcesses[pid]
	traceGuestPID, traceGuestStart := 0, uint64(0)
	if trace != nil {
		traceGuestPID, traceGuestStart = fakeGuestIdentity(trace)
	}
	if trace != nil && trace.payloadRunning && traceGuestPID == pid {
		process = fakeGuestProcess{Start: traceGuestStart, Running: true}
	}
	valid := pidErr == nil && startErr == nil && signalErr == nil && process.Running && process.Start == start && traceExit == 0
	if valid {
		f.guestProcesses[pid] = fakeGuestProcess{Start: start, Running: false}
		f.signalTargets = append(f.signalTargets, pid)
		if trace != nil && traceGuestPID == pid {
			trace.payloadRunning = false
			trace.payloadExitCode = 143
			trace.payloadPID = 0
		}
	}
	result := f.execRecords[execID]
	result.ExitCode = traceExit
	if !valid && result.ExitCode == 0 {
		result.ExitCode = 124
	}
	f.execRecords[execID] = result
	f.mu.Unlock()
	if valid && trace != nil {
		trace.finishOnce.Do(func() { close(trace.finishPayload) })
	}
	stdout := []byte(nil)
	if valid && !noAck {
		if stdoutOverride != "" {
			stdout = []byte(stdoutOverride)
		} else {
			stdout = []byte(fmt.Sprintf("OPENSBX_SIGNAL %d %d %d\n", pid, start, signal))
		}
	}
	writeExecFrames(rw, stdout, []byte(stderrOverride))
}

func (f *dockerAPIFixture) serveStartupPayload(w http.ResponseWriter, r *http.Request, trace *execStartupFixture, record dockerExecRecord) {
	_, _ = io.Copy(io.Discard, r.Body)
	trace.startEnteredOnce.Do(func() { close(trace.payloadStartEntered) })
	select {
	case <-trace.releasePayloadStart:
	case <-r.Context().Done():
		return
	}
	f.mu.Lock()
	startFailure := trace.payloadStartFailure
	if startFailure {
		f.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"fixture payload exec start failed"}`))
		trace.startFailedOnce.Do(func() { close(trace.payloadStartFailed) })
		return
	}
	trace.payloadRunning = true
	if trace.payloadWaitForRunningRelease || trace.payloadFinishOnStart {
		trace.payloadRunning = false
	}
	if trace.payloadRunning {
		trace.payloadPID = 4321
		guestPID, guestStart := fakeGuestIdentity(trace)
		f.guestProcesses[guestPID] = fakeGuestProcess{Start: guestStart, Running: true}
	}
	trace.payloadWasStarted = true
	if trace.payloadFinishOnStart {
		trace.payloadExitCode = 0
	}
	finishOnStart := trace.payloadFinishOnStart
	f.mu.Unlock()
	trace.startedOnce.Do(func() { close(trace.payloadStarted) })
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		f.t.Error("Docker fixture response writer does not support hijacking")
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		f.t.Errorf("hijack payload exec start response: %v", err)
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	_ = rw.Flush()
	if trace.blockPayloadIdentity {
		if trace.identityEntered != nil {
			trace.identityEnteredOnce.Do(func() { close(trace.identityEntered) })
		}
		select {
		case <-trace.releaseIdentity:
		case <-r.Context().Done():
			return
		}
	}
	f.writePayloadIdentity(rw, trace, record)
	if finishOnStart {
		return
	}
	if trace.payloadWaitForRunningRelease {
		select {
		case <-trace.releasePayloadRunning:
			f.mu.Lock()
			trace.payloadRunning = true
			if trace.payloadWaitForPIDRelease {
				trace.payloadPID = 0
			} else {
				trace.payloadPID = 4321
				guestPID, guestStart := fakeGuestIdentity(trace)
				f.guestProcesses[guestPID] = fakeGuestProcess{Start: guestStart, Running: true}
			}
			f.mu.Unlock()
			if trace.payloadRunningReady != nil {
				close(trace.payloadRunningReady)
			}
			if trace.payloadWaitForPIDRelease {
				select {
				case <-trace.releasePayloadPID:
					f.mu.Lock()
					trace.payloadPID = 4321
					guestPID, guestStart := fakeGuestIdentity(trace)
					f.guestProcesses[guestPID] = fakeGuestProcess{Start: guestStart, Running: true}
					f.mu.Unlock()
					if trace.payloadPIDReady != nil {
						close(trace.payloadPIDReady)
					}
				case <-trace.finishPayload:
					return
				case <-r.Context().Done():
					return
				}
			}
		case <-trace.finishPayload:
			return
		case <-r.Context().Done():
			return
		}
	}
	select {
	case <-trace.finishPayload:
	case <-r.Context().Done():
		return
	}
}

func (f *dockerAPIFixture) writePayloadIdentity(rw *bufio.ReadWriter, trace *execStartupFixture, record dockerExecRecord) {
	chunks := trace.payloadPreambleChunks
	if chunks == nil {
		line := trace.payloadPreamble
		if line == "" {
			pid, start := fakeGuestIdentity(trace)
			line = f.payloadIdentityLine(record.Nonce, pid, start)
		}
		chunks = [][]byte{[]byte(line)}
	}
	for i, part := range chunks {
		part = bytes.ReplaceAll(part, []byte("{{NONCE}}"), []byte(record.Nonce))
		if i == len(chunks)-1 && len(trace.payloadStderrTail) > 0 {
			tail := bytes.ReplaceAll(trace.payloadStderrTail, []byte("{{NONCE}}"), []byte(record.Nonce))
			part = append(append([]byte(nil), part...), tail...)
		}
		writeExecFrames(rw, nil, part)
	}
}

func (f *dockerAPIFixture) serveBlockedSignalAttach(w http.ResponseWriter, r *http.Request, trace *execStartupFixture, _ dockerExecRecord) {
	_, _ = io.Copy(io.Discard, r.Body)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		f.t.Error("Docker fixture response writer does not support hijacking")
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		f.t.Errorf("hijack signal exec start response: %v", err)
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	_ = rw.Flush()
	trace.signalAttachOnce.Do(func() { close(trace.signalAttachEntered) })
	select {
	case <-trace.releaseSignalAttach:
	case <-r.Context().Done():
		return
	}
	writeExecFrames(rw, []byte("signal\n"), nil)
}

func (f *dockerAPIFixture) serveExecInspect(w http.ResponseWriter, execID string) {
	f.mu.Lock()
	trace := f.execStartup
	record, ok := f.execRecords[execID]
	if !ok && trace != nil {
		switch execID {
		case "exec-payload":
			record, ok = dockerExecRecord{Kind: "payload"}, true
		case "exec-signal":
			record, ok = dockerExecRecord{Kind: "signal", ExitCode: trace.signalExecExitCode}, true
		}
	}
	if !ok {
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"exec not found"}`))
		return
	}
	switch record.Kind {
	case "probe", "signal":
		exitCode := record.ExitCode
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"ID":%q,"Running":false,"ExitCode":%d}`, execID, exitCode)
	case "direct":
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"ID":%q,"Running":false,"ExitCode":0}`, execID)
	case "payload":
		if trace == nil {
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"ID":%q,"Running":false,"ExitCode":0}`, execID)
			return
		}
		running, started, pid, exitCode := trace.payloadRunning, trace.payloadWasStarted, trace.payloadPID, trace.payloadExitCode
		trace.payloadInspectCalls++
		f.mu.Unlock()
		if !started || running {
			_, _ = fmt.Fprintf(w, `{"ID":%q,"Running":%t,"Pid":%d,"ExitCode":null}`, execID, running, pid)
		} else {
			_, _ = fmt.Fprintf(w, `{"ID":%q,"Running":false,"Pid":%d,"ExitCode":%d}`, execID, pid, exitCode)
		}
	default:
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"exec not found"}`))
	}
}

func (r *execStartupFixture) releaseStart() {
	r.releaseStartOnce.Do(func() { close(r.releasePayloadStart) })
}

func (r *execStartupFixture) releaseRunning() {
	if r.releasePayloadRunning != nil {
		r.runningReleaseOnce.Do(func() { close(r.releasePayloadRunning) })
	}
}

func (r *execStartupFixture) releasePID() {
	if r.releasePayloadPID != nil {
		r.pidReleaseOnce.Do(func() { close(r.releasePayloadPID) })
	}
}

func (r *execStartupFixture) releaseSignal() {
	if r.releaseSignalAttach != nil {
		r.releaseSignalOnce.Do(func() { close(r.releaseSignalAttach) })
	}
}

func (r *execStartupFixture) releaseIdentityPreamble() {
	if r.releaseIdentity != nil {
		r.releaseIdentityOnce.Do(func() { close(r.releaseIdentity) })
	}
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
	created, err := dc.Create(ctx, runtimeio.CreateSandboxRequest{
		Image: "alpine:latest", Ports: []string{"3000"}, Timeout: 600,
		Resources: &runtimeio.ResourceLimits{Memory: 512, CPUs: 1.5}, Env: []string{"MODE=test"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if created.ID != "container-1" || created.Name == "" || len(created.Ports) != 1 || created.Ports[0] != "3000/tcp" {
		t.Fatalf("Create() response = %+v", created)
	}
	createdRow, err := dc.repo.FindByID(created.ID)
	if err != nil || createdRow == nil || createdRow.ExpiresAt == nil {
		t.Fatalf("Create() did not persist its absolute expiration: row=%+v err=%v", createdRow, err)
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
	startedRow, err := dc.repo.FindByID(created.ID)
	if err != nil || startedRow == nil || startedRow.ExpiresAt == nil || !startedRow.ExpiresAt.Equal(*started.ExpiresAt) {
		t.Fatalf("Start() response/persisted deadline mismatch: response=%v row=%+v err=%v", started.ExpiresAt, startedRow, err)
	}
	if err := dc.Stop(ctx, created.ID); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	stoppedRow, err := dc.repo.FindByID(created.ID)
	if err != nil || stoppedRow == nil || stoppedRow.ExpiresAt != nil {
		t.Fatalf("Stop() did not clear durable deadline: row=%+v err=%v", stoppedRow, err)
	}
	restarted, err := dc.Restart(ctx, created.ID)
	if err != nil || restarted.Status != "restarted" || restarted.ExpiresAt == nil {
		t.Fatalf("Restart() = %+v, %v", restarted, err)
	}
	restartedRow, err := dc.repo.FindByID(created.ID)
	if err != nil || restartedRow == nil || restartedRow.ExpiresAt == nil || !restartedRow.ExpiresAt.Equal(*restarted.ExpiresAt) {
		t.Fatalf("Restart() response/persisted deadline mismatch: response=%v row=%+v err=%v", restarted.ExpiresAt, restartedRow, err)
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
	renewedRow, err := dc.repo.FindByID(created.ID)
	renewedTimer := dc.getTimerEntry(created.ID)
	if err != nil || renewedRow == nil || renewedRow.ExpiresAt == nil || renewedTimer == nil || !renewedRow.ExpiresAt.Equal(renewedTimer.expiresAt) {
		t.Fatalf("RenewExpiration() deadline not durable and synchronized: row=%+v timer=%v err=%v", renewedRow, entryDeadline(renewedTimer), err)
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
	if _, err := dc.Create(ctx, runtimeio.CreateSandboxRequest{Image: "missing"}); err != ErrImageNotFound {
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
	started, err := dc.ExecCommand(ctx, "container-1", runtimeio.ExecCommandRequest{Command: "echo", Args: []string{"hello"}, Cwd: "/work", Env: map[string]string{"K": "V"}})
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
	seedDockerLifecycleOwner(t, dc, "container-1")
	fixture.running = false
	if _, err := dc.ExecCommand(ctx, "container-1", runtimeio.ExecCommandRequest{Command: "echo"}); err != ErrNotRunning {
		t.Fatalf("ExecCommand() stopped sandbox error = %v", err)
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
	if _, err := dc.ExecCommand(ctx, "container-1", runtimeio.ExecCommandRequest{Command: "echo"}); err != ErrNotFound {
		t.Fatalf("ExecCommand() missing sandbox error = %v", err)
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
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", NativeID: "container-1", RuntimeKind: "docker", Name: "demo"}); err != nil {
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
	if _, err := dc.Create(ctx, runtimeio.CreateSandboxRequest{Image: "broken"}); err == nil {
		t.Fatal("Create() should return image-inspect failures")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "GET /images/broken/json")
	fixture.mu.Unlock()
	if _, err := dc.Create(ctx, runtimeio.CreateSandboxRequest{Image: "alpine"}); err == nil {
		t.Fatal("Create() should return container-create failures")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "POST /containers/create")
	fixture.mu.Unlock()
	if _, err := dc.Create(ctx, runtimeio.CreateSandboxRequest{Image: "alpine"}); err == nil {
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
	fixture.mu.Unlock()
	if err := dc.Remove(ctx, "container-1"); err != nil {
		t.Fatalf("Remove() should treat Docker not-found as already removed: %v", err)
	}
}

func TestDockerClientStatsAndExecErrorBranches(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	ctx := context.Background()
	seedDockerLifecycleOwner(t, dc, "container-1")
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
	if _, err := dc.ExecCommand(ctx, "container-1", runtimeio.ExecCommandRequest{Command: "echo"}); err == nil {
		t.Fatal("ExecCommand() should surface exec-create failures")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "POST /containers/container-1/exec")
	fixture.fail["POST /exec/exec-1/start"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	command, err := dc.ExecCommand(ctx, "container-1", runtimeio.ExecCommandRequest{Command: "echo"})
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

func TestDockerClientKillRunningCommand(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	race := &execStartupFixture{
		payloadStartEntered: make(chan struct{}),
		releasePayloadStart: make(chan struct{}),
		payloadStarted:      make(chan struct{}),
		payloadStartFailed:  make(chan struct{}),
		finishPayload:       make(chan struct{}),
		payloadRunning:      true,
		payloadWasStarted:   true,
		payloadPID:          4321,
	}
	fixture.mu.Lock()
	fixture.execStartup = race
	fixture.mu.Unlock()
	ctx := context.Background()
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := dc.repo.SaveCommand(database.Command{ID: "cmd-live", SandboxID: "container-1", Name: "sleep", Args: `["30"]`}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := newRingBuffer(32), newRingBuffer(32)
	attached := make(chan struct{})
	close(attached)
	rc := &runningCommand{execID: "exec-payload", sandboxID: "container-1", cmd: []string{"sleep", "30"}, cancel: func() {}, stdout: stdout, stderr: stderr, done: make(chan struct{}), attached: attached}
	setFakeGuestIdentity(rc, 4321, 987654, nil)
	dc.commands.Store("cmd-live", rc)
	if _, err := dc.KillCommand(ctx, "container-1", "cmd-live", 15); err != nil {
		t.Fatalf("KillCommand() running command error: %v", err)
	}
	fixture.mu.Lock()
	signalExecCreates, prematureSignals := race.signalExecCreates, race.prematureSignals
	fixture.mu.Unlock()
	if signalExecCreates != 1 || prematureSignals != 0 {
		t.Fatalf("KillCommand() issued signal before the original exec was running: creates=%d premature=%d", signalExecCreates, prematureSignals)
	}
}

func TestDockerKillRejectsPreCanceledContextAndWrongSandboxBeforeExec(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	rc := &runningCommand{
		execID:    "exec-payload",
		sandboxID: "container-1",
		cmd:       []string{"sleep", "30"},
		cancel:    func() {},
		stdout:    newRingBuffer(32),
		stderr:    newRingBuffer(32),
		done:      make(chan struct{}),
		attached:  make(chan struct{}),
	}
	dc.commands.Store("cmd-live", rc)
	if _, err := dc.KillCommand(context.Background(), "other-container", "cmd-live", 15); !errors.Is(err, sandbox.ErrCommandNotFound) {
		t.Fatalf("KillCommand() wrong sandbox error=%v; want ErrCommandNotFound", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dc.KillCommand(ctx, "container-1", "cmd-live", 15); !errors.Is(err, context.Canceled) {
		t.Fatalf("KillCommand() precanceled context error=%v; want context.Canceled", err)
	}
	fixture.mu.Lock()
	requests := append([]string(nil), fixture.requests...)
	fixture.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("rejected KillCommand calls issued native Docker requests: %v", requests)
	}
}

func TestDockerKillReturnsErrCommandFinishedWhenDoneClosesBeforeFinishedFlag(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	race := &execStartupFixture{
		payloadStartEntered: make(chan struct{}),
		releasePayloadStart: make(chan struct{}),
		payloadStarted:      make(chan struct{}),
		payloadStartFailed:  make(chan struct{}),
		finishPayload:       make(chan struct{}),
		payloadRunning:      false,
		payloadWasStarted:   true,
		payloadPID:          4321,
		payloadExitCode:     143,
	}
	fixture.mu.Lock()
	fixture.execStartup = race
	fixture.mu.Unlock()
	if state := inspectFakePayload(t, dc); state.Running || state.PID == 0 {
		t.Fatalf("finished fake exec should show Running=false with retained PID, got Running=%t PID=%d", state.Running, state.PID)
	}
	done := make(chan struct{})
	close(done)
	dc.commands.Store("cmd-done", &runningCommand{
		execID:    "exec-payload",
		sandboxID: "container-1",
		cmd:       []string{"sleep", "30"},
		cancel:    func() {},
		stdout:    newRingBuffer(32),
		stderr:    newRingBuffer(32),
		done:      done,
		attached:  make(chan struct{}),
	})
	if _, err := dc.KillCommand(context.Background(), "container-1", "cmd-done", 15); !errors.Is(err, sandbox.ErrCommandFinished) {
		t.Fatalf("KillCommand() completed exec error=%v; want ErrCommandFinished", err)
	}
	fixture.mu.Lock()
	signals := race.signalExecCreates
	fixture.mu.Unlock()
	if signals != 0 {
		t.Fatalf("completed exec received %d native signal commands", signals)
	}
}

func TestDockerKillHonorsContextWhileWaitingForPayloadCompletion(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	race := &execStartupFixture{
		payloadStartEntered: make(chan struct{}),
		releasePayloadStart: make(chan struct{}),
		payloadStarted:      make(chan struct{}),
		payloadStartFailed:  make(chan struct{}),
		finishPayload:       make(chan struct{}),
		payloadRunning:      true,
		payloadWasStarted:   true,
		payloadPID:          4321,
	}
	fixture.mu.Lock()
	fixture.execStartup = race
	fixture.mu.Unlock()
	attached := make(chan struct{})
	close(attached)
	dc.commands.Store("cmd-running", &runningCommand{
		execID:    "exec-payload",
		sandboxID: "container-1",
		cmd:       []string{"sleep", "30"},
		cancel:    func() {},
		stdout:    newRingBuffer(32),
		stderr:    newRingBuffer(32),
		done:      make(chan struct{}),
		attached:  attached,
	})
	if value, ok := dc.commands.Load("cmd-running"); ok {
		setFakeGuestIdentity(value.(*runningCommand), 4321, 987654, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := dc.KillCommand(ctx, "container-1", "cmd-running", 15); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("KillCommand() completion wait error=%v; want context.DeadlineExceeded", err)
	}
	fixture.mu.Lock()
	signals := race.signalExecCreates
	fixture.mu.Unlock()
	if signals != 1 {
		t.Fatalf("confirmed-running target should receive one signal before bounded completion wait; got %d", signals)
	}
}

func TestDockerKillWaitsForPayloadExecStartAndHonorsCancellation(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	race := &execStartupFixture{
		payloadStartEntered: make(chan struct{}),
		releasePayloadStart: make(chan struct{}),
		payloadStarted:      make(chan struct{}),
		payloadStartFailed:  make(chan struct{}),
		finishPayload:       make(chan struct{}),
	}
	fixture.mu.Lock()
	fixture.execStartup = race
	fixture.mu.Unlock()
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	command, err := dc.ExecCommand(context.Background(), "container-1", runtimeio.ExecCommandRequest{Command: "sleep", Args: []string{"3600"}})
	if err != nil {
		t.Fatalf("start payload command: %v", err)
	}
	var releaseStartOnce sync.Once
	releasePayloadStart := func() { releaseStartOnce.Do(func() { close(race.releasePayloadStart) }) }
	finishPayload := func() { race.finishOnce.Do(func() { close(race.finishPayload) }) }
	defer func() {
		releasePayloadStart()
		finishPayload()
	}()
	select {
	case <-race.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload ExecAttach did not enter the deterministic blocked start barrier")
	}

	killCtx, cancelKill := context.WithTimeout(context.Background(), 250*time.Millisecond)
	killDone := make(chan struct {
		command runtimeio.CommandDetail
		err     error
	}, 1)
	killCallStarted := make(chan struct{})
	go func() {
		close(killCallStarted)
		result, err := dc.KillCommand(killCtx, "container-1", command.ID, 15)
		killDone <- struct {
			command runtimeio.CommandDetail
			err     error
		}{result, err}
	}()
	<-killCallStarted
	var firstKill struct {
		command runtimeio.CommandDetail
		err     error
	}
	firstKillReturned := false
	select {
	case firstKill = <-killDone:
		firstKillReturned = true
	case <-time.After(2 * time.Second):
	}
	cancelKill()
	fixture.mu.Lock()
	signalsBeforePayloadStarted := race.signalExecCreates
	prematureSignals := race.prematureSignals
	fixture.mu.Unlock()

	// Allow the original exec-start to acknowledge Running only after the
	// cancellation attempt has returned; then exercise the successful kill path.
	releasePayloadStart()
	select {
	case <-race.payloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not reach the fake Docker Running state after releasing start")
	}
	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), time.Second)
	inspect, inspectErr := dc.cli.ExecInspect(inspectCtx, "exec-payload", moby.ExecInspectOptions{})
	inspectCancel()
	if inspectErr != nil || !inspect.Running {
		t.Fatalf("fake Docker payload inspect Running=%t err=%v; want Running=true before the successful kill", inspect.Running, inspectErr)
	}
	completionCtx, cancelCompletion := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCompletion()
	killed, successfulKillErr := dc.KillCommand(completionCtx, "container-1", command.ID, 15)
	finished, waitErr := dc.WaitCommand(completionCtx, "container-1", command.ID)
	fixture.mu.Lock()
	signalExecCreates := race.signalExecCreates
	prematureSignals = race.prematureSignals
	fixture.mu.Unlock()

	if !firstKillReturned || !errors.Is(firstKill.err, context.DeadlineExceeded) || signalsBeforePayloadStarted != 0 || prematureSignals != 0 {
		t.Errorf("canceled pre-start kill returned=%t err=%v signal_exec_creates_before_running=%d premature_signals=%d; want deadline error and no native signal", firstKillReturned, firstKill.err, signalsBeforePayloadStarted, prematureSignals)
	}
	if successfulKillErr != nil {
		t.Errorf("kill after payload Running confirmation: %v", successfulKillErr)
	}
	if waitErr != nil || finished.ExitCode == nil || *finished.ExitCode != 143 {
		t.Errorf("payload completion after confirmed-running kill=%+v err=%v; want exit 143", finished, waitErr)
	}
	if signalExecCreates != 1 {
		t.Errorf("native signal exec count=%d; want exactly one after the payload was Running", signalExecCreates)
	}
	_ = killed
}

func TestDockerKillDoesNotSignalOrReportSuccessWhenPayloadAttachStartFails(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	race := &execStartupFixture{
		payloadStartEntered: make(chan struct{}),
		releasePayloadStart: make(chan struct{}),
		payloadStarted:      make(chan struct{}),
		payloadStartFailed:  make(chan struct{}),
		finishPayload:       make(chan struct{}),
		payloadStartFailure: true,
	}
	fixture.mu.Lock()
	fixture.execStartup = race
	fixture.mu.Unlock()
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	command, err := dc.ExecCommand(context.Background(), "container-1", runtimeio.ExecCommandRequest{Command: "sleep", Args: []string{"3600"}})
	if err != nil {
		t.Fatalf("create payload exec: %v", err)
	}
	var releaseStartOnce sync.Once
	releasePayloadStart := func() { releaseStartOnce.Do(func() { close(race.releasePayloadStart) }) }
	defer releasePayloadStart()
	select {
	case <-race.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload ExecAttach did not enter the deterministic blocked start barrier")
	}

	killCtx, cancelKill := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelKill()
	killDone := make(chan struct {
		command runtimeio.CommandDetail
		err     error
	}, 1)
	go func() {
		result, err := dc.KillCommand(killCtx, "container-1", command.ID, 15)
		killDone <- struct {
			command runtimeio.CommandDetail
			err     error
		}{result, err}
	}()
	releasePayloadStart()
	select {
	case <-race.payloadStartFailed:
	case <-time.After(2 * time.Second):
		t.Fatal("fake Docker did not return the scripted payload attach failure")
	}
	var killResult struct {
		command runtimeio.CommandDetail
		err     error
	}
	select {
	case killResult = <-killDone:
	case <-time.After(2 * time.Second):
		t.Fatal("KillCommand hung after payload ExecAttach failed")
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	finished, waitErr := dc.WaitCommand(waitCtx, "container-1", command.ID)
	fixture.mu.Lock()
	signalExecCreates := race.signalExecCreates
	fixture.mu.Unlock()
	if killResult.err == nil && (killResult.command.ExitCode == nil || *killResult.command.ExitCode == 0) {
		t.Errorf("KillCommand falsely reported successful kill after payload attach failure: %+v", killResult.command)
	}
	if signalExecCreates != 0 {
		t.Errorf("KillCommand dispatched %d native signal execs despite payload attach failure", signalExecCreates)
	}
	if waitErr != nil || finished.ExitCode == nil || *finished.ExitCode != -1 {
		t.Errorf("failed payload command terminal status=%+v err=%v; want bounded nonzero attach failure", finished, waitErr)
	}
}

func TestDockerKillWaitsForWrapperIdentityBeforeCreatingSignalExec(t *testing.T) {
	trace := &execStartupFixture{
		blockPayloadIdentity: true,
		identityEntered:      make(chan struct{}),
		releaseIdentity:      make(chan struct{}),
		payloadRunning:       true,
		payloadWasStarted:    true,
		payloadPID:           4321,
	}
	dc, fixture, command := startFakePayloadExec(t, trace)
	select {
	case <-trace.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not enter its attach barrier")
	}
	trace.releaseStart()
	select {
	case <-trace.identityEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("wrapper did not reach the withheld identity record")
	}
	rc := waitForFakeExecAttached(t, dc, command.ID)
	if !inspectFakePayloadRunning(t, dc) {
		t.Fatal("payload should be Running while its identity record is withheld")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err := dc.KillCommand(ctx, "container-1", command.ID, 15)
	cancel()
	fixture.mu.Lock()
	signals, stillRunning := trace.signalExecCreates, trace.payloadRunning
	fixture.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || signals != 0 || !stillRunning {
		t.Fatalf("pre-identity kill: err=%v helpers=%d payloadRunning=%t; want timeout without signal", err, signals, stillRunning)
	}
	trace.releaseIdentityPreamble()
	select {
	case <-rc.identityReady:
	case <-time.After(time.Second):
		t.Fatal("identity readiness was not published after releasing its record")
	}
	if _, err := dc.KillCommand(context.Background(), "container-1", command.ID, 15); err != nil {
		t.Fatalf("kill after identity became ready: %v", err)
	}
	fixture.mu.Lock()
	signals, stillRunning = trace.signalExecCreates, trace.payloadRunning
	fixture.mu.Unlock()
	if signals != 1 || stillRunning {
		t.Fatalf("post-readiness kill helpers=%d payloadRunning=%t", signals, stillRunning)
	}
}

func TestDockerKillRejectsSignalHelperExitWithoutAck(t *testing.T) {
	trace := &execStartupFixture{}
	dc, fixture, command := startFakePayloadExec(t, trace)
	select {
	case <-trace.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not enter its attach barrier")
	}
	trace.releaseStart()
	waitForFakeExecAttached(t, dc, command.ID)
	if !inspectFakePayloadRunning(t, dc) {
		t.Fatal("payload is not running before missing-ack test")
	}
	fixture.mu.Lock()
	fixture.signalNoAck = true
	fixture.mu.Unlock()
	if _, err := dc.KillCommand(context.Background(), "container-1", command.ID, 15); err == nil || !strings.Contains(err.Error(), "invalid helper acknowledgment") {
		t.Fatalf("missing signal acknowledgment error=%v; helper exit zero must not confirm success", err)
	}
	fixture.mu.Lock()
	creates := trace.signalExecCreates
	targets := append([]int(nil), fixture.signalTargets...)
	fixture.mu.Unlock()
	if creates != 1 || !reflect.DeepEqual(targets, []int{4321}) {
		t.Fatalf("missing-ack helper trace creates=%d targeted guest pids=%v", creates, targets)
	}
}

func TestDockerKillWaitsWhileExecAttachIsUpgradedButPayloadIsNotRunning(t *testing.T) {
	race := execStartupFixture{
		payloadWaitForRunningRelease: true,
		payloadWaitForPIDRelease:     true,
		payloadRunningReady:          make(chan struct{}),
		releasePayloadRunning:        make(chan struct{}),
		payloadPIDReady:              make(chan struct{}),
		releasePayloadPID:            make(chan struct{}),
	}
	dc, fixture, command := startFakePayloadExec(t, &race)
	select {
	case <-race.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not enter held ExecAttach start")
	}
	race.releaseStart()
	waitForFakeExecAttached(t, dc, command.ID)
	if running := inspectFakePayloadRunning(t, dc); running {
		t.Fatal("fake Docker reported Running before the release barrier after the 101 upgrade")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := dc.KillCommand(ctx, "container-1", command.ID, 15)
	fixture.mu.Lock()
	inspectCalls, signals := race.payloadInspectCalls, race.signalExecCreates
	fixture.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("KillCommand() while payload stayed not-running error=%v; want bounded context deadline", err)
	}
	if inspectCalls == 0 {
		t.Error("KillCommand() did not inspect the original exec while it remained not-running")
	}
	if signals != 0 {
		t.Errorf("KillCommand() dispatched %d native signal execs before Running confirmation", signals)
	}
	race.releaseRunning()
	select {
	case <-race.payloadRunningReady:
	case <-time.After(2 * time.Second):
		t.Fatal("fake payload did not reach Running after the explicit release barrier")
	}
	if !inspectFakePayloadRunning(t, dc) {
		t.Fatal("fake Docker did not report Running after the release barrier")
	}
	state := inspectFakePayload(t, dc)
	if !state.Running || state.PID != 0 {
		t.Fatalf("fake Docker must expose the native startup gap Running=true/PID=0, got Running=%t PID=%d", state.Running, state.PID)
	}
	pidCtx, cancelPID := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, pidWaitErr := dc.KillCommand(pidCtx, "container-1", command.ID, 15)
	cancelPID()
	fixture.mu.Lock()
	signalsAtZeroPID := race.signalExecCreates
	fixture.mu.Unlock()
	if !errors.Is(pidWaitErr, context.DeadlineExceeded) || signalsAtZeroPID != 0 {
		t.Fatalf("Running=true/PID=0 must remain unkillable until native PID appears: error=%v signal_execs=%d", pidWaitErr, signalsAtZeroPID)
	}
	race.releasePID()
	select {
	case <-race.payloadPIDReady:
	case <-time.After(2 * time.Second):
		t.Fatal("fake native PID did not become ready after the PID release barrier")
	}
	state = inspectFakePayload(t, dc)
	if !state.Running || state.PID == 0 {
		t.Fatalf("fake Docker did not report a positive native startup PID: Running=%t PID=%d", state.Running, state.PID)
	}
	completeCtx, cancelComplete := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelComplete()
	if _, err := dc.KillCommand(completeCtx, "container-1", command.ID, 15); err != nil {
		t.Fatalf("KillCommand() after Running confirmation: %v", err)
	}
	finished, err := dc.WaitCommand(completeCtx, "container-1", command.ID)
	if err != nil || finished.ExitCode == nil || *finished.ExitCode != 143 {
		t.Fatalf("WaitCommand() after confirmed-running kill=%+v err=%v; want exit 143", finished, err)
	}
}

func TestDockerKillReturnsErrCommandFinishedWhenPayloadCompletesBeforeReady(t *testing.T) {
	race := execStartupFixture{payloadFinishOnStart: true}
	dc, fixture, command := startFakePayloadExec(t, &race)
	select {
	case <-race.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not enter held ExecAttach start")
	}
	race.releaseStart()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	finished, waitErr := dc.WaitCommand(ctx, "container-1", command.ID)
	if waitErr != nil || finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatalf("payload completion before Running confirmation=%+v err=%v", finished, waitErr)
	}
	if _, err := dc.KillCommand(ctx, "container-1", command.ID, 15); !errors.Is(err, sandbox.ErrCommandFinished) {
		t.Fatalf("KillCommand() on exec completed before readiness error=%v; want ErrCommandFinished", err)
	}
	fixture.mu.Lock()
	signals := race.signalExecCreates
	fixture.mu.Unlock()
	if signals != 0 {
		t.Fatalf("completed-before-ready payload received %d native signal execs", signals)
	}
}

func TestDockerKillPropagatesOriginalExecInspectFailure(t *testing.T) {
	race := execStartupFixture{}
	dc, fixture, command := startFakePayloadExec(t, &race)
	select {
	case <-race.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not enter held ExecAttach start")
	}
	race.releaseStart()
	waitForFakeExecAttached(t, dc, command.ID)
	fixture.mu.Lock()
	fixture.fail["GET /exec/exec-payload/json"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := dc.KillCommand(ctx, "container-1", command.ID, 15)
	if err == nil || !strings.Contains(err.Error(), "fixture error") {
		t.Fatalf("KillCommand() original ExecInspect error=%v; want propagated inspect failure", err)
	}
	fixture.mu.Lock()
	signals := race.signalExecCreates
	fixture.mu.Unlock()
	if signals != 0 {
		t.Fatalf("original ExecInspect failure dispatched %d signal execs", signals)
	}
}

func TestDockerKillPropagatesSignalStartFailureAndNonzeroExit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		startError bool
		exitCode   int
		want       string
	}{
		{name: "signal ExecStart error", startError: true, want: "500"},
		{name: "signal exec nonzero exit", exitCode: 9, want: "was not confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			race := execStartupFixture{signalExecExitCode: tc.exitCode}
			dc, fixture, command := startFakePayloadExec(t, &race)
			select {
			case <-race.payloadStartEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("payload did not enter held ExecAttach start")
			}
			race.releaseStart()
			waitForFakeExecAttached(t, dc, command.ID)
			if !inspectFakePayloadRunning(t, dc) {
				t.Fatal("payload is not Running before signal failure subtest")
			}
			if tc.startError {
				fixture.mu.Lock()
				fixture.fail["POST /exec/exec-signal/start"] = http.StatusInternalServerError
				fixture.mu.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := dc.KillCommand(ctx, "container-1", command.ID, 15)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("KillCommand() signal failure=%v; want error containing %q", err, tc.want)
			}
			fixture.mu.Lock()
			signals := race.signalExecCreates
			payloadRunning := race.payloadRunning
			fixture.mu.Unlock()
			if signals != 1 || !payloadRunning {
				t.Fatalf("failed signal must not report/perform a kill: signal execs=%d payloadRunning=%t", signals, payloadRunning)
			}
		})
	}
}

func TestDockerKillDoesNotConfirmSignalWithoutExactHelperAcknowledgment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config func(*dockerAPIFixture)
	}{
		{"missing acknowledgment", func(f *dockerAPIFixture) { f.signalNoAck = true }},
		{"wrong guest identity acknowledgment", func(f *dockerAPIFixture) { f.signalHelperStdout = "OPENSBX_SIGNAL 9999 987654 15\n" }},
		{"unexpected helper stderr", func(f *dockerAPIFixture) { f.signalHelperStderr = "warning\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &execStartupFixture{}
			dc, fixture, command := startFakePayloadExec(t, trace)
			select {
			case <-trace.payloadStartEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("payload did not enter its attach barrier")
			}
			trace.releaseStart()
			waitForFakeExecAttached(t, dc, command.ID)
			if !inspectFakePayloadRunning(t, dc) {
				t.Fatal("payload is not running before invalid-ack signal case")
			}
			fixture.mu.Lock()
			tc.config(fixture)
			fixture.mu.Unlock()
			if _, err := dc.KillCommand(context.Background(), "container-1", command.ID, 15); err == nil || !strings.Contains(err.Error(), "invalid helper acknowledgment") {
				t.Fatalf("invalid signal helper response = %v; must not confirm success", err)
			}
			fixture.mu.Lock()
			created := trace.signalExecCreates
			targets := append([]int(nil), fixture.signalTargets...)
			fixture.mu.Unlock()
			if created != 1 || !reflect.DeepEqual(targets, []int{4321}) {
				t.Fatalf("invalid-ack native helper trace creates=%d targeted guest pids=%v", created, targets)
			}
		})
	}
}

func TestDockerKillUsesGuestIdentityAndPreservesWrappedExecData(t *testing.T) {
	trace := &execStartupFixture{guestPID: 7777, guestStart: 555123}
	cwd := "/work/with spaces"
	payload := []string{"node", "-e", "emit (a|b)[x]+?\\$HOME 'single' \"double\" `ticks`", "", "--help", "line\nnext", "$(touch /tmp/not-executed)"}
	env := map[string]string{"ENV": "hook; value", "BASH_ENV": "hook $(false)", "A_KEY": "value; $(false) 'quoted'"}
	dc, fixture, command := startFakeCommandExecWithImageEnv(t, trace, runtimeio.ExecCommandRequest{Command: payload[0], Args: payload[1:], Cwd: cwd, Env: env}, []string{"BASE=image", "A_KEY=container"})
	select {
	case <-trace.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("wrapped payload did not enter its deterministic attach barrier")
	}
	trace.releaseStart()
	rc := waitForFakeExecAttached(t, dc, command.ID)
	if pid, start, err := dc.waitGuestIdentity(context.Background(), rc); err != nil || pid != 7777 || start != 555123 {
		t.Fatalf("published wrapped guest identity = %d/%d err=%v", pid, start, err)
	}
	if host := inspectFakePayload(t, dc); host.PID != 4321 || host.PID == 7777 {
		t.Fatalf("fixture must distinguish Docker host startup PID from guest identity: %+v", host)
	}

	// Create a live sibling with the same guest argv but a distinct identity.
	const siblingID = "cmd-identical-sibling"
	argsJSON, _ := json.Marshal(payload[1:])
	if err := dc.repo.SaveCommand(database.Command{ID: siblingID, SandboxID: "container-1", Name: payload[0], Args: string(argsJSON)}); err != nil {
		t.Fatal(err)
	}
	siblingDone := make(chan struct{})
	siblingAttached := make(chan struct{})
	close(siblingAttached)
	sibling := &runningCommand{execID: "exec-sibling", sandboxID: "container-1", cmd: append([]string(nil), payload...), cancel: func() {}, stdout: newRingBuffer(32), stderr: newRingBuffer(32), done: siblingDone, attached: siblingAttached}
	setFakeGuestIdentity(sibling, 8888, 555124, nil)
	dc.commands.Store(siblingID, sibling)
	fixture.mu.Lock()
	fixture.guestProcesses[8888] = fakeGuestProcess{Start: 555124, Running: true}
	fixture.mu.Unlock()

	if _, err := dc.KillCommand(context.Background(), "container-1", command.ID, 15); err != nil {
		t.Fatalf("KillCommand() for one identical-argv command: %v", err)
	}
	finished, err := dc.WaitCommand(context.Background(), "container-1", command.ID)
	if err != nil || finished.ExitCode == nil || *finished.ExitCode != 143 {
		t.Fatalf("selected command terminal detail=%+v err=%v; want SIGTERM exit", finished, err)
	}
	fixture.mu.Lock()
	selected, siblingState := fixture.guestProcesses[7777], fixture.guestProcesses[8888]
	targets := append([]int(nil), fixture.signalTargets...)
	creates := append([]dockerExecCreate(nil), fixture.execCreates...)
	fixture.mu.Unlock()
	if selected.Running || !siblingState.Running || !reflect.DeepEqual(targets, []int{7777}) {
		t.Fatalf("signal isolation failed: selected=%+v sibling=%+v targets=%v", selected, siblingState, targets)
	}

	var probe, wrapped, signal *dockerExecCreate
	for i := range creates {
		record := &creates[i]
		switch record.Kind {
		case "probe":
			probe = record
		case "payload":
			wrapped = record
		case "signal":
			signal = record
		}
	}
	if probe == nil || wrapped == nil || signal == nil {
		t.Fatalf("native helper roles were not distinguished: %#v", creates)
	}
	internalEnv := []string{"A_KEY=value; $(false) 'quoted'", "ENV=", "BASH_ENV="}
	if probe.Cmd[2] != identityProbeScript || probe.Cmd[3] != "opensbx-probe" || probe.Cmd[4] != command.ID || probe.WorkingDir != cwd || !reflect.DeepEqual(probe.Env, internalEnv) {
		t.Fatalf("probe exec lost nonce/cwd contract: %+v", *probe)
	}
	if wrapped.Cmd[3] != "opensbx-command" || wrapped.Cmd[4] != command.ID || wrapped.WorkingDir != cwd || wrapped.Cmd[2] != identityWrapperScript {
		t.Fatalf("wrapped payload helper metadata changed: %+v", *wrapped)
	}
	separator := -1
	for i, arg := range wrapped.Cmd {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || !reflect.DeepEqual(wrapped.Cmd[separator+1:], payload) {
		t.Fatalf("guest argv after wrapper separator = %#v, want %#v", wrapped.Cmd[separator+1:], payload)
	}
	if !reflect.DeepEqual(wrapped.Cmd[5:separator], []string{"A_KEY=value; $(false) 'quoted'", "BASE=image", "BASH_ENV=hook $(false)", "ENV=hook; value"}) {
		t.Fatalf("wrapper did not carry the effective sorted original environment as data: %#v", wrapped.Cmd[5:separator])
	}
	for _, untrusted := range []string{payload[2], payload[4], payload[5], env["A_KEY"], env["ENV"], env["BASH_ENV"]} {
		if strings.Contains(wrapped.Cmd[2], untrusted) {
			t.Fatalf("untrusted argument/environment was interpolated into wrapper source: %q", untrusted)
		}
	}
	if !reflect.DeepEqual(wrapped.Env, internalEnv) {
		t.Fatalf("internal payload hook-suppression environment = %#v", wrapped.Env)
	}
	if signal.Cmd[3] != "opensbx-signal" || !reflect.DeepEqual(signal.Cmd[4:], []string{"7777", "555123", "15"}) || !reflect.DeepEqual(signal.Env, []string{"ENV=", "BASH_ENV="}) {
		t.Fatalf("signal helper did not carry the one verified guest identity: %+v", *signal)
	}
	if strings.Contains(signal.Cmd[2], payload[2]) || strings.Contains(signal.Cmd[2], env["A_KEY"]) {
		t.Fatal("payload-controlled text was interpolated into signal helper source")
	}
}

func TestDockerKillRejectsInvalidSignalsBeforeNativeExec(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	for _, signal := range []int{0, -1, 65} {
		t.Run(fmt.Sprintf("signal_%d", signal), func(t *testing.T) {
			_, err := dc.KillCommand(context.Background(), "container-1", "missing-command", signal)
			if !errors.Is(err, sandbox.ErrInvalidInput) {
				t.Fatalf("KillCommand(signal=%d) error=%v; want ErrInvalidInput before command lookup", signal, err)
			}
		})
	}
	fixture.mu.Lock()
	requests := append([]string(nil), fixture.requests...)
	fixture.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("invalid signals caused native Docker requests: %v", requests)
	}
}

func TestDockerKillPropagatesSignalStreamContextCancellation(t *testing.T) {
	race := execStartupFixture{
		blockSignalAttach:   true,
		signalAttachEntered: make(chan struct{}),
		releaseSignalAttach: make(chan struct{}),
	}
	dc, fixture, command := startFakePayloadExec(t, &race)
	select {
	case <-race.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not enter held ExecAttach start")
	}
	race.releaseStart()
	waitForFakeExecAttached(t, dc, command.ID)
	if !inspectFakePayloadRunning(t, dc) {
		t.Fatal("payload is not Running before signal-stream cancellation test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := dc.KillCommand(ctx, "container-1", command.ID, 15)
		done <- err
	}()
	select {
	case <-race.signalAttachEntered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("signal exec did not reach the deterministic blocked output stream")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("KillCommand() canceled signal stream error=%v; want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("KillCommand() did not return promptly after signal stream cancellation")
	}
	fixture.mu.Lock()
	payloadRunning := race.payloadRunning
	fixture.mu.Unlock()
	if !payloadRunning {
		t.Error("canceling the signal stream also canceled the original payload")
	}
}

func startFakePayloadExec(t *testing.T, trace *execStartupFixture) (*Client, *dockerAPIFixture, runtimeio.CommandDetail) {
	return startFakeCommandExec(t, trace, runtimeio.ExecCommandRequest{Command: "sleep", Args: []string{"3600"}})
}

func startFakeCommandExec(t *testing.T, trace *execStartupFixture, request runtimeio.ExecCommandRequest) (*Client, *dockerAPIFixture, runtimeio.CommandDetail) {
	return startFakeCommandExecWithImageEnv(t, trace, request, nil)
}

func startFakeCommandExecWithImageEnv(t *testing.T, trace *execStartupFixture, request runtimeio.ExecCommandRequest, imageEnv []string) (*Client, *dockerAPIFixture, runtimeio.CommandDetail) {
	t.Helper()
	if trace.payloadStartEntered == nil {
		trace.payloadStartEntered = make(chan struct{})
	}
	if trace.releasePayloadStart == nil {
		trace.releasePayloadStart = make(chan struct{})
	}
	if trace.payloadStarted == nil {
		trace.payloadStarted = make(chan struct{})
	}
	if trace.payloadStartFailed == nil {
		trace.payloadStartFailed = make(chan struct{})
	}
	if trace.finishPayload == nil {
		trace.finishPayload = make(chan struct{})
	}
	dc, fixture := newDockerFixture(t)
	fixture.mu.Lock()
	fixture.execStartup = trace
	fixture.containerEnv = append([]string(nil), imageEnv...)
	fixture.mu.Unlock()
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		trace.releaseStart()
		trace.releaseRunning()
		trace.releasePID()
		trace.releaseSignal()
		trace.releaseIdentityPreamble()
		fixture.mu.Lock()
		trace.payloadRunning = false
		if trace.payloadExitCode == 0 {
			trace.payloadExitCode = 143
		}
		fixture.mu.Unlock()
		trace.finishOnce.Do(func() { close(trace.finishPayload) })
	})
	command, err := dc.ExecCommand(context.Background(), "container-1", request)
	if err != nil {
		t.Fatalf("create fake payload exec: %v", err)
	}
	return dc, fixture, command
}

func waitForFakeExecAttached(t *testing.T, dc *Client, commandID string) *runningCommand {
	t.Helper()
	value, ok := dc.commands.Load(commandID)
	if !ok {
		t.Fatalf("payload command %s is not tracked", commandID)
	}
	rc := value.(*runningCommand)
	select {
	case <-rc.attached:
		rc.mu.Lock()
		startErr := rc.startErr
		rc.mu.Unlock()
		if startErr != nil {
			t.Fatalf("payload Attach failed before readiness check: %v", startErr)
		}
		return rc
	case <-rc.done:
		rc.mu.Lock()
		startErr := rc.startErr
		rc.mu.Unlock()
		t.Fatalf("payload exec finished before Attach: %v", startErr)
	case <-time.After(2 * time.Second):
		t.Fatalf("payload command %s did not complete Attach handshake", commandID)
	}
	return nil
}

func inspectFakePayloadRunning(t *testing.T, dc *Client) bool {
	t.Helper()
	return inspectFakePayload(t, dc).Running
}

func inspectFakePayload(t *testing.T, dc *Client) moby.ExecInspectResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inspect, err := dc.cli.ExecInspect(ctx, "exec-payload", moby.ExecInspectOptions{})
	if err != nil {
		t.Fatalf("inspect fake payload exec: %v", err)
	}
	return inspect
}

func TestDockerClientTimerExpiryStopsContainer(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	seedDockerLifecycleOwner(t, dc, "container-1")
	dc.scheduleStop("container-1", -1)
	select {
	case <-fixture.stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the timer-triggered Docker stop request to complete")
	}
	// The HTTP fixture signals before the client consumes its response. Wait for
	// expiration to finish confirming the stop before checking tracking cleanup.
	dc.lifecycleMu.Lock()
	entry := dc.getTimerEntry("container-1")
	dc.lifecycleMu.Unlock()
	if entry != nil {
		t.Fatal("expired timer remained in tracking map after confirmed stop")
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
