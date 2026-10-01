//go:build e2e && performance

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
	"opensbx/internal/images"
	"opensbx/models"
)

const performanceProxyMarker = "opensbx-performance-ok"

func TestPerformance(t *testing.T) {
	settings, err := parsePerformanceSettings(environmentMap())
	require.NoError(t, err, "invalid performance configuration must fail before live setup")
	reportPath, err := performanceReportPath(settings)
	require.NoError(t, err)

	report := &performanceReport{
		SchemaVersion: 2,
		StartedAt:     time.Now().UTC(),
		Environment: performanceEnvironment{
			OS: runtime.GOOS, Architecture: runtime.GOARCH, GoVersion: runtime.Version(),
			LoadGenerator: "Go net/http fixed-arrival-rate", ConnectionReuse: true,
			ConnectionPolicy: "shared Go HTTP transport with keep-alive; sandbox.localhost dialed at 127.0.0.1 with original Host",
		},
		Configuration: performanceRunConfiguration{
			RatePerSecond: settings.rate, Duration: settings.duration.String(),
			MaxConcurrentRequests: settings.maxVUs,
			Iterations:            settings.iterations, RequestTimeout: settings.requestTimeout.String(),
			TimingUnit: "milliseconds", Scope: "local OpenSBX process and loopback HTTP; excludes setup, warm-up, image preparation, and cleanup",
		},
		Dataset: performanceDataset{
			Sandboxes: 1, CommandHistory: settings.commandHistory + 1,
			ConcurrentRuntimeSandboxes: 2, SandboxMemoryMiB: performanceSandboxMemory(settings.runtime), SandboxCPUs: 1,
		},
		Measurements: []*performanceSeries{},
		Process:      processObservation{Scope: "OpenSBX server process only; excludes runtime daemons and the Go test/load-generator process", Method: "one-second macOS/Linux ps samples; CPU is ps %cpu, RSS is ps resident kilobytes", Samples: 0, Unavailable: "load phase did not start"},
	}
	for _, spec := range []struct{ name, scope string }{
		{"lifecycle/create", "one local HTTP POST including request JSON encoding, response transfer and body read; excludes ownership verification"},
		{"lifecycle/stop", "one local HTTP POST; excludes native inventory verification"},
		{"lifecycle/start", "one local HTTP POST; excludes native inventory verification"},
		{"lifecycle/delete", "one local HTTP DELETE; excludes post-delete inventory verification"},
		{"exec/acceptance", "one local HTTP POST returning command registration; excludes guest completion"},
		{"exec/completion", "elapsed from exec request start until API reports non-nil guest exit code; includes polling"},
		{"logs/first_data", "GET request start to first non-empty stdout/stderr NDJSON record, which may be retained output; excludes headers/empty records and does not claim guest emit latency"},
		{"http/load", "Go fixed-arrival-rate scheduling window plus completion of pending requests; no latency threshold"},
	} {
		report.Measurements = append(report.Measurements, newPerformanceSeries(spec.name, "ms", spec.scope))
	}
	t.Cleanup(func() {
		report.FinishedAt = time.Now().UTC()
		report.TestFailed = t.Failed()
		if err := writePerformanceReport(reportPath, report); err != nil {
			t.Errorf("write performance report: %v", err)
			return
		}
		printPerformanceSummary(t, report, reportPath)
	})

	// The harness cleanup is registered after the report callback; Go executes
	// cleanups in reverse order, so the artifact captures cleanup failures too.
	t.Setenv("OPENSBX_E2E_RUNTIME", settings.runtime)
	h := newHarness(t)
	require.NotNil(t, h)

	h.images(t)
	platform, err := v1.ParsePlatform(h.platform)
	require.NoError(t, err)
	store, err := images.Open(h.data)
	require.NoError(t, err)
	artifact, err := store.Resolve(context.Background(), importedImage, *platform)
	require.NoError(t, err, "prepared OCI workload must resolve locally before measurement")
	report.Runtime = performanceRuntimeMetadata{
		Name: settings.runtime, Version: performanceRuntimeVersion(t, h),
		Image: importedImage, SourceImage: workload, Digest: artifact.Manifest.Digest.String(),
	}
	report.Environment.Revision, report.Environment.Dirty = performanceRevision(t)

	// Materialize the selected image cache and exercise one complete lifecycle
	// before collecting lifecycle values. The warm-up resource is removed exactly.
	warm := createPerformanceSandbox(t, h, nil)
	h.remove(t, warm.ID)
	main := createPerformanceSandbox(t, h, []string{"3000"})
	startPerformanceNode(t, h, main.ID)
	eventually(t, 20*time.Second, "performance app readiness", func() (bool, string) {
		status, body, requestErr := appRequest(main.URL + "/")
		return requestErr == nil && status == http.StatusOK && body == performanceProxyMarker,
			fmt.Sprintf("status=%d body=%q err=%v", status, body, requestErr)
	})
	for i := 0; i < settings.commandHistory; i++ {
		command := h.command(t, main.ID, "node", "-e", "process.exit(0)")
		finished := h.waitCommand(t, main.ID, string(command.ID))
		require.NotNil(t, finished.ExitCode)
		require.Zero(t, *finished.ExitCode, "history preload command must succeed")
	}
	var history models.CommandListResponse
	h.api(t, http.MethodGet, "/v1/sandboxes/"+main.ID+"/cmd", nil, http.StatusOK, &history)
	require.Len(t, history.Commands, settings.commandHistory+1, "preloaded command history plus proxy process must remain fixed during HTTP load")

	if !measureRealLifecycle(t, h, settings, report) {
		seriesByName(report, "http/load").fail("fixture_incomplete", "load phase not run after lifecycle resource uncertainty")
		report.Warnings = append(report.Warnings, "HTTP load and later measurements were not run because lifecycle ownership/state was uncertain; exact harness cleanup will reconcile captured owners")
		return
	}
	measureCommandAndLogs(t, h, main.ID, settings, report)
	var fixedHistory models.CommandListResponse
	h.api(t, http.MethodGet, "/v1/sandboxes/"+main.ID+"/cmd", nil, http.StatusOK, &fixedHistory)
	require.GreaterOrEqual(t, len(fixedHistory.Commands), settings.commandHistory+1, "measured workload and proxy server command history must remain observable")
	report.Dataset.CommandHistory = len(fixedHistory.Commands)
	processSampler := startProcessSampler(h.cmd.Process.Pid)
	samplingFinished := false
	t.Cleanup(func() {
		if !samplingFinished {
			report.Process = processSampler.stop()
		}
	})
	measureHTTPPerformance(t, h, main.ID, main.URL, len(fixedHistory.Commands), settings, report)
	report.Process = processSampler.stop()
	samplingFinished = true

	// Main sandbox deletion is deliberately left to the exact-ownership harness
	// cleanup, which validates API/native/database/cache absence.
}

