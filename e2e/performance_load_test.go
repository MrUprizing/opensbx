//go:build e2e && performance

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"opensbx/models"
)

type httpEndpointResult struct {
	Requests        int                    `json:"requests"`
	HTTPSuccesses   int                    `json:"http_successes"`
	HTTPFailures    int                    `json:"http_failures"`
	TransportErrors int                    `json:"transport_errors"`
	Timeouts        int                    `json:"timeouts"`
	Latency         *performanceStatistics `json:"latency_ms"`
}

type httpLoadResult struct {
	OfferedRate         float64                       `json:"offered_rate_per_second"`
	AchievedRate        float64                       `json:"achieved_rate_per_second"`
	OfferedIterations   int                           `json:"offered_iterations"`
	AchievedIterations  int                           `json:"achieved_iterations"`
	DroppedIterations   int                           `json:"dropped_iterations"`
	SchedulerDropped    int                           `json:"scheduler_dropped_iterations"`
	CapacityDropped     int                           `json:"capacity_dropped_iterations"`
	CanceledArrivals    int                           `json:"canceled_arrivals"`
	LatencyTimingSource string                        `json:"latency_timing_source"`
	SchedulingWindow    string                        `json:"scheduling_window"`
	ElapsedMS           float64                       `json:"elapsed_ms"`
	Endpoints           map[string]httpEndpointResult `json:"endpoints"`
}

type loadEndpoint struct {
	name, url     string
	authenticated bool
	valid         func([]byte) bool
}

// The original .localhost Host is retained, but dialing is restricted to the
// numeric loopback destinations chosen by the isolated harness. No DNS/proxy is used.
func newPerformanceHTTPClient(endpoints []loadEndpoint, maxConcurrent int) (*http.Client, error) {
	allowed := map[string]string{}
	for _, endpoint := range endpoints {
		u, err := url.Parse(endpoint.url)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Port() == "" || u.Fragment != "" {
			return nil, fmt.Errorf("load endpoint must be a local HTTP URL with a port")
		}
		host := u.Hostname()
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("load endpoint port must be between 1 and 65535")
		}
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			allowed[u.Host] = net.JoinHostPort(host, u.Port())
		} else if strings.HasSuffix(host, ".localhost") && len(host) > len(".localhost") {
			allowed[u.Host] = net.JoinHostPort("127.0.0.1", u.Port())
		} else {
			return nil, fmt.Errorf("load endpoint must use numeric loopback or a sandbox .localhost host")
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil, MaxIdleConns: maxConcurrent * len(endpoints), MaxIdleConnsPerHost: maxConcurrent,
		MaxConnsPerHost: maxConcurrent, IdleConnTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			target, ok := allowed[address]
			if !ok {
				return nil, fmt.Errorf("load generator refused an unconfigured destination")
			}
			return dialer.DialContext(ctx, network, target)
		},
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type loadObservation struct {
	endpoint string
	elapsed  time.Duration
	outcome  string
	timeout  bool
}

func observeLoadRequest(ctx context.Context, client *http.Client, endpoint loadEndpoint, key string, timeout time.Duration) loadObservation {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	observation := loadObservation{endpoint: endpoint.name, outcome: "transport"}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.url, nil)
	if err != nil {
		return observation
	}
	if endpoint.authenticated {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	started := time.Now()
	var data []byte
	response, err := client.Do(request)
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		closeErr := response.Body.Close()
		if err == nil {
			err = closeErr
		}
	}
	observation.elapsed = time.Since(started)
	if err == nil {
		observation.outcome = "http_failure"
		if len(data) <= 4<<20 && response.StatusCode == http.StatusOK && endpoint.valid(data) {
			observation.outcome = "success"
		}
	}
	if err != nil {
		var networkError net.Error
		observation.timeout = errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout()
	}
	return observation
}

