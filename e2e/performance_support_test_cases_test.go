//go:build e2e && performance

package e2e_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPerformanceSettingsRejectInvalidValuesBeforeSetup(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
	}{
		{"unknown runtime", map[string]string{"OPENSBX_E2E_RUNTIME": "remote"}},
		{"zero rate", map[string]string{"OPENSBX_PERF_RATE": "0"}},
		{"unbounded rate", map[string]string{"OPENSBX_PERF_RATE": "21"}},
		{"invalid iterations", map[string]string{"OPENSBX_PERF_ITERATIONS": "many"}},
		{"too many commands", map[string]string{"OPENSBX_PERF_COMMAND_HISTORY": "101"}},
		{"invalid duration", map[string]string{"OPENSBX_PERF_DURATION": "0s"}},
		{"duration too long", map[string]string{"OPENSBX_PERF_DURATION": "3m"}},
		{"invalid concurrency", map[string]string{"OPENSBX_PERF_MAX_VUS": "0"}},
		{"relative artifact path", map[string]string{"OPENSBX_PERF_ARTIFACTS": "reports"}},
		{"negative timeout", map[string]string{"OPENSBX_PERF_REQUEST_TIMEOUT": "-1s"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parsePerformanceSettings(test.values); err == nil {
				t.Fatal("parsePerformanceSettings() unexpectedly accepted invalid config")
			}
		})
	}
}

