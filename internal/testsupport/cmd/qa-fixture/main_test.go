package main

import (
	"bufio"
	"bytes"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestQAFixtureProcessHelper(t *testing.T) {
	if os.Getenv("OPENSBX_QA_FIXTURE_HELPER") != "1" {
		return
	}
	main()
}

func TestSyntheticFixtureUsesProductionHostRouterAndProxyWithoutRuntimeResources(t *testing.T) {
	server, listener, cleanup, err := startFixture()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); cleanup() })
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	request := func(host, path string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("fixture request Host=%q path=%q: %v", host, path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}
	status, _, body := request("localhost:"+port, "/v1/health")
	if status != http.StatusOK || !strings.Contains(body, `"fixture":true`) {
		t.Fatalf("synthetic control route status=%d body=%q", status, body)
	}
	status, _, body = request("qa.localhost:"+port, "/")
	if status != http.StatusOK || !strings.Contains(body, "synthetic sandbox") || !strings.Contains(body, "path=/") {
		t.Fatalf("synthetic proxied root status=%d body=%q", status, body)
	}
	status, header, body := request("qa.localhost:"+port, "/api/events")
	if status != http.StatusOK || !strings.Contains(header.Get("Content-Type"), "text/event-stream") || !strings.Contains(body, "data: synthetic-upstream") {
		t.Fatalf("synthetic proxied SSE status=%d header=%v body=%q", status, header, body)
	}
	status, _, body = request("localhost:"+port, "/swagger/")
	if status != http.StatusOK || !strings.Contains(body, "src='/swagger/fixture.js'") {
		t.Fatalf("synthetic Swagger page status=%d body=%q", status, body)
	}
	status, header, body = request("localhost:"+port, "/swagger/fixture.js")
	if status != http.StatusOK || !strings.Contains(header.Get("Content-Type"), "javascript") || !strings.Contains(body, "dataset.fixture") {
		t.Fatalf("relative Swagger asset status=%d header=%v body=%q", status, header, body)
	}
	status, _, _ = request("evil.test:"+port, "/v1/health")
	if status != http.StatusMisdirectedRequest {
		t.Fatalf("unknown host status=%d", status)
	}
}

func TestQAFixtureCommandPrintsBrowserURLsAndShutsDownOnSignal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestQAFixtureProcessHelper$")
	if coverageDir := flag.Lookup("test.gocoverdir"); coverageDir != nil && coverageDir.Value.String() != "" { cmd.Args = append(cmd.Args, "-test.gocoverdir="+coverageDir.Value.String()) }
	cmd.Env = append(os.Environ(), "OPENSBX_QA_FIXTURE_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	var output strings.Builder
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ready := false
	for !ready {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("fixture exited before readiness: %s", stderr.String())
			}
			output.WriteString(line + "\n")
			ready = strings.HasPrefix(line, "Stop with Ctrl-C")
		case <-deadline.C:
			t.Fatalf("fixture readiness timeout; output=%s stderr=%s", output.String(), stderr.String())
		}
	}
	if !strings.Contains(output.String(), "http://qa.localhost:") || !strings.Contains(output.String(), "http://localhost:") {
		t.Fatalf("fixture did not print expected browser URLs: %s", output.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("fixture graceful signal exit: %v stderr=%s", err, stderr.String())
	}
	finished = true
}
