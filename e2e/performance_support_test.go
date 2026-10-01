//go:build e2e && performance

package e2e_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type performanceSettings struct {
	runtime                                  string
	iterations, commandHistory, rate, maxVUs int
	duration, requestTimeout                 time.Duration
	artifactDirectory                        string
}

func parsePerformanceSettings(values map[string]string) (performanceSettings, error) {
	s := performanceSettings{runtime: "docker", iterations: 3, commandHistory: 10, rate: 5, maxVUs: 10, duration: 10 * time.Second, requestTimeout: 5 * time.Second}
	if value := values["OPENSBX_E2E_RUNTIME"]; value != "" {
		s.runtime = value
	}
	if s.runtime != "docker" && s.runtime != "container" {
		return s, fmt.Errorf("OPENSBX_E2E_RUNTIME must be docker or container")
	}
	for _, item := range []struct {
		key      string
		target   *int
		min, max int
	}{
		{"OPENSBX_PERF_ITERATIONS", &s.iterations, 1, 20},
		{"OPENSBX_PERF_COMMAND_HISTORY", &s.commandHistory, 1, 100},
		{"OPENSBX_PERF_RATE", &s.rate, 1, 20},
		{"OPENSBX_PERF_MAX_VUS", &s.maxVUs, 1, 50},
	} {
		if raw := values[item.key]; raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < item.min || n > item.max {
				return s, fmt.Errorf("%s must be an integer from %d through %d", item.key, item.min, item.max)
			}
			*item.target = n
		}
	}
	for _, item := range []struct {
		key    string
		target *time.Duration
		max    time.Duration
	}{
		{"OPENSBX_PERF_DURATION", &s.duration, 2 * time.Minute},
		{"OPENSBX_PERF_REQUEST_TIMEOUT", &s.requestTimeout, 30 * time.Second},
	} {
		if raw := values[item.key]; raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d < time.Second || d > item.max {
				return s, fmt.Errorf("%s must be between 1s and %s", item.key, item.max)
			}
			*item.target = d
		}
	}
	s.artifactDirectory = strings.TrimSpace(values["OPENSBX_PERF_ARTIFACTS"])
	if s.artifactDirectory != "" && !filepath.IsAbs(s.artifactDirectory) {
		return s, fmt.Errorf("OPENSBX_PERF_ARTIFACTS must be an absolute directory")
	}
	return s, nil
}

type performanceStatistics struct {
	Count  int     `json:"count"`
	MinMS  float64 `json:"min_ms"`
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
}

func summarizePerformance(values []float64) (*performanceStatistics, error) {
	if len(values) == 0 {
		return nil, nil
	}
	ordered := append([]float64(nil), values...)
	var sum float64
	for _, value := range ordered {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("sample must be a finite non-negative number")
		}
		sum += value
	}
	sort.Float64s(ordered)
	return &performanceStatistics{Count: len(ordered), MinMS: ordered[0], MeanMS: sum / float64(len(ordered)), P50MS: percentile(ordered, .5), P95MS: percentile(ordered, .95), P99MS: percentile(ordered, .99), MaxMS: ordered[len(ordered)-1]}, nil
}

func percentile(sorted []float64, fraction float64) float64 {
	position := fraction * float64(len(sorted)-1)
	lower, upper := int(math.Floor(position)), int(math.Ceil(position))
	return sorted[lower] + (position-float64(lower))*(sorted[upper]-sorted[lower])
}

type performanceSeries struct {
	Name         string                 `json:"name"`
	Unit         string                 `json:"unit"`
	Scope        string                 `json:"scope"`
	SampleCount  int                    `json:"sample_count"`
	MissingCount int                    `json:"missing_count"`
	Errors       []performanceError     `json:"errors"`
	Statistics   *performanceStatistics `json:"statistics"`
	Warnings     []string               `json:"warnings,omitempty"`
	values       []float64
}
type performanceError struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

func newPerformanceSeries(name, unit, scope string) *performanceSeries {
	return &performanceSeries{Name: name, Unit: unit, Scope: scope, Errors: []performanceError{}}
}
func (s *performanceSeries) add(value time.Duration) {
	s.values = append(s.values, float64(value)/float64(time.Millisecond))
	s.SampleCount++
}
func (s *performanceSeries) noteError(kind, detail string) {
	s.Errors = append(s.Errors, performanceError{Kind: kind, Detail: detail})
}
func (s *performanceSeries) fail(kind, detail string) { s.MissingCount++; s.noteError(kind, detail) }
func (s *performanceSeries) finalize() error {
	if len(s.values) > 0 || s.Statistics == nil {
		var err error
		s.Statistics, err = summarizePerformance(s.values)
		if err != nil {
			return fmt.Errorf("%s statistics: %w", s.Name, err)
		}
	}
	if s.SampleCount > 0 && s.SampleCount < 20 {
		addPerformanceWarning(s, "small sample count; percentiles are descriptive only")
	}
	if s.SampleCount == 0 {
		addPerformanceWarning(s, "no latency samples are available")
	}
	return nil
}
func addPerformanceWarning(s *performanceSeries, warning string) {
	for _, existing := range s.Warnings {
		if existing == warning {
			return
		}
	}
	s.Warnings = append(s.Warnings, warning)
}