func TestPerformanceSettingsAcceptBoundedOverrides(t *testing.T) {
	got, err := parsePerformanceSettings(map[string]string{
		"OPENSBX_E2E_RUNTIME": "container", "OPENSBX_PERF_ITERATIONS": "2",
		"OPENSBX_PERF_COMMAND_HISTORY": "12", "OPENSBX_PERF_RATE": "7",
		"OPENSBX_PERF_DURATION": "1.5s",
		"OPENSBX_PERF_MAX_VUS":  "8", "OPENSBX_PERF_REQUEST_TIMEOUT": "1500ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.runtime != "container" || got.iterations != 2 || got.commandHistory != 12 || got.rate != 7 || got.duration != 1500*time.Millisecond || got.maxVUs != 8 || got.requestTimeout != 1500*time.Millisecond {
		t.Fatalf("settings = %+v", got)
	}
}

func TestPerformanceStatisticsAreStableAndEmptyIsUnavailable(t *testing.T) {
	got, err := summarizePerformance([]float64{5, 1, 4, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 5 || got.MinMS != 1 || got.MeanMS != 3 || got.P50MS != 3 || got.P95MS != 4.8 || got.P99MS != 4.96 || got.MaxMS != 5 {
		t.Fatalf("statistics = %+v", got)
	}
	if missing, err := summarizePerformance(nil); err != nil || missing != nil {
		t.Fatalf("empty statistics = %+v, err=%v; want nil unavailable metric", missing, err)
	}
	if _, err := summarizePerformance([]float64{1, -1}); err == nil {
		t.Fatal("negative samples must be rejected")
	}
}

func TestPerformanceProcessResourceStatisticsUseResourceUnits(t *testing.T) {
	stats := summarizeResources([]float64{1.5, 2.5, 3.5})
	if stats == nil || stats.Count != 3 || stats.Min != 1.5 || stats.Mean != 2.5 || stats.P50 != 2.5 || stats.Max != 3.5 {
		t.Fatalf("resource statistics = %+v", stats)
	}
	if missing := summarizeResources(nil); missing != nil {
		t.Fatalf("empty resource samples should remain unavailable: %+v", missing)
	}
}

func TestPerformanceSeriesCountsMeasuredErrorsWithoutLatencyBudget(t *testing.T) {
	series := newPerformanceSeries("api/list", "ms", "single HTTP request")
	series.add(4 * time.Millisecond)
	series.fail("http", "unexpected status")
	if err := series.finalize(); err != nil {
		t.Fatal(err)
	}
	if series.SampleCount != 1 || series.MissingCount != 1 || series.Statistics == nil || series.Statistics.Count != 1 || len(series.Errors) != 1 {
		t.Fatalf("series = %+v", series)
	}
	if len(series.Warnings) == 0 || !strings.Contains(series.Warnings[0], "small sample") {
		t.Fatalf("small sample warning missing: %+v", series.Warnings)
	}
}

func TestLifecycleMeasurementsStopAfterUncertainCreateAndKeepExactOwners(t *testing.T) {
	for _, mode := range []string{"lost create response", "malformed create response"} {
		t.Run(mode, func(t *testing.T) {
			calls := []string{}
			owned := map[string]bool{}
			ops := lifecycleSampleOps{
				create: func(sample int) (string, time.Duration, error) {
					calls = append(calls, "create")
					owned["native-owned-exact"] = true
					kind := "http"
					if mode == "malformed create response" {
						kind = "response"
					}
					return "", 0, &lifecycleFailure{kind: kind, err: errors.New(mode)}
				},
				stop:   func(string) (time.Duration, error) { calls = append(calls, "stop"); return 0, nil },
				start:  func(string) (time.Duration, error) { calls = append(calls, "start"); return 0, nil },
				delete: func(string) (time.Duration, error) { calls = append(calls, "delete"); return 0, nil },
			}
			report := lifecycleTestReport()
			if runLifecycleSamples(t, 3, ops, report) {
				t.Fatal("uncertain create must mark lifecycle fixture incomplete")
			}
			if len(calls) != 1 || calls[0] != "create" {
				t.Fatalf("uncertain create must stop later mutations: %v", calls)
			}
			if !owned["native-owned-exact"] {
				t.Fatal("uncertain native ID was not retained in exact cleanup ownership")
			}
			if seriesByName(report, "lifecycle/create").MissingCount != 3 || seriesByName(report, "lifecycle/stop").MissingCount != 3 || seriesByName(report, "lifecycle/start").MissingCount != 3 || seriesByName(report, "lifecycle/delete").MissingCount != 3 {
				t.Fatalf("unattempted measurements were not reported: %+v", report.Measurements)
			}
		})
	}
}

func TestLifecycleMeasurementsStopAfterFailedDeleteAndRetainOwner(t *testing.T) {
	calls := []string{}
	owned := map[string]bool{}
	ops := lifecycleSampleOps{
		create: func(int) (string, time.Duration, error) {
			calls = append(calls, "create")
			owned["native-owned-exact"] = true
			return "public-owned", time.Millisecond, nil
		},
		verifyRunning: func(string) error { return nil },
		stop:          func(string) (time.Duration, error) { calls = append(calls, "stop"); return time.Millisecond, nil },
		verifyStopped: func(string) error { return nil },
		start:         func(string) (time.Duration, error) { calls = append(calls, "start"); return time.Millisecond, nil },
		verifyStarted: func(string) error { return nil },
		delete: func(string) (time.Duration, error) {
			calls = append(calls, "delete")
			return 0, errors.New("delete response lost")
		},
		verifyDeleted: func(string) error { t.Fatal("must not verify a failed delete as completed"); return nil },
	}
	report := lifecycleTestReport()
	if runLifecycleSamples(t, 3, ops, report) {
		t.Fatal("failed delete must mark lifecycle fixture incomplete")
	}
	if strings.Join(calls, ",") != "create,stop,start,delete" {
		t.Fatalf("no later lifecycle create is allowed after uncertain deletion: %v", calls)
	}
	if !owned["native-owned-exact"] {
		t.Fatal("failed deletion must remain eligible for exact harness cleanup")
	}
	if seriesByName(report, "lifecycle/delete").MissingCount != 3 {
		t.Fatalf("delete failure and both unrun samples must be reported: %+v", seriesByName(report, "lifecycle/delete"))
	}
}

func TestLifecycleVerificationErrorsPreserveCompletedSamplesAndStopMutations(t *testing.T) {
	for _, failedVerifier := range []string{"verifyRunning", "verifyStopped", "verifyStarted", "verifyDeleted"} {
		for _, planned := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/iterations_%d", failedVerifier, planned), func(t *testing.T) {
				calls := []string{}
				owned := map[string]bool{}
				verify := func(name string) error {
					calls = append(calls, name)
					if name == failedVerifier {
						return errors.New("synthetic verification mismatch")
					}
					return nil
				}
				ops := lifecycleSampleOps{
					create: func(sample int) (string, time.Duration, error) {
						calls = append(calls, "create")
						id := fmt.Sprintf("public-%d", sample)
						owned["native-"+id] = true
						return id, time.Millisecond, nil
					},
					verifyRunning: func(string) error { return verify("verifyRunning") },
					stop:          func(string) (time.Duration, error) { calls = append(calls, "stop"); return time.Millisecond, nil },
					verifyStopped: func(string) error { return verify("verifyStopped") },
					start:         func(string) (time.Duration, error) { calls = append(calls, "start"); return time.Millisecond, nil },
					verifyStarted: func(string) error { return verify("verifyStarted") },
					delete: func(id string) (time.Duration, error) {
						calls = append(calls, "delete")
						if failedVerifier != "verifyDeleted" {
							delete(owned, "native-"+id)
						}
						return time.Millisecond, nil
					},
					verifyDeleted: func(string) error { return verify("verifyDeleted") },
				}
				report := lifecycleTestReport()
				if runLifecycleSamples(t, planned, ops, report) {
					t.Fatal("verification mismatch must stop lifecycle samples")
				}
				for _, name := range []string{"create", "stop", "start", "delete"} {
					series := seriesByName(report, "lifecycle/"+name)
					if series.SampleCount+series.MissingCount != planned {
						t.Errorf("%s accounting samples=%d missing=%d planned=%d", name, series.SampleCount, series.MissingCount, planned)
					}
				}
				failedStage := map[string]string{
					"verifyRunning": "create", "verifyStopped": "stop",
					"verifyStarted": "start", "verifyDeleted": "delete",
				}[failedVerifier]
				failedSeries := seriesByName(report, "lifecycle/"+failedStage)
				if failedSeries.SampleCount != 1 || failedSeries.MissingCount != planned-1 || len(failedSeries.Errors) < 1 || failedSeries.Errors[0].Kind != "runtime_state" && failedSeries.Errors[0].Kind != "cleanup_verification" {
					t.Fatalf("verified HTTP sample must remain sampled with diagnostic only: %+v", failedSeries)
				}
				if failedSeries.Errors[0].Kind != "runtime_state" && failedSeries.Errors[0].Kind != "cleanup_verification" {
					t.Fatalf("post-verification diagnostic kind=%q", failedSeries.Errors[0].Kind)
				}
				encoded, err := json.Marshal(report)
				if err != nil {
					t.Fatal(err)
				}
				var decoded struct {
					Measurements []struct {
						Name         string `json:"name"`
						SampleCount  int    `json:"sample_count"`
						MissingCount int    `json:"missing_count"`
						Errors       []struct {
							Kind string `json:"kind"`
						} `json:"errors"`
					} `json:"measurements"`
				}
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				for _, measurement := range decoded.Measurements {
					if measurement.SampleCount+measurement.MissingCount != planned {
						t.Fatalf("serialized %s accounting sample=%d missing=%d planned=%d", measurement.Name, measurement.SampleCount, measurement.MissingCount, planned)
					}
				}
				if calls[len(calls)-1] != failedVerifier {
					t.Fatalf("a mutation ran after failed verifier %s: %v", failedVerifier, calls)
				}
				if len(owned) != 1 {
					t.Fatalf("the exact test-owned resource must remain eligible for harness cleanup: %v", owned)
				}
				for id := range owned {
					if !strings.HasPrefix(id, "native-public-") {
						t.Fatalf("unexpected cleanup owner %q", id)
					}
				}
			})
		}
	}
}

