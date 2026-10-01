package docker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

func TestDockerIdentityScriptsHaveValidPOSIXShellSyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell syntax validation requires /bin/sh")
	}
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"probe", identityProbeScript},
		{"wrapped payload", identityWrapperScript},
		{"signal helper", identitySignalScript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/sh", "-n")
			command.Stdin = strings.NewReader(tc.script)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("actual %s script embedded in native Cmd is invalid POSIX shell syntax: %v; output=%q", tc.name, err, output)
			}
		})
	}
}

func TestIdentityStderrStripsOnlyOneFragmentedPreambleAndForwardsCoalescedPayload(t *testing.T) {
	const nonce = "cmd_0123456789012345678901234567890123456789"
	rc := &runningCommand{stderr: newRingBuffer(1024), identityReady: make(chan struct{})}
	filter := &identityStderr{rc: rc, nonce: nonce}
	first := "OPENSBX_EXEC " + nonce + " 234"
	if n, err := filter.Write([]byte(first)); err != nil || n != len(first) {
		t.Fatalf("fragmented first preamble write = %d, %v", n, err)
	}
	select {
	case <-rc.identityReady:
		t.Fatal("partial identity preamble published readiness")
	default:
	}
	coalesced := []byte("5 987654\npayload line\nOPENSBX_EXEC " + nonce + " 2345 987654\n")
	if n, err := filter.Write(coalesced); err != nil || n != len(coalesced) {
		t.Fatalf("completed/coalesced preamble write = %d, %v", n, err)
	}
	if n, err := filter.Write([]byte("later OPENSBX_EXEC " + nonce + " 2345 987654\n")); err != nil || n == 0 {
		t.Fatalf("subsequent payload write = %d, %v", n, err)
	}
	select {
	case <-rc.identityReady:
	default:
		t.Fatal("complete matching preamble did not publish readiness")
	}
	rc.mu.Lock()
	pid, start, identityErr := rc.guestPID, rc.guestStart, rc.identityErr
	rc.mu.Unlock()
	if pid != 2345 || start != 987654 || identityErr != nil {
		t.Fatalf("published guest identity = %d/%d err=%v", pid, start, identityErr)
	}
	want := "payload line\nOPENSBX_EXEC " + nonce + " 2345 987654\nlater OPENSBX_EXEC " + nonce + " 2345 987654\n"
	if got := string(rc.stderr.Bytes()); got != want {
		t.Fatalf("stderr after initial identity filter = %q; want exact subsequent payload %q", got, want)
	}
}

func TestIdentityStderrRejectsMalformedOversizedAndTruncatedPreambles(t *testing.T) {
	const nonce = "cmd_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name   string
		prefix string
		finish bool
	}{
		{"wrong nonce", "OPENSBX_EXEC other 2345 987654\n", false},
		{"noncanonical pid", "OPENSBX_EXEC " + nonce + " 02345 987654\n", false},
		{"zero start time", "OPENSBX_EXEC " + nonce + " 2345 0\n", false},
		{"overlong line", "OPENSBX_EXEC " + nonce + " 2345 987654 " + strings.Repeat("x", identityPreambleLimit) + "\n", false},
		{"truncated before newline", "OPENSBX_EXEC " + nonce + " 2345 987654", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &runningCommand{stderr: newRingBuffer(64), identityReady: make(chan struct{})}
			filter := &identityStderr{rc: rc, nonce: nonce}
			_, writeErr := filter.Write([]byte(tc.prefix))
			err := writeErr
			if tc.finish {
				err = filter.finish(io.EOF)
			}
			if err == nil {
				t.Fatal("invalid identity preamble was accepted")
			}
			select {
			case <-rc.identityReady:
			default:
				t.Fatal("invalid identity result did not wake readiness waiters")
			}
			rc.mu.Lock()
			defer rc.mu.Unlock()
			if rc.identityErr == nil || rc.guestPID != 0 || rc.guestStart != 0 {
				t.Fatalf("invalid identity publication = pid=%d start=%d err=%v", rc.guestPID, rc.guestStart, rc.identityErr)
			}
		})
	}
}