func environmentMap() map[string]string {
	values := map[string]string{}
	for _, key := range []string{
		"OPENSBX_E2E_RUNTIME", "OPENSBX_PERF_ITERATIONS", "OPENSBX_PERF_COMMAND_HISTORY",
		"OPENSBX_PERF_RATE", "OPENSBX_PERF_DURATION",
		"OPENSBX_PERF_MAX_VUS", "OPENSBX_PERF_REQUEST_TIMEOUT", "OPENSBX_PERF_ARTIFACTS",
	} {
		values[key] = os.Getenv(key)
	}
	return values
}

func performanceReportPath(settings performanceSettings) (reportPath string, err error) {
	directory := settings.artifactDirectory
	if directory == "" {
		directory, err = os.MkdirTemp("", "opensbx-performance-reports-")
		if err != nil {
			return "", fmt.Errorf("create retained temporary report directory: %w", err)
		}
	} else {
		if !filepath.IsAbs(directory) {
			return "", fmt.Errorf("OPENSBX_PERF_ARTIFACTS must be an absolute directory")
		}
		if err := os.MkdirAll(directory, 0700); err != nil {
			return "", fmt.Errorf("create performance artifact directory: %w", err)
		}
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	base := fmt.Sprintf("opensbx-performance-%s-%s-%d", settings.runtime, stamp, os.Getpid())
	reportPath = filepath.Join(directory, base+".json")
	if _, statErr := os.Stat(reportPath); statErr == nil {
		return "", fmt.Errorf("refusing to overwrite existing report %s", reportPath)
	}
	return reportPath, nil
}

func performanceRuntimeVersion(t *testing.T, h *harness) string {
	t.Helper()
	var output []byte
	var err error
	if h.runtime == "docker" {
		output, err = h.native("version", "--format", "{{.Server.Version}}")
	} else {
		output, err = h.native("--version")
	}
	require.NoError(t, err, "read runtime version")
	return strings.TrimSpace(string(output))
}

func performanceRevision(t *testing.T) (string, *bool) {
	t.Helper()
	revision, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unavailable", nil
	}
	dirtyOutput, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return strings.TrimSpace(string(revision)), nil
	}
	dirty := len(bytes.TrimSpace(dirtyOutput)) != 0
	return strings.TrimSpace(string(revision)), &dirty
}

