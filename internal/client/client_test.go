package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func localClient(t *testing.T, server *httptest.Server, token string) *Client {
	t.Helper()
	c, err := New(strings.TrimPrefix(server.URL, "http://"), token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestNewAcceptsOnlyNumericLoopbackEndpoints(t *testing.T) {
	for _, addr := range []string{"", "127.0.0.1:18089", "[::1]:18089"} {
		c, err := New(addr, "")
		if err != nil {
			t.Errorf("New(%q): %v", addr, err)
			continue
		}
		c.Close()
	}
	for _, addr := range []string{"example.com:80", "0.0.0.0:80", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1", "localhost:80"} {
		if c, err := New(addr, ""); err == nil {
			c.Close()
			t.Errorf("New(%q) accepted a non-loopback or invalid endpoint", addr)
		}
	}
}

func TestRequestSendsBearerJSONAndEscapedResourcePath(t *testing.T) {
	type response struct {
		OK bool `json:"ok"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.RawPath == "" || !strings.Contains(r.RequestURI, "a%2Fb%20c") {
			t.Errorf("request method/escaped path = %s %s (raw=%q)", r.Method, r.URL.Path, r.RequestURI)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["content"] != "" {
			t.Errorf("decoded request body=%v err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	c := localClient(t, server, "secret")
	var got response
	if err := c.Request(context.Background(), http.MethodPut, Path("sandboxes", "a/b c", "files"), map[string]string{"content": ""}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatalf("response = %+v", got)
	}
}

func TestRequestRejectsRedirectWithoutForwardingBearerToken(t *testing.T) {
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls++
		if r.Header.Get("Authorization") != "" {
			t.Errorf("redirect target received Authorization header")
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := localClient(t, source, "secret")
	if err := c.Request(context.Background(), http.MethodGet, "/v1/sandboxes", nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect result = %v, want HTTP 307 error", err)
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls)
	}
}

func TestClientIgnoresConfiguredHTTPProxyForLoopbackEndpoint(t *testing.T) {
	proxyCalls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls++ }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c := localClient(t, server, "")
	if err := c.Request(context.Background(), http.MethodGet, "/v1/health", nil, &map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if proxyCalls != 0 {
		t.Fatalf("configured proxy received %d loopback requests", proxyCalls)
	}
}

func TestMutationFailureIsNotRetried(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"failed"}`)
	}))
	defer server.Close()
	c := localClient(t, server, "")
	err := c.Request(context.Background(), http.MethodPost, "/v1/sandboxes", map[string]string{"image": "node"}, nil)
	if err == nil || calls != 1 {
		t.Fatalf("mutation error=%v request count=%d, want one failed attempt", err, calls)
	}
}

func TestOrdinaryMutationCanReceiveSuccessfulHeadersAfterTenSeconds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10*time.Second + 250*time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"sbx-1"}`)
	}))
	defer server.Close()
	c := localClient(t, server, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var result map[string]any
	if err := c.Request(ctx, http.MethodPost, "/v1/sandboxes", map[string]string{"image": "node:22"}, &result); err != nil {
		t.Fatalf("successful mutation response after 10 seconds was rejected: %v", err)
	}
	if result["id"] != "sbx-1" {
		t.Fatalf("mutation result = %#v", result)
	}
}

func TestMutationTransportFailureAfterRequestReceiptReportsUncertainOutcome(t *testing.T) {
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(received)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support connection hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack mutation response: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	c := localClient(t, server, "")
	err := c.Request(context.Background(), http.MethodPost, "/v1/sandboxes", map[string]string{"image": "node:22"}, nil)
	select {
	case <-received:
	default:
		t.Fatal("server did not receive mutation before transport failure")
	}
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "outcome") || !strings.Contains(strings.ToLower(err.Error()), "inspect") {
		t.Fatalf("post-receipt mutation error = %v; want uncertain outcome and inspect-before-retry guidance", err)
	}
	if strings.Contains(err.Error(), "start the server explicitly") {
		t.Fatalf("received mutation was misclassified as an unavailable server: %v", err)
	}
}

func TestRequestRejectsNonJSONAndTrailingValues(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body string }{
		{"wrong media type", "text/plain", `{"ok":true}`},
		{"trailing value", "application/json", `{"ok":true} {"ok":false}`},
		{"malformed", "application/json", `{"ok":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			c := localClient(t, server, "")
			var result map[string]any
			if err := c.Request(context.Background(), http.MethodGet, "/v1/x", nil, &result); err == nil {
				t.Fatalf("accepted response %q (%s)", tc.body, tc.contentType)
			}
		})
	}
}

func TestResolveRequiresExactOrUniquePrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxes":[{"id":"sbx-abcd","name":"alpha"},{"id":"sbx-abce","name":"beta"}]}`)
	}))
	defer server.Close()
	c := localClient(t, server, "")
	for target, want := range map[string]string{"alpha": "sbx-abcd", "sbx-abcd": "sbx-abcd", "sbx-ab": ""} {
		got, err := c.Resolve(context.Background(), target)
		if target == "sbx-ab" {
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Errorf("Resolve(%q) = %q, %v; want ambiguity", target, got, err)
			}
		} else if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", target, got, err, want)
		}
	}
	if _, err := c.Resolve(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing target error = %v", err)
	}
}

func TestWaitRequiresMatchingConfirmedTerminalState(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    string
	}{
		{"valid terminal", `{"command":{"id":"cmd-1","sandbox_id":"sb-1","exit_code":7}}`, ""},
		{"eof without terminal", `{"command":{"id":"cmd-1","sandbox_id":"sb-1"}}`, "without a confirmed exit code"},
		{"wrong command", `{"command":{"id":"other","sandbox_id":"sb-1","exit_code":0}}`, "mismatched command"},
		{"wrong sandbox", `{"command":{"id":"cmd-1","sandbox_id":"other","exit_code":0}}`, "mismatched command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = fmt.Fprintln(w, tc.body)
			}))
			defer server.Close()
			c := localClient(t, server, "")
			got, err := c.Wait(context.Background(), "sb-1", "cmd-1")
			if tc.wantErr == "" {
				if err != nil || got.ExitCode == nil || *got.ExitCode != 7 {
					t.Fatalf("Wait = %+v, %v", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Wait error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLogsDemultiplexesExactTextAndRejectsInvalidRecords(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		wantErr      bool
	}{
		{"valid raw terminal text", `{"type":"stdout","data":"\u001b[31mraw\u001b[0m\u001b]52;c;payload\u0007\n"}` + "\n" + `{"type":"stderr","data":"終"}`, false},
		{"missing data", `{"type":"stdout"}`, true},
		{"unknown stream", `{"type":"other","data":"x"}`, true},
		{"source error record", `{"type":"error","data":"logs were truncated"}`, true},
		{"malformed", `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = io.WriteString(w, tc.record)
			}))
			defer server.Close()
			c := localClient(t, server, "")
			var stdout, stderr strings.Builder
			err := c.Logs(context.Background(), "sb-1", "cmd-1", &stdout, &stderr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Logs error = %v, wantErr=%t", err, tc.wantErr)
			}
			if !tc.wantErr && (stdout.String() != "\x1b[31mraw\x1b[0m\x1b]52;c;payload\x07\n" || stderr.String() != "終") {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestLogsReturnsWhenObservationContextIsCanceled(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	c := localClient(t, server, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Logs(ctx, "sb-1", "cmd-1", io.Discard, io.Discard) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("log request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled log stream returned nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Logs did not return after cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("server did not observe stream cancellation")
	}
}

func TestRequestRedactsTokenFromAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"invalid secret"}`)
	}))
	defer server.Close()
	c := localClient(t, server, "secret")
	err := c.Request(context.Background(), http.MethodGet, "/v1/health", nil, nil)
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("API error exposed token: %v", err)
	}
}

func TestRequestReturnsCancellationInsteadOfTransportNoise(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := localClient(t, server, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	err := c.Request(ctx, http.MethodGet, "/v1/health", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Request error = %v, want context cancellation", err)
	}
}

func TestAPIErrorTextCannotEmitTerminalControlSequences(t *testing.T) {
	const hostile = "failure \x1b[31mred\x1b[0m \x1b]52;c;clipboard-payload\x07"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": hostile})
	}))
	defer server.Close()
	c := localClient(t, server, "")
	err := c.Request(context.Background(), http.MethodGet, "/v1/sandboxes", nil, nil)
	if err == nil {
		t.Fatal("expected API error")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Fatalf("API diagnostic contains terminal control characters: %q", err.Error())
	}
}

func TestWaitRejectsWrongContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"command":{}}`)
	}))
	defer server.Close()
	c := localClient(t, server, "")
	if _, err := c.Wait(context.Background(), "sb-1", "cmd-1"); err == nil || !strings.Contains(err.Error(), "NDJSON") {
		t.Fatalf("Wait error = %v, want stream content type error", err)
	}
}
