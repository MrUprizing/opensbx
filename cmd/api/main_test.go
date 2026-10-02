package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/docker"
	"opensbx/internal/processctl"
)

func TestMainProcessHelper(t *testing.T) {
	if os.Getenv("OPENSBX_MAIN_HELPER") != "1" {
		return
	}
	os.Args = []string{"opensbx-main-helper"}
	main()
}

func TestMainRuntimePreflightFailureHelper(t *testing.T) {
	if os.Getenv("OPENSBX_RUNTIME_PREFLIGHT_HELPER") != "1" {
		return
	}
	os.Args = []string{"opensbx-main-helper", "-runtime", "container"}
	flag.CommandLine = flag.NewFlagSet("opensbx-preflight-helper", flag.ExitOnError)
	main()
}

func TestMainSyntheticDockerHelper(t *testing.T) {
	if os.Getenv("OPENSBX_MAIN_SYNTHETIC_HELPER") != "1" {
		return
	}
	os.Args = []string{"opensbx-main-helper", "-runtime", "docker"}
	flag.CommandLine = flag.NewFlagSet("opensbx-synthetic-main", flag.ExitOnError)
	main()
}

func TestMainImageCommandHelper(t *testing.T) {
	if os.Getenv("OPENSBX_IMAGE_COMMAND_HELPER") != "1" {
		return
	}
	command := os.Getenv("OPENSBX_IMAGE_COMMAND")
	os.Args = []string{"opensbx-main-helper", "image", command, "--data-dir", os.Getenv("OPENSBX_IMAGE_DATA_DIR")}
	main()
}

func TestImageListAndHelpCommandsBypassRuntimeSelection(t *testing.T) {
	for _, operation := range []string{"help", "list"} {
		t.Run(operation, func(t *testing.T) {
			dataDir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainImageCommandHelper$")
			env := make([]string, 0, len(os.Environ())+3)
			for _, item := range os.Environ() {
				if strings.HasPrefix(item, "OPENSBX_IMAGE_COMMAND_HELPER=") || strings.HasPrefix(item, "OPENSBX_IMAGE_COMMAND=") || strings.HasPrefix(item, "OPENSBX_IMAGE_DATA_DIR=") || strings.HasPrefix(item, "DOCKER_HOST=") || strings.HasPrefix(item, "PROXY_ADDR=") || strings.HasPrefix(item, "BASE_DOMAIN=") {
					continue
				}
				env = append(env, item)
			}
			cmd.Env = append(env, "OPENSBX_IMAGE_COMMAND_HELPER=1", "OPENSBX_IMAGE_COMMAND="+operation, "OPENSBX_IMAGE_DATA_DIR="+dataDir, "DOCKER_HOST=tcp://127.0.0.1:1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("image %s invoked runtime or failed: %v stdout=%s stderr=%s", operation, err, stdout.String(), stderr.String())
			}
			if operation == "help" && !strings.Contains(stdout.String(), "Usage: opensbx image") {
				t.Fatalf("image help=%q", stdout.String())
			}
			if operation == "list" && !strings.HasPrefix(strings.TrimSpace(stdout.String()), "[]") {
				t.Fatalf("empty isolated image catalog=%q", stdout.String())
			}
		})
	}
}