func lifecycleTestReport() *performanceReport {
	report := &performanceReport{Measurements: []*performanceSeries{}}
	for _, name := range []string{"create", "stop", "start", "delete"} {
		report.Measurements = append(report.Measurements, newPerformanceSeries("lifecycle/"+name, "ms", "synthetic lifecycle"))
	}
	return report
}

func TestReadFirstLogContentHandlesEmptyFramesAndSplitMarker(t *testing.T) {
	started := time.Now()
	clock := []time.Time{started.Add(10 * time.Millisecond), started.Add(200 * time.Millisecond)}
	index := 0
	readAt := func() time.Time {
		value := clock[index]
		index++
		return value
	}
	stream := strings.Join([]string{
		`{"type":"stdout","data":""}`,
		`{"type":"heartbeat","data":"still waiting"}`,
		`{"type":"stdout","data":"perf-"}`,
		`{"type":"stdout","data":"first-data"}`,
	}, "\n") + "\n"
	firstAt, err := readFirstPerformanceLogData(bufio.NewReader(strings.NewReader(stream)), readAt)
	if err != nil {
		t.Fatal(err)
	}
	if !firstAt.Equal(clock[0]) {
		t.Fatalf("first data timestamp = %v, want the first nonempty record timestamp %v, not marker completion %v", firstAt, clock[0], clock[1])
	}
}