func seriesByName(report *performanceReport, name string) *performanceSeries {
	for _, series := range report.Measurements {
		if series.Name == name {
			return series
		}
	}
	panic("missing performance series " + name)
}

func timedPerformanceRequest(h *harness, method, path string, body any, expectedStatus int) (time.Duration, []byte, error) {
	started := time.Now()
	status, response, err := h.request(method, path, body, h.key, nil)
	elapsed := time.Since(started)
	if err != nil {
		return elapsed, nil, err
	}
	if status != expectedStatus {
		return elapsed, nil, fmt.Errorf("HTTP status %d, expected %d", status, expectedStatus)
	}
	return elapsed, response, nil
}

func createPerformanceSandbox(t *testing.T, h *harness, ports []string) models.CreateSandboxResponse {
	t.Helper()
	var created models.CreateSandboxResponse
	h.api(t, http.MethodPost, "/v1/sandboxes", models.CreateSandboxRequest{
		Image: importedImage, Ports: ports, Timeout: 300,
		Resources: &models.ResourceLimits{Memory: performanceSandboxMemory(h.runtime), CPUs: 1},
	}, http.StatusCreated, &created)
	require.NoError(t, h.captureOwnership())
	require.Contains(t, h.owned, created.ID)
	states, err := h.inventory()
	require.NoError(t, err)
	require.Equal(t, "running", states[h.owned[created.ID]], "created performance resource must run in selected runtime")
	return created
}

func performanceSandboxMemory(runtime string) int64 {
	if runtime == "container" {
		// Apple Container's Linux guest has higher fixed VM startup needs; retain
		// its supported 1 GiB default rather than under-provisioning the VM.
		return 1024
	}
	return 128
}

type lifecycleFailure struct {
	kind    string
	err     error
	fixture bool
}

func (failure *lifecycleFailure) Error() string { return failure.err.Error() }
func (failure *lifecycleFailure) Unwrap() error { return failure.err }

type lifecycleSampleOps struct {
	create        func(int) (string, time.Duration, error)
	verifyRunning func(string) error
	stop          func(string) (time.Duration, error)
	verifyStopped func(string) error
	start         func(string) (time.Duration, error)
	verifyStarted func(string) error
	delete        func(string) (time.Duration, error)
	verifyDeleted func(string) error
}