func TestMainUsesOneLoopbackListenerForControlAndSandboxHosts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful child termination uses Unix SIGTERM")
	}
	var engineMu sync.Mutex
	engineRequests := []string{}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.54")
		path = strings.TrimPrefix(path, "/v1.53")
		engineMu.Lock()
		engineRequests = append(engineRequests, r.Method+" "+path)
		engineMu.Unlock()
		switch {
		case r.Method == http.MethodGet && path == "/_ping":
			_, _ = io.WriteString(w, "OK")
		case r.Method == http.MethodGet && path == "/version":
			_, _ = io.WriteString(w, `{"ApiVersion":"1.54","MinAPIVersion":"1.24"}`)
		case r.Method == http.MethodGet && path == "/info":
			_, _ = io.WriteString(w, `{"OSType":"linux","Architecture":"x86_64","ServerVersion":"27.1.0"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"unsupported synthetic Docker API endpoint"}`)
		}
	}))
	t.Cleanup(engine.Close)
	workDir := t.TempDir()
	apiAddr := reserveTCPAddress(t)
	logPath := filepath.Join(workDir, "api.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainSyntheticDockerHelper$")
	env := make([]string, 0, len(os.Environ())+7)
	for _, entry := range os.Environ() {
		remove := false
		for _, prefix := range []string{"ADDR=", "API_KEY=", "BASE_DOMAIN=", "PROXY_ADDR=", "DOCKER_HOST=", "LOG_FILE=", "OPENSBX_DATA_DIR=", "OPENSBX_MAIN_SYNTHETIC_HELPER="} {
			if strings.HasPrefix(entry, prefix) {
				remove = true
				break
			}
		}
		if !remove {
			env = append(env, entry)
		}
	}
	cmd.Env = append(env,
		"OPENSBX_MAIN_SYNTHETIC_HELPER=1",
		"DOCKER_HOST=tcp://"+strings.TrimPrefix(engine.URL, "http://"),
		"ADDR="+apiAddr,
		"API_KEY=",
		"OPENSBX_DATA_DIR="+filepath.Join(workDir, "data"),
		"LOG_FILE="+logPath,
	)
	if coverageDir := flagValue("test.gocoverdir"); coverageDir != "" {
		cmd.Args = append(cmd.Args, "-test.gocoverdir="+coverageDir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	stop := func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			finished = true
		}
	}
	t.Cleanup(stop)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + apiAddr + "/v1/health")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	endpoint := "http://" + apiAddr
	secondAddr := reserveTCPAddress(t)
	second := exec.Command(os.Args[0], "-test.run=^TestMainSyntheticDockerHelper$")
	second.Env = append(env,
		"OPENSBX_MAIN_SYNTHETIC_HELPER=1",
		"DOCKER_HOST=tcp://"+strings.TrimPrefix(engine.URL, "http://"),
		"ADDR="+secondAddr,
		"OPENSBX_DATA_DIR="+filepath.Join(workDir, "data"),
		"LOG_FILE="+filepath.Join(workDir, "second-api.log"),
	)
	if coverageDir := flagValue("test.gocoverdir"); coverageDir != "" {
		second.Args = append(second.Args, "-test.gocoverdir="+coverageDir)
	}
	var secondStdout, secondStderr bytes.Buffer
	second.Stdout, second.Stderr = &secondStdout, &secondStderr
	if err := second.Run(); err == nil || !strings.Contains(secondStdout.String()+secondStderr.String(), "execution database already in use") {
		t.Fatalf("second process did not fail the exclusive data-dir lock: err=%v stdout=%s stderr=%s", err, secondStdout.String(), secondStderr.String())
	}
	assertListenerReleased(t, secondAddr)
	request := func(host, path, token string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, endpoint+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request Host=%q path=%q: %v; stdout=%s stderr=%s", host, path, err, stdout.String(), stderr.String())
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(body)
	}
	port := strings.Split(apiAddr, ":")[1]
	controlHost := "localhost:" + port
	control, body := request(controlHost, "/v1/health", "")
	if control.StatusCode != http.StatusOK || !strings.Contains(body, "healthy") {
		t.Fatalf("control host health status=%d body=%q", control.StatusCode, body)
	}
	images, imageBody := request(controlHost, "/v1/images", "")
	if images.StatusCode != http.StatusOK || !strings.Contains(imageBody, `"images":[]`) {
		t.Fatalf("control image catalog status=%d body=%q", images.StatusCode, imageBody)
	}
	postOrigin := func(origin string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/images/pull", strings.NewReader("image=example.test/attacker"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = controlHost
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("mutating request Origin=%q: %v", origin, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(body)
	}
	postFetchSite := func(site string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/images/pull", strings.NewReader("image=example.test/attacker"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = controlHost
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", site)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("mutating request Fetch-Site=%q: %v", site, err)
		}
		_ = resp.Body.Close()
		return resp
	}
	t.Run("sandbox origin cannot mutate control host", func(t *testing.T) {
		crossOrigin, body := postOrigin("http://qa.localhost:" + port)
		if crossOrigin.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin form status=%d want 403 body=%q", crossOrigin.StatusCode, body)
		}
	})
	t.Run("same control origin remains allowed", func(t *testing.T) {
		response, _ := postOrigin("http://localhost:" + port)
		if response.StatusCode == http.StatusForbidden {
			t.Fatalf("same-origin mutation rejected: %d", response.StatusCode)
		}
	})
	t.Run("originless local CLI remains allowed", func(t *testing.T) {
		response, _ := postOrigin("")
		if response.StatusCode == http.StatusForbidden {
			t.Fatalf("originless local CLI rejected: %d", response.StatusCode)
		}
	})
	t.Run("originless MCP remains allowed", func(t *testing.T) {
		mcpReq, err := http.NewRequest(http.MethodPost, endpoint+"/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"unknown"}`))
		if err != nil {
			t.Fatal(err)
		}
		mcpReq.Host = controlHost
		mcpReq.Header.Set("Content-Type", "application/json")
		mcpResponse, err := client.Do(mcpReq)
		if err != nil {
			t.Fatal(err)
		}
		_ = mcpResponse.Body.Close()
		if mcpResponse.StatusCode == http.StatusForbidden {
			t.Fatal("originless MCP client was rejected")
		}
	})
	t.Run("Fetch-Metadata rejects cross-site and same-site forms without Origin", func(t *testing.T) {
		for _, site := range []string{"cross-site", "same-site"} {
			if response := postFetchSite(site); response.StatusCode != http.StatusForbidden {
				t.Errorf("Fetch-Site=%q status=%d want 403", site, response.StatusCode)
			}
		}
	})
	t.Run("Fetch-Metadata accepts native and same-origin traffic", func(t *testing.T) {
		for _, site := range []string{"none", "same-origin"} {
			if response := postFetchSite(site); response.StatusCode == http.StatusForbidden {
				t.Errorf("Fetch-Site=%q rejected local request", site)
			}
		}
	})
	sandboxHost := "qa.localhost:" + port
	forwarded, body := request(sandboxHost, "/v1/health", "")
	if forwarded.StatusCode != http.StatusBadGateway || strings.Contains(body, "healthy") {
		t.Fatalf("sandbox host fell back to management API: status=%d body=%q", forwarded.StatusCode, body)
	}
	swagger, body := request(controlHost, "/swagger/index.html", "")
	if swagger.StatusCode != http.StatusOK || !strings.Contains(body, "Swagger") {
		t.Fatalf("control swagger status=%d body length=%d", swagger.StatusCode, len(body))
	}
	unknown, _ := request("untrusted.test:"+port, "/v1/health", "")
	if unknown.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("unknown host status=%d", unknown.StatusCode)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("synthetic server shutdown: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	finished = true
	assertListenerReleased(t, apiAddr)
	engineMu.Lock()
	requests := append([]string(nil), engineRequests...)
	engineMu.Unlock()
	for _, request := range requests {
		if strings.Contains(request, "/containers/create") {
			t.Fatalf("synthetic API test unexpectedly created a native sandbox: %v", requests)
		}
	}
}

func TestExplicitApplePreflightFailureOccursBeforeOpeningListeners(t *testing.T) {
	workDir := t.TempDir()
	apiAddr := reserveTCPAddress(t)
	pathWithoutRuntime := filepath.Join(workDir, "empty-path")
	if err := os.Mkdir(pathWithoutRuntime, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainRuntimePreflightFailureHelper$")
	cmd.Dir = workDir
	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "ADDR=") || strings.HasPrefix(entry, "PROXY_ADDR=") || strings.HasPrefix(entry, "BASE_DOMAIN=") || strings.HasPrefix(entry, "LOG_FILE=") || strings.HasPrefix(entry, "OPENSBX_DATA_DIR=") || strings.HasPrefix(entry, "OPENSBX_RUNTIME_PREFLIGHT_HELPER=") {
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env,
		"OPENSBX_RUNTIME_PREFLIGHT_HELPER=1",
		"PATH="+pathWithoutRuntime,
		"ADDR="+apiAddr,
		"OPENSBX_DATA_DIR="+filepath.Join(workDir, "data"),
		"LOG_FILE="+filepath.Join(workDir, "preflight.log"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("explicit container runtime unexpectedly passed preflight; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String()+stderr.String(), "runtime validation failed") {
		t.Fatalf("subprocess did not fail at runtime preflight: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	assertListenerReleased(t, apiAddr)
}

func TestMainStartsAndGracefullyShutsDownOnTermination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful child termination uses Unix SIGTERM")
	}
	db := database.New(":memory:")
	if err := docker.New(database.NewRepository(db)).Ping(context.Background()); err != nil {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		t.Skipf("Docker daemon unavailable for subprocess startup test: %v", err)
	}
	if sqlDB, dbErr := db.DB(); dbErr == nil {
		_ = sqlDB.Close()
	}
	workDir := t.TempDir()
	dataDir := filepath.Join(workDir, "data")
	logPath := filepath.Join(workDir, "logs", "api.log")
	apiAddr := reserveTCPAddress(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"OPENSBX_MAIN_HELPER=1",
		"ADDR="+apiAddr,
		"API_KEY=test-key",
		"OPENSBX_DATA_DIR="+dataDir,
		"LOG_FILE="+logPath,
	)
	// Share Go's standard coverage data directory with the subprocess. The Go
	// test runner merges both processes' counters into the ordinary coverprofile.
	if coverageDir := flagValue("test.gocoverdir"); coverageDir != "" {
		cmd.Args = append(cmd.Args, "-test.gocoverdir="+coverageDir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start main helper process: %v", err)
	}
	stopChild := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	serverPublished := false
	for time.Now().Before(deadline) {
		pid, running, err := processctl.Running(dataDir)
		if err == nil && running && pid == cmd.Process.Pid {
			serverPublished = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !serverPublished {
		stopChild()
		t.Fatalf("timed out waiting for the child to publish its ready process record; stdout: %s; stderr: %s", stdout.String(), stderr.String())
	}
	if !listenerResponds(client, apiAddr) {
		stopChild()
		t.Fatalf("API/proxy did not respond to HTTP readiness probes; stdout: %s; stderr: %s", stdout.String(), stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		stopChild()
		t.Fatalf("send termination signal to child process: %v", err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("main subprocess exited unsuccessfully: %v; stdout: %s; stderr: %s", err, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-finished
		t.Fatalf("main subprocess did not finish graceful shutdown; stdout: %s; stderr: %s", stdout.String(), stderr.String())
	}

	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read startup/shutdown log: %v", err)
	}
	for _, expected := range []string{"local API and sandbox URLs listening on", "shutting down: stopping incoming traffic", "shutting down: stopping tracked sandboxes", "server stopped"} {
		if !strings.Contains(string(logs), expected) {
			t.Errorf("main log omitted %q: %s", expected, logs)
		}
	}
	assertListenerReleased(t, apiAddr)
}

func TestStartAndStopCLIControlsBackgroundServer(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binName := "opensbx"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	bin := filepath.Join(t.TempDir(), binName)
	build := exec.Command("go", "build", "-o", bin, "./cmd/api")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build OpenSBX binary: %v\n%s", err, output)
	}

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.54")
		path = strings.TrimPrefix(path, "/v1.53")
		switch {
		case r.Method == http.MethodGet && path == "/_ping":
			_, _ = io.WriteString(w, "OK")
		case r.Method == http.MethodGet && path == "/version":
			_, _ = io.WriteString(w, `{"ApiVersion":"1.54","MinAPIVersion":"1.24"}`)
		case r.Method == http.MethodGet && path == "/info":
			_, _ = io.WriteString(w, `{"OSType":"linux","Architecture":"x86_64","ServerVersion":"27.1.0"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"unsupported synthetic Docker API endpoint"}`)
		}
	}))
	defer engine.Close()

	workDir := t.TempDir()
	dataDir := filepath.Join(workDir, "data")
	logPath := filepath.Join(workDir, "opensbx.log")
	addr := reserveTCPAddress(t)
	env := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "ADDR=") || strings.HasPrefix(item, "API_KEY=") || strings.HasPrefix(item, "BASE_DOMAIN=") || strings.HasPrefix(item, "DOCKER_HOST=") || strings.HasPrefix(item, "LOG_FILE=") || strings.HasPrefix(item, "OPENSBX_DATA_DIR=") || strings.HasPrefix(item, "PROXY_ADDR=") {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "DOCKER_HOST=tcp://"+strings.TrimPrefix(engine.URL, "http://"))
	startArgs := []string{"start", "-runtime", "docker", "-addr", addr, "-data-dir", dataDir, "-log-file", logPath}
	started := false
	stop := func() {
		if started {
			cmd := exec.Command(bin, "stop", "-data-dir", dataDir)
			cmd.Env = env
			_ = cmd.Run()
		}
	}
	t.Cleanup(stop)

	start := exec.Command(bin, startArgs...)
	start.Env = env
	output, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("opensbx start: %v\n%s", err, output)
	}
	started = true
	if !strings.Contains(string(output), "OpenSBX started") {
		t.Fatalf("start output=%s", output)
	}

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("server did not accept requests after start: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d, want %d", response.StatusCode, http.StatusOK)
	}

	second := exec.Command(bin, startArgs...)
	second.Env = env
	secondOutput, err := second.CombinedOutput()
	if err == nil || !strings.Contains(string(secondOutput), "already running") {
		t.Fatalf("duplicate start: err=%v output=%s", err, secondOutput)
	}

	stopCmd := exec.Command(bin, "stop", "-data-dir", dataDir)
	stopCmd.Env = env
	stopOutput, err := stopCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("opensbx stop: %v\n%s", err, stopOutput)
	}
	started = false
	if !strings.Contains(string(stopOutput), "OpenSBX stopped") {
		t.Fatalf("stop output=%s", stopOutput)
	}
	assertListenerReleased(t, addr)
}

func flagValue(name string) string {
	if value := flag.Lookup(name); value != nil {
		return value.Value.String()
	}
	return ""
}

func reserveTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved loopback address: %v", err)
	}
	return addr
}

func listenerResponds(client *http.Client, addr string) bool {
	response, err := client.Get("http://" + addr + "/readiness-probe")
	if response != nil {
		_ = response.Body.Close()
	}
	return err == nil
}

func assertListenerReleased(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("listener at %s remained open after process shutdown", addr)
	}
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("checking released listener at %s: %v", addr, err)
	}
}