func TestDockerProbeDirectFallbackRequiresConfirmedCapabilityAbsence(t *testing.T) {
	request := runtimeio.ExecCommandRequest{Command: "node", Args: []string{"--version", "", "$(no-shell)", "line\nnext"}, Cwd: "/tmp/work dir", Env: map[string]string{"MODE": "test"}}
	for _, tc := range []struct {
		name        string
		config      func(*dockerAPIFixture)
		probeBefore bool
	}{
		{"shell builtin/proc prerequisite exit 125", func(f *dockerAPIFixture) { f.probeExitCode = 125 }, true},
		{"native reports missing shell", func(f *dockerAPIFixture) {
			f.probeCreateStatus = 500
			f.probeCreateError = `exec: "/bin/sh": executable file not found in $PATH`
		}, true},
		{"explicit shell-sensitive variable skips probe", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			fixture.containerEnv = []string{"PATH=/usr/bin:/bin"}
			if tc.config != nil {
				tc.config(fixture)
			}
			if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
				t.Fatal(err)
			}
			currentRequest := request
			if !tc.probeBefore {
				currentRequest.Env = map[string]string{"PWD": "/guest/work"}
			}
			command, err := dc.ExecCommand(context.Background(), "container-1", currentRequest)
			if err != nil {
				t.Fatalf("confirmed capability absence should retain direct exec compatibility: %v", err)
			}
			fixture.mu.Lock()
			creates := append([]dockerExecCreate(nil), fixture.execCreates...)
			fixture.mu.Unlock()
			wantCreates := 1
			if tc.probeBefore {
				wantCreates = 2
			}
			if len(creates) != wantCreates || tc.probeBefore && (creates[0].Kind != "probe" || creates[1].Kind != "direct") || !tc.probeBefore && creates[0].Kind != "direct" {
				t.Fatalf("native exec roles after safe direct fallback = %#v", creates)
			}
			wantArgv := append([]string{request.Command}, request.Args...)
			direct := creates[len(creates)-1]
			wantEnv := []string{"MODE=test"}
			if !tc.probeBefore {
				wantEnv = []string{"PWD=/guest/work"}
			}
			if !reflect.DeepEqual(direct.Cmd, wantArgv) || direct.WorkingDir != request.Cwd || !reflect.DeepEqual(direct.Env, wantEnv) {
				t.Fatalf("direct payload changed requested command data: %+v want argv=%#v cwd=%q env=%#v", direct, wantArgv, request.Cwd, wantEnv)
			}
			value, ok := dc.commands.Load(command.ID)
			if !ok {
				t.Fatal("direct payload was not tracked")
			}
			rc := value.(*runningCommand)
			rc.mu.Lock()
			identityErr := rc.identityErr
			rc.mu.Unlock()
			if !errors.Is(identityErr, sandbox.ErrUnsupported) {
				t.Fatalf("direct payload identity status = %v, want safe unsupported signal status", identityErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := dc.WaitCommand(ctx, "container-1", command.ID); err != nil {
				t.Fatalf("direct fallback command did not complete fixture attach: %v", err)
			}
		})
	}
}

func TestDockerProbeNativeFailureDoesNotRetryPayloadDirectly(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	fixture.probeCreateStatus = http.StatusInternalServerError
	fixture.probeCreateError = "permission denied while starting helper"
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.ExecCommand(context.Background(), "container-1", runtimeio.ExecCommandRequest{Command: "node", Args: []string{"script.js"}}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("probe native failure = %v; want propagated helper failure", err)
	}
	fixture.mu.Lock()
	creates := append([]dockerExecCreate(nil), fixture.execCreates...)
	fixture.mu.Unlock()
	if len(creates) != 1 || creates[0].Kind != "probe" {
		t.Fatalf("probe failure created payload/helper fallback commands: %#v", creates)
	}
}

func TestDockerProbeMalformedSuccessDoesNotRetryPayloadDirectly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config func(*dockerAPIFixture)
	}{
		{"wrong nonce", func(f *dockerAPIFixture) { f.probeStdout = "OPENSBX_PROBE wrong-nonce 2222 987654\n" }},
		{"unconfirmed nonzero exit", func(f *dockerAPIFixture) { f.probeExitCode = 1 }},
		{"stderr despite valid capability record", func(f *dockerAPIFixture) { f.probeStderr = "unexpected stderr\n" }},
		{"bounded probe output overflow", func(f *dockerAPIFixture) { f.probeStdout = strings.Repeat("x", identityPreambleLimit+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			tc.config(fixture)
			if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
				t.Fatal(err)
			}
			if _, err := dc.ExecCommand(context.Background(), "container-1", runtimeio.ExecCommandRequest{Command: "node"}); err == nil {
				t.Fatal("invalid probe response was accepted")
			}
			fixture.mu.Lock()
			creates := append([]dockerExecCreate(nil), fixture.execCreates...)
			fixture.mu.Unlock()
			if len(creates) != 1 || creates[0].Kind != "probe" {
				t.Fatalf("invalid probe was followed by payload retry: %#v", creates)
			}
		})
	}
}