func measureRealLifecycle(t *testing.T, h *harness, settings performanceSettings, report *performanceReport) bool {
	t.Helper()
	ops := lifecycleSampleOps{
		create: func(sample int) (string, time.Duration, error) {
			request := models.CreateSandboxRequest{Image: importedImage, Timeout: 300,
				Resources: &models.ResourceLimits{Memory: performanceSandboxMemory(h.runtime), CPUs: 1}}
			elapsed, response, requestErr := timedPerformanceRequest(h, http.MethodPost, "/v1/sandboxes", request, http.StatusCreated)
			if err := h.captureOwnership(); err != nil {
				return "", elapsed, &lifecycleFailure{kind: "fixture", err: fmt.Errorf("capture ownership after measured create: %w", err), fixture: true}
			}
			if requestErr != nil {
				return "", elapsed, &lifecycleFailure{kind: "http", err: requestErr}
			}
			var created models.CreateSandboxResponse
			if err := json.Unmarshal(response, &created); err != nil || created.ID == "" {
				return "", elapsed, &lifecycleFailure{kind: "response", err: fmt.Errorf("sample %d: create response did not contain a usable sandbox ID", sample+1)}
			}
			if _, owned := h.owned[created.ID]; !owned {
				return "", elapsed, &lifecycleFailure{kind: "fixture", err: fmt.Errorf("created sandbox is absent from the private ownership database"), fixture: true}
			}
			return created.ID, elapsed, nil
		},
		verifyRunning: func(id string) error { return verifyPerformanceNativeState(h, id, "running") },
		stop: func(id string) (time.Duration, error) {
			elapsed, _, err := timedPerformanceRequest(h, http.MethodPost, "/v1/sandboxes/"+id+"/stop", nil, http.StatusOK)
			return elapsed, err
		},
		verifyStopped: func(id string) error {
			states, err := h.inventory()
			if err != nil {
				return err
			}
			state, exists := states[h.owned[id]]
			if !exists || state != "stopped" && state != "exited" {
				return fmt.Errorf("sandbox %s did not reach stopped state", id)
			}
			return nil
		},
		start: func(id string) (time.Duration, error) {
			elapsed, _, err := timedPerformanceRequest(h, http.MethodPost, "/v1/sandboxes/"+id+"/start", nil, http.StatusOK)
			return elapsed, err
		},
		verifyStarted: func(id string) error { return verifyPerformanceNativeState(h, id, "running") },
		delete: func(id string) (time.Duration, error) {
			elapsed, _, err := timedPerformanceRequest(h, http.MethodDelete, "/v1/sandboxes/"+id, nil, http.StatusNoContent)
			return elapsed, err
		},
		verifyDeleted: func(id string) error {
			status, _, err := h.request(http.MethodGet, "/v1/sandboxes/"+id, nil, h.key, nil)
			if err != nil || status != http.StatusNotFound {
				return fmt.Errorf("public sandbox remains after delete (status=%d): %v", status, err)
			}
			states, err := h.inventory()
			if err != nil {
				return err
			}
			if _, exists := states[h.owned[id]]; exists {
				return fmt.Errorf("native sandbox remains after delete")
			}
			return nil
		},
	}
	return runLifecycleSamples(t, settings.iterations, ops, report)
}

func verifyPerformanceNativeState(h *harness, id, expected string) error {
	states, err := h.inventory()
	if err != nil {
		return err
	}
	if states[h.owned[id]] != expected {
		return fmt.Errorf("sandbox %s native state is %q, expected %q", id, states[h.owned[id]], expected)
	}
	return nil
}

