// Package client implements the local HTTP management boundary. It never opens runtime or database state.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"opensbx/internal/terminaltext"
	"opensbx/models"
)

const DefaultAddr = "127.0.0.1:18089"
const OperationTimeout = 5 * time.Minute
const StreamHeaderTimeout = 10 * time.Second

type Client struct {
	addr, token string
	http        *http.Client
	streamHTTP  *http.Client
}

func New(addr, token string) (*Client, error) {
	if addr == "" {
		addr = DefaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errors.New("--addr must be a loopback host:port, for example 127.0.0.1:18089 or [::1]:18089")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("--addr requires a numeric loopback IP; remote destinations are not supported")
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return nil, errors.New("--addr port must be between 1 and 65535")
	}
	transport := &http.Transport{
		Proxy: nil,
		// Transport can replay failures on reused connections, including a
		// mutation if no bytes were written. One request per connection prevents
		// that implicit retry without introducing a custom RoundTripper.
		DisableKeepAlives:     true,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: OperationTimeout,
		IdleConnTimeout:       30 * time.Second,
	}
	streamTransport := transport.Clone()
	streamTransport.ResponseHeaderTimeout = StreamHeaderTimeout
	newHTTP := func(t *http.Transport) *http.Client {
		return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Client{addr: net.JoinHostPort(ip.String(), strconv.Itoa(int(p))), token: token, http: newHTTP(transport), streamHTTP: newHTTP(streamTransport)}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections(); c.streamHTTP.CloseIdleConnections() }

type requestError struct {
	cause   error
	message string
}

func (e *requestError) Error() string { return e.message }
func (e *requestError) Unwrap() error { return e.cause }

func mutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func (c *Client) safeText(value string) string {
	if c.token != "" {
		value = strings.ReplaceAll(value, c.token, "[redacted]")
	}
	return terminaltext.Escape(value, false)
}

func (c *Client) failure(cause error, method string, sent bool) error {
	detail := c.safeText(cause.Error())
	var message string
	if sent && mutation(method) {
		message = fmt.Sprintf("OpenSBX at %s: mutation outcome is uncertain: %s; inspect resource or command history before retrying", c.addr, detail)
	} else if sent {
		message = fmt.Sprintf("OpenSBX request at %s failed: %s", c.addr, detail)
	} else {
		message = fmt.Sprintf("cannot reach OpenSBX at %s: %s; check the configured endpoint and that the server is running", c.addr, detail)
	}
	return &requestError{cause: cause, message: message}
}

// Path escapes every resource component independently; query values use url.Values.
func Path(parts ...string) string {
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/v1/" + strings.Join(parts, "/")
}

func (c *Client) open(ctx context.Context, transport *http.Client, method, path string, body any) (*http.Response, error) {
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.addr+path, data)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	var sent atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteHeaders: func() { sent.Store(true) },
	}))
	res, err := transport.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, c.failure(err, method, sent.Load())
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		var problem struct{ Code, Message, Error string }
		_ = json.NewDecoder(io.LimitReader(res.Body, 64*1024)).Decode(&problem)
		message := problem.Message
		if message == "" {
			message = problem.Error
		}
		if message == "" {
			message = http.StatusText(res.StatusCode)
		}
		message = c.safeText(message)
		return nil, fmt.Errorf("OpenSBX at %s: HTTP %d: %s", c.addr, res.StatusCode, message)
	}
	return res, nil
}

// Request bounds ordinary operations, but not long-running observation streams.
func (c *Client) Request(ctx context.Context, method, path string, body, result any) (err error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	res, err := c.open(ctx, c.http, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	defer func() {
		if err != nil && mutation(method) {
			err = c.failure(err, method, true)
		}
	}()
	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	if result == nil {
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}
	media, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return errors.New("expected a JSON response from OpenSBX")
	}
	decoder := json.NewDecoder(res.Body)
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("invalid API JSON response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid API JSON response: expected exactly one JSON value")
	}
	return nil
}

func (c *Client) stream(ctx context.Context, path string) (*http.Response, error) {
	res, err := c.open(ctx, c.streamHTTP, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	media, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || media != "application/x-ndjson" {
		res.Body.Close()
		return nil, errors.New("expected an NDJSON stream from OpenSBX")
	}
	return res, nil
}

func (c *Client) Wait(ctx context.Context, sandboxID, commandID string) (models.CommandDetail, error) {
	res, err := c.stream(ctx, Path("sandboxes", sandboxID, "cmd", commandID)+"?wait=true")
	if err != nil {
		return models.CommandDetail{}, err
	}
	defer res.Body.Close()
	records := streamRecords(res.Body)
	for {
		var item models.CommandResponse
		if err := decodeRecord(records, &item); err != nil {
			if errors.Is(err, io.EOF) {
				return item.Command, fmt.Errorf("command %s: wait stream ended without a confirmed exit code; inspect or wait again", commandID)
			}
			return item.Command, fmt.Errorf("command %s: invalid or interrupted wait stream: %w", commandID, err)
		}
		if item.Command.ID != commandID || item.Command.SandboxID != sandboxID {
			return item.Command, errors.New("wait stream returned a mismatched command")
		}
		if item.Command.ExitCode != nil {
			return item.Command, nil
		}
	}
}

func (c *Client) Logs(ctx context.Context, sandboxID, commandID string, stdout, stderr io.Writer) error {
	res, err := c.stream(ctx, Path("sandboxes", sandboxID, "cmd", commandID, "logs")+"?stream=true")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	records := streamRecords(res.Body)
	for {
		var item struct {
			Type string  `json:"type"`
			Data *string `json:"data"`
		}
		if err := decodeRecord(records, &item); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("invalid or interrupted log stream: %w", err)
		}
		if item.Data == nil {
			return errors.New("log record is missing text data")
		}
		var out io.Writer
		switch item.Type {
		case "stdout":
			out = stdout
		case "stderr":
			out = stderr
		case "error":
			return fmt.Errorf("log stream error: %s", c.safeText(*item.Data))
		default:
			return fmt.Errorf("unexpected log stream type %q", item.Type)
		}
		if _, err := io.WriteString(out, *item.Data); err != nil {
			return fmt.Errorf("write command output: %w", err)
		}
	}
}

func (c *Client) Resolve(ctx context.Context, target string) (string, error) {
	var result struct {
		Sandboxes []models.SandboxSummary `json:"sandboxes"`
	}
	if err := c.Request(ctx, http.MethodGet, Path("sandboxes"), nil, &result); err != nil {
		return "", err
	}
	var matches []models.SandboxSummary
	for _, item := range result.Sandboxes {
		if item.ID == target || item.Name == target {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		for _, item := range result.Sandboxes {
			if target != "" && strings.HasPrefix(item.ID, target) {
				matches = append(matches, item)
			}
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("sandbox %q not found; use opensbx ls", target)
	}
	var choices []string
	for _, item := range matches {
		choices = append(choices, terminaltext.Escape(item.ID+" ("+item.Name+")", false))
	}
	return "", fmt.Errorf("sandbox %q is ambiguous: %s; use a full ID", target, strings.Join(choices, ", "))
}
