package main

import (
	"bytes"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMainProcessHelper(t *testing.T) {
	if os.Getenv("OPENSBX_MAIN_HELPER") != "1" {
		return
	}
	os.Args = []string{"opensbx-main-helper"}
	main()
}

func TestMainStartsAndGracefullyShutsDownOnTermination(t *testing.T) {
	workDir := t.TempDir()
	logPath := filepath.Join(workDir, "logs", "api.log")
	apiAddr := reserveTCPAddress(t)
	proxyAddr := reserveTCPAddress(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"OPENSBX_MAIN_HELPER=1",
		"ADDR="+apiAddr,
		"API_KEY=test-key",
		"PROXY_ADDR="+proxyAddr,
		"BASE_DOMAIN=localhost",
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
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + apiAddr + "/readiness-probe")
		apiReady := err == nil
		if response != nil {
			_ = response.Body.Close()
		}
		proxyResponse, proxyErr := client.Get("http://" + proxyAddr + "/readiness-probe")
		proxyReady := proxyErr == nil
		if proxyResponse != nil {
			_ = proxyResponse.Body.Close()
		}
		if apiReady && proxyReady {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		stopChild()
		t.Fatalf("timed out waiting for actual API/proxy readiness; stdout: %s; stderr: %s", stdout.String(), stderr.String())
	}
	if !listenerResponds(client, apiAddr) || !listenerResponds(client, proxyAddr) {
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
	for _, expected := range []string{"proxy listening on", "api listening on", "shutting down: stopping incoming traffic", "shutting down: stopping tracked sandboxes", "server stopped"} {
		if !strings.Contains(string(logs), expected) {
			t.Errorf("main log omitted %q: %s", expected, logs)
		}
	}
	assertListenerReleased(t, apiAddr)
	assertListenerReleased(t, proxyAddr)
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