func runLifecycleSamples(t *testing.T, iterations int, ops lifecycleSampleOps, report *performanceReport) bool {
	t.Helper()
	create := seriesByName(report, "lifecycle/create")
	stop := seriesByName(report, "lifecycle/stop")
	start := seriesByName(report, "lifecycle/start")
	remove := seriesByName(report, "lifecycle/delete")
	all := []*performanceSeries{create, stop, start, remove}
	fail := func(index int, current *performanceSeries, kind string, err error, dependencies ...*performanceSeries) {
		var detail string
		var fixture *lifecycleFailure
		if errors.As(err, &fixture) {
			kind = fixture.kind
			detail = fixture.Error()
			if fixture.fixture {
				t.Errorf("performance lifecycle fixture invalid: %v", fixture)
			}
		} else if err != nil {
			detail = err.Error()
		}
		current.fail(kind, fmt.Sprintf("sample %d: %s", index+1, detail))
		for _, dependent := range dependencies {
			dependent.fail("dependency", fmt.Sprintf("sample %d not run after lifecycle failure", index+1))
		}
		for sample := index + 1; sample < iterations; sample++ {
			for _, series := range all {
				series.fail("not_run", fmt.Sprintf("sample %d not run after lifecycle failure; no further create attempted", sample+1))
			}
		}
	}
	verificationFailure := func(index int, current *performanceSeries, kind string, err error, dependencies ...*performanceSeries) {
		current.noteError(kind, fmt.Sprintf("sample %d: %v", index+1, err))
		for _, dependent := range dependencies {
			dependent.fail("dependency", fmt.Sprintf("sample %d not run after lifecycle state verification failure", index+1))
		}
		for sample := index + 1; sample < iterations; sample++ {
			for _, series := range all {
				series.fail("not_run", fmt.Sprintf("sample %d not run after lifecycle verification failure; no further create attempted", sample+1))
			}
		}
	}
	for sample := 0; sample < iterations; sample++ {
		id, elapsed, err := ops.create(sample)
		if err != nil {
			fail(sample, create, "http", err, stop, start, remove)
			return false
		}
		if id == "" {
			fail(sample, create, "response", fmt.Errorf("create returned no owned sandbox ID"), stop, start, remove)
			return false
		}
		create.add(elapsed)
		if err := ops.verifyRunning(id); err != nil {
			verificationFailure(sample, create, "runtime_state", err, stop, start, remove)
			return false
		}
		elapsed, err = ops.stop(id)
		if err != nil {
			fail(sample, stop, "http", err, start, remove)
			return false
		}
		stop.add(elapsed)
		if err := ops.verifyStopped(id); err != nil {
			verificationFailure(sample, stop, "runtime_state", err, start, remove)
			return false
		}
		elapsed, err = ops.start(id)
		if err != nil {
			fail(sample, start, "http", err, remove)
			return false
		}
		start.add(elapsed)
		if err := ops.verifyStarted(id); err != nil {
			verificationFailure(sample, start, "runtime_state", err, remove)
			return false
		}
		elapsed, err = ops.delete(id)
		if err != nil {
			fail(sample, remove, "http", err)
			return false
		}
		remove.add(elapsed)
		if err := ops.verifyDeleted(id); err != nil {
			verificationFailure(sample, remove, "cleanup_verification", err)
			return false
		}
	}
	return true
}

func startPerformanceNode(t *testing.T, h *harness, sandboxID string) {
	t.Helper()
	program := "require('http').createServer((_,res)=>res.end('" + performanceProxyMarker + "')).listen(3000,'0.0.0.0')"
	command := h.command(t, sandboxID, "node", "-e", program)
	require.NotEmpty(t, command.ID)
}