func TestReadFirstLogContentRejectsEOFAndStreamErrors(t *testing.T) {
	for name, input := range map[string]string{
		"incomplete marker at EOF": `{"type":"stdout","data":"perf-"}` + "\n",
		"explicit error record":    `{"type":"error","data":"source failed"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readFirstPerformanceLogData(bufio.NewReader(strings.NewReader(input)), time.Now); err == nil {
				t.Fatal("incomplete or error stream was accepted")
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type contextTrackingBody struct {
	ctx    context.Context
	first  bool
	closed atomic.Bool
}

func (body *contextTrackingBody) Read(p []byte) (int, error) {
	if !body.first {
		body.first = true
		return copy(p, `{"type":"stdout","data":""}`+"\n"), nil
	}
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}
func (body *contextTrackingBody) Close() error { body.closed.Store(true); return nil }

func TestFirstLogReadCancellationClosesResponseBody(t *testing.T) {
	var tracked *contextTrackingBody
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		tracked = &contextTrackingBody{ctx: request.Context()}
		return &http.Response{StatusCode: http.StatusOK, Body: tracked, Header: make(http.Header)}, nil
	})}
	h := &harness{endpoint: "http://loopback", key: "test", http: client}
	if _, err := firstPerformanceLogData(h, "sbx-fixture", "cmd-fixture", 25*time.Millisecond); err == nil {
		t.Fatal("stream without log data should time out")
	}
	if tracked == nil || !tracked.closed.Load() {
		t.Fatal("stream response body was not closed after cancellation")
	}
}

func TestWritePerformanceReportDoesNotSerializeSecrets(t *testing.T) {
	path := t.TempDir() + "/report.json"
	report := &performanceReport{
		SchemaVersion: 2,
		Measurements:  []*performanceSeries{newPerformanceSeries("test", "ms", "bounded")},
		Environment:   performanceEnvironment{LoadGenerator: "Go net/http", ConnectionReuse: true},
		Process:       processObservation{CPUPercent: summarizeResources([]float64{1, 2}), RSSKB: summarizeResources([]float64{1024, 2048})},
	}
	if err := writePerformanceReport(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Authorization") || strings.Contains(string(data), "API_KEY") || strings.Contains(string(data), "benchmark-only-key") {
		t.Fatalf("report serialized a credential: %s", data)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	process := decoded["open_sbx_process_observation"].(map[string]any)
	cpu := process["cpu_percent"].(map[string]any)
	if _, hasMillisecondLabel := cpu["min_ms"]; hasMillisecondLabel || cpu["min"] != float64(1) {
		t.Fatalf("CPU metric should use unit-neutral statistic keys: %v", cpu)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("report permissions expose group/other access: %v", info.Mode().Perm())
	}
	if err := writePerformanceReport(path, report); err == nil {
		t.Fatal("report writer must refuse to overwrite an existing artifact")
	}
}
