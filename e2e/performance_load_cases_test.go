//go:build e2e && performance

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPPerformanceLoadCollectsMixedRoutesAndPrivateReport(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/protected" && r.Header.Get("Authorization") != "Bearer private-fixture-key" {
			t.Error("missing management authentication")
		}
		if r.URL.Path == "/public" && r.Header.Get("Authorization") != "" {
			t.Error("management token leaked to public route")
		}
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	endpoints := []loadEndpoint{
		{"management", server.URL + "/protected", true, func(data []byte) bool { return string(data) == "ok" }},
		{"public", server.URL + "/public", false, func(data []byte) bool { return string(data) == "ok" }},
	}
	settings := performanceSettings{rate: 10, duration: 350 * time.Millisecond, maxVUs: 2, requestTimeout: time.Second}
	result, err := runHTTPPerformanceLoad(context.Background(), endpoints, "private-fixture-key", settings)
	if err != nil {
		t.Fatal(err)
	}
	if result.OfferedIterations != 4 || result.AchievedIterations != 4 || result.DroppedIterations != 0 {
		t.Fatalf("unexpected arrivals: %+v", result)
	}
	for name, e := range result.Endpoints {
		if e.Requests != 2 || e.HTTPSuccesses != 2 || e.Latency == nil || e.Latency.Count != 2 {
			t.Fatalf("%s: %+v", name, e)
		}
	}
	report := &performanceReport{SchemaVersion: 2}
	appendHTTPMetrics(report, result)
	path := t.TempDir() + "/report.json"
	if err := writePerformanceReport(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-fixture-key") || strings.Contains(string(data), "k6") {
		t.Fatalf("obsolete tool metadata or credential in report: %s", data)
	}
	for _, s := range report.Measurements {
		if s.Statistics == nil || s.Statistics.Count != 2 {
			t.Fatalf("report lost samples: %+v", s)
		}
	}
}

func TestHTTPPerformanceObservationClassifiesFailuresAndDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/destination", http.StatusFound)
		case "/destination":
			redirected.Add(1)
			io.WriteString(w, "ok")
		case "/timeout":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/bad":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			io.WriteString(w, "wrong payload")
		}
	}))
	defer server.Close()
	for _, path := range []string{"/redirect", "/bad", "/shape", "/timeout"} {
		t.Run(path, func(t *testing.T) {
			ep := loadEndpoint{"fixture", server.URL + path, true, func(data []byte) bool { return string(data) == "ok" }}
			client, err := newPerformanceHTTPClient([]loadEndpoint{ep}, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			observation := observeLoadRequest(context.Background(), client, ep, "private", 50*time.Millisecond)
			if path == "/timeout" {
				if observation.outcome != "transport" || !observation.timeout {
					t.Fatalf("timeout: %+v", observation)
				}
			} else if observation.outcome != "http_failure" || observation.timeout {
				t.Fatalf("failure: %+v", observation)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("load generator followed redirect")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ep := loadEndpoint{"refusal", "http://" + address, false, func([]byte) bool { return true }}
	client, err := newPerformanceHTTPClient([]loadEndpoint{ep}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	observation := observeLoadRequest(context.Background(), client, ep, "", time.Second)
	if observation.outcome != "transport" || observation.timeout {
		t.Fatalf("connection refusal: %+v", observation)
	}
}

func TestHTTPPerformanceLoadDropsAtCapacityWithoutQueueAndJoinsTail(t *testing.T) {
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		select {
		case <-time.After(350 * time.Millisecond):
			io.WriteString(w, "ok")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	ep := loadEndpoint{"slow", server.URL, false, func(data []byte) bool { return string(data) == "ok" }}
	result, err := runHTTPPerformanceLoad(context.Background(), []loadEndpoint{ep}, "", performanceSettings{rate: 20, duration: 250 * time.Millisecond, maxVUs: 1, requestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.OfferedIterations != 5 || result.AchievedIterations != 1 || result.CapacityDropped == 0 || maximum.Load() != 1 {
		t.Fatalf("capacity was not enforced: %+v maximum=%d", result, maximum.Load())
	}
	if result.Endpoints["slow"].HTTPSuccesses != 1 || result.Endpoints["slow"].Timeouts != 0 || active.Load() != 0 {
		t.Fatalf("pending response was interrupted or not joined: %+v", result)
	}
}

func TestHTTPPerformanceLoadCancellationJoinsRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ep := loadEndpoint{"cancel", server.URL, false, func([]byte) bool { return true }}
	result, err := runHTTPPerformanceLoad(ctx, []loadEndpoint{ep}, "", performanceSettings{rate: 10, duration: time.Second, maxVUs: 1, requestTimeout: time.Second})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing cancellation: %v", err)
	}
	if err := validateHTTPResult(result); err != nil {
		t.Fatal(err)
	}
	if result.Endpoints["cancel"].TransportErrors != result.AchievedIterations || result.CanceledArrivals == 0 {
		t.Fatalf("incomplete canceled attempts: %+v", result)
	}
}

func TestHTTPPerformanceProxyPreservesHostAndRefusesRemoteEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "fixture.localhost:") || r.Header.Get("Authorization") != "" {
			t.Errorf("wrong proxy host or leaked key: %q", r.Host)
		}
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	ep := loadEndpoint{"proxy", "http://fixture.localhost:" + u.Port() + "/", false, func(data []byte) bool { return string(data) == "ok" }}
	client, err := newPerformanceHTTPClient([]loadEndpoint{ep}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if result := observeLoadRequest(context.Background(), client, ep, "private", time.Second); result.outcome != "success" {
		t.Fatalf("proxy mapping failed: %+v", result)
	}
	for _, address := range []string{"http://example.com:80", "http://192.0.2.1:80", "http://user:pass@127.0.0.1:80", "https://127.0.0.1:80", "http://127.0.0.1:0"} {
		if _, err := newPerformanceHTTPClient([]loadEndpoint{{name: "bad", url: address}}, 1); err == nil {
			t.Fatalf("accepted unsafe target %q", address)
		}
	}
}

func TestHTTPPerformanceResultRejectsIncompleteMetrics(t *testing.T) {
	for _, mutate := range []func(*httpLoadResult){
		func(r *httpLoadResult) { r.AchievedIterations++ },
		func(r *httpLoadResult) { e := r.Endpoints["fixture"]; e.Latency = nil; r.Endpoints["fixture"] = e },
		func(r *httpLoadResult) { e := r.Endpoints["fixture"]; e.Timeouts = 1; r.Endpoints["fixture"] = e },
		func(r *httpLoadResult) {
			e := r.Endpoints["fixture"]
			e.Latency.MeanMS = math.NaN()
			r.Endpoints["fixture"] = e
		},
	} {
		stats, _ := summarizePerformance([]float64{1})
		r := httpLoadResult{OfferedIterations: 1, AchievedIterations: 1, Endpoints: map[string]httpEndpointResult{"fixture": {Requests: 1, HTTPSuccesses: 1, Latency: stats}}}
		mutate(&r)
		if err := validateHTTPResult(r); err == nil {
			t.Fatal("incomplete or corrupt report accepted")
		}
	}
}