func measureCommandAndLogs(t *testing.T, h *harness, sandboxID string, settings performanceSettings, report *performanceReport) {
	t.Helper()
	acceptance := seriesByName(report, "exec/acceptance")
	completion := seriesByName(report, "exec/completion")
	firstData := seriesByName(report, "logs/first_data")
	for i := 0; i < settings.iterations; i++ {
		program := "process.stdout.write('perf-first-data');setTimeout(()=>process.exit(0),150)"
		started := time.Now()
		elapsed, response, err := timedPerformanceRequest(h, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/cmd", models.ExecCommandRequest{Command: "node", Args: []string{"-e", program}}, http.StatusOK)
		commandStarted := started
		if err != nil {
			acceptance.fail("http", fmt.Sprintf("sample %d: %v", i+1, err))
			completion.fail("dependency", fmt.Sprintf("sample %d: command was not accepted", i+1))
			firstData.fail("dependency", fmt.Sprintf("sample %d: command was not accepted", i+1))
			continue
		}
		acceptance.add(elapsed)
		var result models.CommandResponse
		if err := json.Unmarshal(response, &result); err != nil || result.Command.ID == "" {
			completion.fail("response", fmt.Sprintf("sample %d: command response omitted command ID", i+1))
			firstData.fail("dependency", fmt.Sprintf("sample %d: no usable command response", i+1))
			continue
		}
		waited := make(chan commandWaitResult, 1)
		go func(id string) {
			finished, duration, waitErr := waitPerformanceCommand(h, sandboxID, id, commandStarted, 15*time.Second)
			waited <- commandWaitResult{command: finished, elapsed: duration, err: waitErr}
		}(result.Command.ID)
		streamElapsed, streamErr := firstPerformanceLogData(h, sandboxID, result.Command.ID, settings.requestTimeout)
		if streamErr != nil {
			firstData.fail("stream", fmt.Sprintf("sample %d: %v", i+1, streamErr))
		} else {
			firstData.add(streamElapsed)
		}
		waitResult := <-waited
		if waitResult.err != nil {
			completion.fail("command", fmt.Sprintf("sample %d: %v", i+1, waitResult.err))
			continue
		}
		if waitResult.command.ExitCode == nil {
			completion.fail("command", fmt.Sprintf("sample %d: completion had no exit code", i+1))
			continue
		}
		if *waitResult.command.ExitCode != 0 {
			completion.fail("command", fmt.Sprintf("sample %d: guest exit code %d", i+1, *waitResult.command.ExitCode))
			continue
		}
		completion.add(waitResult.elapsed)
	}
}

type commandWaitResult struct {
	command models.CommandDetail
	elapsed time.Duration
	err     error
}

func firstPerformanceLogData(h *harness, sandboxID, commandID string, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	endpoint := strings.TrimPrefix(h.endpoint, "http://")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.endpoint+"/v1/sandboxes/"+sandboxID+"/cmd/"+commandID+"/logs?stream=true", nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+h.key)
	started := time.Now()
	response, err := h.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("open stream at %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("stream status %d", response.StatusCode)
	}
	reader := bufio.NewReader(io.LimitReader(response.Body, 1<<20))
	firstAt, err := readFirstPerformanceLogData(reader, time.Now)
	if err != nil {
		return 0, err
	}
	return firstAt.Sub(started), nil
}

func readFirstPerformanceLogData(reader *bufio.Reader, now func() time.Time) (time.Time, error) {
	var firstDataAt time.Time
	var payload bytes.Buffer
	markerStream := ""
	for records := 0; records < 128; records++ {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return time.Time{}, fmt.Errorf("stream ended before the expected marker was observed")
			}
			return time.Time{}, fmt.Errorf("read stream data record: %w", err)
		}
		var record struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &record); err != nil {
			return time.Time{}, fmt.Errorf("decode stream record")
		}
		if record.Type == "error" {
			return time.Time{}, fmt.Errorf("stream returned an error record")
		}
		if (record.Type == "stdout" || record.Type == "stderr") && record.Data != "" {
			if firstDataAt.IsZero() {
				firstDataAt = now()
			}
			if markerStream != record.Type {
				payload.Reset()
				markerStream = record.Type
			}
			if payload.Len()+len(record.Data) > 1<<20 {
				return time.Time{}, fmt.Errorf("log marker not found within the bounded 1 MiB read")
			}
			payload.WriteString(record.Data)
			if bytes.Contains(payload.Bytes(), []byte("perf-first-data")) {
				return firstDataAt, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("stream contained no data record within the bounded read")
}

func waitPerformanceCommand(h *harness, sandboxID, commandID string, started time.Time, timeout time.Duration) (models.CommandDetail, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.endpoint+"/v1/sandboxes/"+sandboxID+"/cmd/"+commandID, nil)
		if err != nil {
			return models.CommandDetail{}, time.Since(started), err
		}
		request.Header.Set("Authorization", "Bearer "+h.key)
		response, err := h.http.Do(request)
		if err != nil {
			return models.CommandDetail{}, time.Since(started), err
		}
		var result models.CommandResponse
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
		_ = response.Body.Close()
		if decodeErr != nil {
			return models.CommandDetail{}, time.Since(started), decodeErr
		}
		if response.StatusCode != http.StatusOK {
			return models.CommandDetail{}, time.Since(started), fmt.Errorf("command status request returned HTTP %d", response.StatusCode)
		}
		if result.Command.ExitCode != nil {
			return result.Command, time.Since(started), nil
		}
		select {
		case <-ctx.Done():
			return models.CommandDetail{}, time.Since(started), fmt.Errorf("command completion observation timed out")
		case <-ticker.C:
		}
	}
}

