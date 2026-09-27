package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
)

func TestBoundedBufferDrainsAndRetainsConfiguredTailWithoutLosingOverflow(t *testing.T) {
	b := &boundedBuffer{limit: 5, tail: true}
	if n, err := io.WriteString(b, "abc"); n != 3 || err != nil {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if n, err := io.WriteString(b, "defgh"); n != 5 || err != nil {
		t.Fatalf("overflow write = %d, %v", n, err)
	}
	got, overflow := b.snapshot()
	if got != "defgh" || !overflow || b.total != 8 {
		t.Fatalf("tail snapshot = %q overflow=%v total=%d", got, overflow, b.total)
	}
	protocol := &boundedBuffer{limit: 3}
	_, _ = io.WriteString(protocol, "abcdef")
	got, overflow = protocol.snapshot()
	if got != "abc" || !overflow {
		t.Fatalf("protocol overflow snapshot = %q, %v", got, overflow)
	}
}

func TestLogReaderClosesAndReturnsEOFAfterBufferedOutput(t *testing.T) {
	b := &boundedBuffer{limit: 8, tail: true}
	done := make(chan struct{})
	close(done)
	r := &logReader{buffer: b, done: done, closed: make(chan struct{})}
	_, _ = io.WriteString(b, "hello")
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "hello" {
		t.Fatalf("ReadAll() = %q, %v", data, err)
	}
	if _, err := r.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after drain = %v, want EOF", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err != io.ErrClosedPipe {
		t.Fatalf("read after Close = %v, want closed pipe", err)
	}
}

func TestSafeErrorDoesNotExposeNativeDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"native", errors.New("secret path /Users/private/token"), "CLI execution failed"},
		{"canceled", context.Canceled, "context canceled"},
		{"timeout", context.DeadlineExceeded, "context deadline exceeded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := safeError(tc.err)
			if got.Error() != tc.want || strings.Contains(got.Error(), "private") {
				t.Fatalf("safeError() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProtocolOutputOverflowIsRejectedInsteadOfParsedTruncated(t *testing.T) {
	r := &scriptedRunner{t: t, run: func(_ []string, _ io.Reader, out, _ io.Writer) error {
		_, _ = io.WriteString(out, strings.Repeat("x", outputLimit+1))
		return nil
	}}
	c, _ := testClient(t, r)
	if _, err := c.run(context.Background(), nil, "system", "status", "--format", "json"); err == nil || !strings.Contains(err.Error(), fmt.Sprint(outputLimit)) {
		t.Fatalf("oversized protocol output error = %v", err)
	}
}

func TestCLIEnvironmentExcludesUntrustedHostVariables(t *testing.T) {
	t.Setenv("CONTAINER_SECRET", "must-not-forward")
	t.Setenv("DYLD_INSERT_LIBRARIES", "must-not-forward")
	t.Setenv("CUSTOM_SECRET", "must-not-forward")
	env := strings.Join(cliEnv(), "\n")
	for _, required := range []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"} {
		if !strings.Contains(env, required) {
			t.Errorf("CLI environment omitted %q: %s", required, env)
		}
	}
	for _, forbidden := range []string{"must-not-forward", "CONTAINER_SECRET=", "DYLD_INSERT_LIBRARIES=", "CUSTOM_SECRET="} {
		if strings.Contains(env, forbidden) {
			t.Errorf("CLI environment inherited %q: %s", forbidden, env)
		}
	}
}

func TestResolveFailsClosedOnNonAppleHostWithoutLaunchingAnything(t *testing.T) {
	_, err := Resolve(context.Background())
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if err == nil {
			t.Skip("Apple container CLI is installed; no user service was started or contacted")
		}
		if !strings.Contains(err.Error(), "Apple container CLI not found") {
			t.Fatalf("Resolve() prerequisite error = %v, want missing CLI", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "Apple Silicon macOS") {
		t.Fatalf("Resolve() on a non-Apple test host = %v", err)
	}
}

func TestResolveReportsMissingCLIWithoutAttemptingInstallation(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" { t.Skip("Apple Silicon host prerequisite") }
	path := t.TempDir()
	t.Setenv("PATH", path)
	runner, err := Resolve(context.Background())
	if err == nil || runner != nil || !strings.Contains(err.Error(), "Apple container CLI not found") { t.Fatalf("Resolve() missing PATH CLI result runner=%v err=%v", runner, err) }
}