func TestDockerProbeCancellationNeverFallsBackToDirectPayload(t *testing.T) {
	dc, fixture := newDockerFixture(t)
	fixture.probeBlock = true
	fixture.probeAttachEntered = make(chan struct{})
	if err := dc.repo.Save(database.Sandbox{ID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := dc.ExecCommand(ctx, "container-1", runtimeio.ExecCommandRequest{Command: "node", Args: []string{"script.js"}})
		result <- err
	}()
	select {
	case <-fixture.probeAttachEntered:
	case <-time.After(time.Second):
		t.Fatal("identity probe did not reach its held native attach")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("canceled probe error=%v; want caller deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("identity probe did not return on caller cancellation")
	}
	fixture.mu.Lock()
	creates := append([]dockerExecCreate(nil), fixture.execCreates...)
	fixture.mu.Unlock()
	if len(creates) != 1 || creates[0].Kind != "probe" {
		t.Fatalf("probe cancellation started or retried guest payload: %#v", creates)
	}
}

func TestDockerMalformedPayloadPreambleFailsClosedWithoutPayloadRetry(t *testing.T) {
	trace := &execStartupFixture{payloadPreamble: "OPENSBX_EXEC wrong-nonce 4321 987654\n"}
	dc, fixture, command := startFakePayloadExec(t, trace)
	select {
	case <-trace.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("payload did not enter its attach barrier")
	}
	trace.releaseStart()
	value, ok := dc.commands.Load(command.ID)
	if !ok {
		t.Fatal("accepted payload was not tracked before attach")
	}
	rc := value.(*runningCommand)
	select {
	case <-rc.identityReady:
	case <-time.After(2 * time.Second):
		t.Fatal("malformed wrapper preamble did not wake identity waiters")
	}
	if _, err := dc.KillCommand(context.Background(), "container-1", command.ID, 15); err == nil {
		t.Fatal("malformed payload identity allowed a command signal")
	}
	trace.finishOnce.Do(func() { close(trace.finishPayload) })
	select {
	case <-rc.done:
	case <-time.After(time.Second):
		t.Fatal("fixture payload stream did not close after releasing its owned completion barrier")
	}
	fixture.mu.Lock()
	creates := append([]dockerExecCreate(nil), fixture.execCreates...)
	signals := trace.signalExecCreates
	fixture.mu.Unlock()
	if len(creates) != 2 || creates[0].Kind != "probe" || creates[1].Kind != "payload" || signals != 0 {
		t.Fatalf("failed payload identity caused fallback/signal: execs=%#v signalHelpers=%d", creates, signals)
	}
}

func TestDockerNativeAttachFiltersFragmentedIdentityAndPreservesLaterMatchingText(t *testing.T) {
	trace := &execStartupFixture{
		payloadFinishOnStart: true,
		payloadPreambleChunks: [][]byte{
			[]byte("OPENSBX_EXEC "),
			[]byte("{{NONCE}} 4321 987"),
			[]byte("654\nfirst payload chunk\nOPENSBX_EXEC {{NONCE}} 4321 987654\n"),
		},
		payloadStderrTail: []byte("warning\n"),
	}
	dc, _, command := startFakeCommandExec(t, trace, runtimeio.ExecCommandRequest{Command: "printf", Args: []string{"payload"}})
	select {
	case <-trace.payloadStartEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("wrapped stream did not enter its attach barrier")
	}
	trace.releaseStart()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	finished, err := dc.WaitCommand(ctx, "container-1", command.ID)
	if err != nil || finished.ExitCode == nil || *finished.ExitCode != 0 {
		value, _ := dc.commands.Load(command.ID)
		rc := value.(*runningCommand)
		rc.mu.Lock()
		startErr, streamErr, identityErr, wasFinished := rc.startErr, rc.streamErr, rc.identityErr, rc.finished
		rc.mu.Unlock()
		t.Fatalf("wrapped payload completion=%+v err=%v startErr=%v streamErr=%v identityErr=%v finished=%t", finished, err, startErr, streamErr, identityErr, wasFinished)
	}
	logs, err := dc.GetCommandLogs(ctx, "container-1", command.ID)
	want := "first payload chunk\nOPENSBX_EXEC " + command.ID + " 4321 987654\nwarning\n"
	if err != nil || logs.Stderr != want {
		t.Fatalf("stderr after filtering only first fragmented preamble=%q err=%v; want %q", logs.Stderr, err, want)
	}
}