// Each arrival has an absolute deadline independent of earlier responses.
// There is no queue: capacity exhaustion or a missed scheduling interval drops
// that arrival instead of slowing down the offered rate or emitting a catch-up burst.
func runHTTPPerformanceLoad(ctx context.Context, endpoints []loadEndpoint, key string, settings performanceSettings) (httpLoadResult, error) {
	result := httpLoadResult{OfferedRate: float64(settings.rate), SchedulingWindow: settings.duration.String(), LatencyTimingSource: "Go monotonic elapsed time around HTTP request and complete bounded body read; includes connection setup and all outcomes", Endpoints: map[string]httpEndpointResult{}}
	if settings.rate < 1 || settings.rate > 20 || settings.duration <= 0 || settings.duration > 2*time.Minute || settings.maxVUs < 1 || settings.maxVUs > 50 || settings.requestTimeout <= 0 || settings.requestTimeout > 30*time.Second || len(endpoints) == 0 {
		return result, fmt.Errorf("invalid bounded load settings")
	}
	for _, endpoint := range endpoints {
		if endpoint.name == "" || endpoint.valid == nil {
			return result, fmt.Errorf("invalid endpoint fixture")
		}
		if _, duplicate := result.Endpoints[endpoint.name]; duplicate {
			return result, fmt.Errorf("duplicate endpoint name")
		}
		result.Endpoints[endpoint.name] = httpEndpointResult{}
	}
	client, err := newPerformanceHTTPClient(endpoints, settings.maxVUs)
	if err != nil {
		return result, err
	}
	defer client.CloseIdleConnections()
	// Integer arithmetic avoids phantom boundary arrivals from float rounding.
	result.OfferedIterations = int((settings.duration*time.Duration(settings.rate) + time.Second - 1) / time.Second)
	observations := make(chan loadObservation, result.OfferedIterations)
	slots := make(chan struct{}, settings.maxVUs)
	var workers sync.WaitGroup
	started := time.Now()
	interval := time.Second / time.Duration(settings.rate)
	for i := 0; i < result.OfferedIterations; i++ {
		due := started.Add(time.Duration(i) * time.Second / time.Duration(settings.rate))
		timer := time.NewTimer(time.Until(due))
		select {
		case <-ctx.Done():
			timer.Stop()
			result.CanceledArrivals += result.OfferedIterations - i
			i = result.OfferedIterations
			continue
		case <-timer.C:
		}
		if time.Since(due) >= interval {
			result.SchedulerDropped++
			continue
		}
		select {
		case slots <- struct{}{}:
			workers.Add(1)
			go func(endpoint loadEndpoint) {
				defer workers.Done()
				defer func() { <-slots }()
				observations <- observeLoadRequest(ctx, client, endpoint, key, settings.requestTimeout)
			}(endpoints[i%len(endpoints)])
		default:
			result.CapacityDropped++
		}
	}
	// Preserve the entire configured scheduling window, then join in-flight
	// requests under their own deadline. No successful tail request is interrupted.
	if remaining := time.Until(started.Add(settings.duration)); remaining > 0 {
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
	workers.Wait()
	close(observations)
	values := map[string][]float64{}
	for observation := range observations {
		e := result.Endpoints[observation.endpoint]
		e.Requests++
		switch observation.outcome {
		case "success":
			e.HTTPSuccesses++
		case "http_failure":
			e.HTTPFailures++
		default:
			e.TransportErrors++
		}
		if observation.timeout {
			e.Timeouts++
		}
		values[observation.endpoint] = append(values[observation.endpoint], float64(observation.elapsed)/float64(time.Millisecond))
		result.Endpoints[observation.endpoint] = e
		result.AchievedIterations++
	}
	for name, samples := range values {
		e := result.Endpoints[name]
		e.Latency, err = summarizePerformance(samples)
		if err != nil {
			return result, err
		}
		result.Endpoints[name] = e
	}
	result.DroppedIterations = result.CapacityDropped + result.SchedulerDropped + result.CanceledArrivals
	result.AchievedRate = float64(result.AchievedIterations) / settings.duration.Seconds()
	result.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
	if err := validateHTTPResult(result); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

func validateHTTPResult(result httpLoadResult) error {
	if result.OfferedIterations < 1 || result.AchievedIterations < 0 || result.DroppedIterations < 0 || result.CapacityDropped < 0 || result.SchedulerDropped < 0 || result.CanceledArrivals < 0 {
		return fmt.Errorf("invalid load arrival counts")
	}
	if result.OfferedIterations != result.AchievedIterations+result.DroppedIterations || result.DroppedIterations != result.CapacityDropped+result.SchedulerDropped+result.CanceledArrivals {
		return fmt.Errorf("load arrivals do not reconcile")
	}
	count := 0
	for name, e := range result.Endpoints {
		if e.Requests < 0 || e.HTTPSuccesses < 0 || e.HTTPFailures < 0 || e.TransportErrors < 0 || e.Timeouts < 0 || e.Timeouts > e.TransportErrors || e.Requests != e.HTTPSuccesses+e.HTTPFailures+e.TransportErrors {
			return fmt.Errorf("invalid outcomes for %s", name)
		}
		if e.Requests > 0 && (e.Latency == nil || e.Latency.Count != e.Requests) || e.Requests == 0 && e.Latency != nil {
			return fmt.Errorf("incomplete latency samples for %s", name)
		}
		if e.Latency != nil {
			s := e.Latency
			for _, value := range []float64{s.MinMS, s.MeanMS, s.P50MS, s.P95MS, s.P99MS, s.MaxMS} {
				if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value < s.MinMS || value > s.MaxMS {
					return fmt.Errorf("invalid latency statistics for %s", name)
				}
			}
			if s.P50MS > s.P95MS || s.P95MS > s.P99MS {
				return fmt.Errorf("unordered latency statistics for %s", name)
			}
		}
		count += e.Requests
	}
	if count != result.AchievedIterations {
		return fmt.Errorf("endpoint samples do not reconcile")
	}
	return nil
}

func measureHTTPPerformance(t *testing.T, h *harness, sandboxID, sandboxURL string, expectedCommands int, settings performanceSettings, report *performanceReport) {
	t.Helper()
	endpoints := []loadEndpoint{
		{"health", h.endpoint + "/v1/health", false, func(data []byte) bool {
			var x struct{ Status string }
			return json.Unmarshal(data, &x) == nil && x.Status == "healthy"
		}},
		{"sandbox_list", h.endpoint + "/v1/sandboxes", true, func(data []byte) bool {
			var x struct{ Sandboxes []models.SandboxSummary }
			return json.Unmarshal(data, &x) == nil && len(x.Sandboxes) == 1 && x.Sandboxes[0].ID == sandboxID
		}},
		{"sandbox_inspect", h.endpoint + "/v1/sandboxes/" + sandboxID, true, func(data []byte) bool {
			var x models.SandboxDetail
			return json.Unmarshal(data, &x) == nil && x.ID == sandboxID
		}},
		{"command_history", h.endpoint + "/v1/sandboxes/" + sandboxID + "/cmd", true, func(data []byte) bool {
			var x models.CommandListResponse
			return json.Unmarshal(data, &x) == nil && len(x.Commands) == expectedCommands
		}},
		{"proxy_app", strings.TrimSuffix(sandboxURL, "/") + "/", false, func(data []byte) bool { return string(data) == performanceProxyMarker }},
	}
	ctx, cancel := context.WithTimeout(context.Background(), settings.duration+settings.requestTimeout+2*time.Second)
	defer cancel()
	started := time.Now()
	result, err := runHTTPPerformanceLoad(ctx, endpoints, h.key, settings)
	appendHTTPMetrics(report, result)
	if err != nil {
		seriesByName(report, "http/load").fail("collector", "HTTP load collection failed")
		t.Errorf("HTTP performance collection: %v", err)
		return
	}
	seriesByName(report, "http/load").add(time.Since(started))
}
