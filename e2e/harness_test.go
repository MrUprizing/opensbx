//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const workload = "node:25-alpine"

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *lockedLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type harness struct {
	root, bin, runtime, data, endpoint, key, platform string
	cmd                                               *exec.Cmd
	done                                              chan struct{}
	waitErr                                           error
	log                                               *lockedLog
	logs                                              []*lockedLog
	http                                              *http.Client
	owned                                             map[string]string // public -> native, retained after API deletion
	cache                                             string
	baseline                                          map[string]bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	runtimeName := os.Getenv("OPENSBX_E2E_RUNTIME")
	if runtimeName == "" {
		runtimeName = "docker"
	}
	require.Contains(t, []string{"docker", "container"}, runtimeName)
	base := t.TempDir()
	h := &harness{root: filepath.Join(base, "state"), bin: filepath.Join(base, "opensbx"), runtime: runtimeName,
		key: "e2e-" + filepath.Base(base), owned: map[string]string{}, http: &http.Client{Timeout: 90 * time.Second,
			Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	h.data = filepath.Join(h.root, "data")
	require.NoError(t, os.MkdirAll(h.root, 0700))
	t.Cleanup(func() { h.cleanup(t) })
	_, err := exec.LookPath("curl")
	require.NoError(t, err, "curl is required to exercise returned .localhost URLs without Host/DNS overrides")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", h.bin, "./cmd/api")
	build.Dir = ".."
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build: %s", out)
	h.preflight(t)
	h.start(t)
	return h
}

func cleanEnv() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "ADDR", "API_KEY", "OPENSBX_DATA_DIR", "LOG_FILE", "PROXY_ADDR", "BASE_DOMAIN", "GIN_MODE":
			continue
		}
		env = append(env, item)
	}
	return env
}

var listenerLine = regexp.MustCompile(`local API and sandbox URLs listening on (127\.0\.0\.1:\d+)`)

func (h *harness) start(t *testing.T) {
	t.Helper()
	h.log = &lockedLog{}
	h.logs = append(h.logs, h.log)
	h.cmd = exec.Command(h.bin, "-runtime", h.runtime, "-addr", "127.0.0.1:0", "-data-dir", h.data,
		"-log-file", filepath.Join(h.root, "server.log"))
	h.cmd.Dir = h.root
	h.cmd.Env = append(cleanEnv(), "API_KEY="+h.key, "GIN_MODE=release")
	h.cmd.Stdout, h.cmd.Stderr = h.log, h.log
	require.NoError(t, h.cmd.Start())
	h.done = make(chan struct{})
	go func() { h.waitErr = h.cmd.Wait(); close(h.done) }()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			t.Fatalf("server exited during startup: %v\n%s", h.waitErr, h.log.text())
		default:
		}
		if m := listenerLine.FindStringSubmatch(h.log.text()); m != nil {
			h.endpoint = "http://" + m[1]
			status, _, err := h.request("GET", "/v1/health", nil, "", nil)
			if err == nil && status == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server readiness deadline exceeded\n%s", h.log.text())
}

func (h *harness) stop() error {
	if h.done == nil {
		return nil
	}
	select {
	case <-h.done:
		return h.waitErr
	default:
	}
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-h.done:
		return h.waitErr
	case <-time.After(65 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.done
		return fmt.Errorf("server failed to terminate within 65 seconds")
	}
}

func (h *harness) request(method, path string, body any, key string, headers map[string]string) (int, []byte, error) {
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return 0, nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, h.endpoint+path, &encoded)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

func (h *harness) api(t *testing.T, method, path string, body any, status int, into any) []byte {
	t.Helper()
	got, b, err := h.request(method, path, body, h.key, nil)
	require.NoError(t, err, "%s %s", method, path)
	require.Equal(t, status, got, "%s %s: %s", method, path, b)
	if into != nil {
		require.NoError(t, json.Unmarshal(b, into), "%s", b)
	}
	return b
}

func (h *harness) cli(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, append([]string{"image", args[0], "--data-dir", h.data, "--platform", h.platform}, args[1:]...)...)
	cmd.Dir, cmd.Env = h.root, cleanEnv()
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, "image CLI %v: %s", args, b)
	return b
}

// curl implements localhost routing itself. Pass the exact public URL, without
// --resolve, --connect-to, a Host override, or a custom Go dialer.
func appRequest(rawURL string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "curl", "--noproxy", "*", "--silent", "--show-error", "--max-time", "4",
		"--write-out", "\n%{http_code}", rawURL).Output()
	if err != nil {
		return 0, "", err
	}
	i := bytes.LastIndexByte(b, '\n')
	if i < 0 {
		return 0, "", fmt.Errorf("missing HTTP status in curl output")
	}
	var status int
	_, err = fmt.Sscanf(string(b[i+1:]), "%d", &status)
	return status, string(b[:i]), err
}

func eventually(t *testing.T, timeout time.Duration, description string, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var detail string
	for time.Now().Before(deadline) {
		var ok bool
		ok, detail = check()
		if ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: %s", description, detail)
}