type performanceReport struct {
	SchemaVersion int                         `json:"schema_version"`
	StartedAt     time.Time                   `json:"started_at"`
	FinishedAt    time.Time                   `json:"finished_at"`
	Runtime       performanceRuntimeMetadata  `json:"runtime"`
	Environment   performanceEnvironment      `json:"environment"`
	Configuration performanceRunConfiguration `json:"configuration"`
	Dataset       performanceDataset          `json:"dataset"`
	Measurements  []*performanceSeries        `json:"measurements"`
	HTTP          *httpLoadResult             `json:"http,omitempty"`
	Warnings      []string                    `json:"warnings,omitempty"`
	Process       processObservation          `json:"open_sbx_process_observation"`
	TestFailed    bool                        `json:"test_failed"`
}
type performanceRuntimeMetadata struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Image       string `json:"image_reference"`
	SourceImage string `json:"source_image_reference"`
	Digest      string `json:"selected_platform_manifest_digest"`
}
type performanceEnvironment struct {
	OS               string `json:"os"`
	Architecture     string `json:"architecture"`
	GoVersion        string `json:"go_version"`
	Revision         string `json:"revision"`
	Dirty            *bool  `json:"dirty"`
	LoadGenerator    string `json:"load_generator"`
	ConnectionReuse  bool   `json:"connection_reuse"`
	ConnectionPolicy string `json:"connection_policy"`
}
type performanceRunConfiguration struct {
	RatePerSecond         int    `json:"arrival_rate_per_second"`
	Duration              string `json:"duration"`
	MaxConcurrentRequests int    `json:"max_concurrent_requests"`
	Iterations            int    `json:"lifecycle_iterations"`
	RequestTimeout        string `json:"request_timeout"`
	TimingUnit            string `json:"timing_unit"`
	Scope                 string `json:"scope"`
}
type performanceDataset struct {
	Sandboxes                  int   `json:"sandboxes_during_load"`
	CommandHistory             int   `json:"commands_during_load"`
	ConcurrentRuntimeSandboxes int   `json:"maximum_runtime_sandboxes"`
	SandboxMemoryMiB           int64 `json:"sandbox_memory_mib"`
	SandboxCPUs                int   `json:"sandbox_cpus"`
}
type processObservation struct {
	Scope       string              `json:"scope"`
	Method      string              `json:"method"`
	Samples     int                 `json:"samples"`
	CPUPercent  *resourceStatistics `json:"cpu_percent"`
	RSSKB       *resourceStatistics `json:"rss_kb"`
	Unavailable string              `json:"unavailable,omitempty"`
}
type resourceStatistics struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

func summarizeResources(values []float64) *resourceStatistics {
	s, err := summarizePerformance(values)
	if err != nil || s == nil {
		return nil
	}
	return &resourceStatistics{Count: s.Count, Min: s.MinMS, Mean: s.MeanMS, P50: s.P50MS, P95: s.P95MS, P99: s.P99MS, Max: s.MaxMS}
}
func writePerformanceReport(path string, report *performanceReport) error {
	for _, s := range report.Measurements {
		if err := s.finalize(); err != nil {
			return err
		}
	}
	if report.FinishedAt.IsZero() {
		report.FinishedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal performance report: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create performance report without overwrite: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func appendHTTPMetrics(report *performanceReport, result httpLoadResult) {
	report.HTTP = &result
	names := make([]string, 0, len(result.Endpoints))
	for name := range result.Endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := result.Endpoints[name]
		s := newPerformanceSeries("http/"+name, "ms", result.LatencyTimingSource)
		s.SampleCount, s.Statistics = e.Requests, e.Latency
		if e.HTTPFailures > 0 {
			s.noteError("http_or_shape_failure", fmt.Sprintf("%d responses failed expected status or payload validation", e.HTTPFailures))
		}
		if e.TransportErrors > 0 {
			s.noteError("transport_error", fmt.Sprintf("%d request transport errors", e.TransportErrors))
		}
		if e.Timeouts > 0 {
			s.noteError("timeout", fmt.Sprintf("%d request timeouts (subset of transport errors)", e.Timeouts))
		}
		report.Measurements = append(report.Measurements, s)
	}
}