type processSampler struct {
	stop func() processObservation
}

func startProcessSampler(pid int) processSampler {
	stop := make(chan struct{})
	done := make(chan processObservation, 1)
	go func() {
		observation := processObservation{
			Scope:  "OpenSBX server process only; excludes runtime daemons and the Go test/load-generator process",
			Method: "one-second macOS/Linux ps samples; CPU is ps %cpu, RSS is ps resident kilobytes",
		}
		var cpus, rss []float64
		collect := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "ps", "-o", "%cpu=", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
			if err != nil {
				observation.Unavailable = "ps process sampling is unsupported or failed"
				return
			}
			var cpu, memory float64
			if _, err := fmt.Sscan(string(out), &cpu, &memory); err != nil || cpu < 0 || memory < 0 {
				observation.Unavailable = "ps returned an unparseable process sample"
				return
			}
			cpus, rss = append(cpus, cpu), append(rss, memory)
		}
		collect()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				observation.Samples = len(cpus)
				observation.CPUPercent = summarizeResources(cpus)
				observation.RSSKB = summarizeResources(rss)
				if observation.Samples == 0 && observation.Unavailable == "" {
					observation.Unavailable = "no process samples were collected"
				}
				done <- observation
				return
			case <-ticker.C:
				collect()
			}
		}
	}()
	return processSampler{stop: func() processObservation { close(stop); return <-done }}
}

func printPerformanceSummary(t *testing.T, report *performanceReport, path string) {
	t.Helper()
	t.Logf("Performance JSON report: %s", path)
	for _, series := range report.Measurements {
		if series.Statistics == nil {
			t.Logf("%s: samples=%d missing=%d error_records=%d latency=unavailable", series.Name, series.SampleCount, series.MissingCount, len(series.Errors))
			continue
		}
		t.Logf("%s: samples=%d missing=%d error_records=%d min=%.3fms mean=%.3fms p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms", series.Name, series.SampleCount, series.MissingCount, len(series.Errors), series.Statistics.MinMS, series.Statistics.MeanMS, series.Statistics.P50MS, series.Statistics.P95MS, series.Statistics.P99MS, series.Statistics.MaxMS)
	}
	if report.HTTP != nil {
		t.Logf("http offered=%d at %.2f/s achieved=%d dropped=%d", report.HTTP.OfferedIterations, report.HTTP.OfferedRate, report.HTTP.AchievedIterations, report.HTTP.DroppedIterations)
		endpoints := make([]string, 0, len(report.HTTP.Endpoints))
		for endpoint := range report.HTTP.Endpoints {
			endpoints = append(endpoints, endpoint)
		}
		sort.Strings(endpoints)
		for _, name := range endpoints {
			result := report.HTTP.Endpoints[name]
			latency := "unavailable"
			if result.Latency != nil {
				latency = fmt.Sprintf("n=%d min=%.3fms mean=%.3fms p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms",
					result.Latency.Count, result.Latency.MinMS, result.Latency.MeanMS, result.Latency.P50MS,
					result.Latency.P95MS, result.Latency.P99MS, result.Latency.MaxMS)
			}
			t.Logf("http/%s: requests=%d successes=%d http_failures=%d transport_errors=%d timeouts=%d latency={%s}", name,
				result.Requests, result.HTTPSuccesses, result.HTTPFailures,
				result.TransportErrors, result.Timeouts, latency)
		}
	}
	if report.Process.Unavailable != "" {
		t.Logf("process metrics unavailable: %s", report.Process.Unavailable)
	} else {
		t.Logf("OpenSBX process observations: %d samples CPU=%+v RSS=%+v", report.Process.Samples, report.Process.CPUPercent, report.Process.RSSKB)
	}
}
