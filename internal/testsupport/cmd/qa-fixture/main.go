// Command qa-fixture starts an opt-in synthetic target for browser QA.
// It uses the production Host router and reverse-proxy path but never creates a sandbox.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/proxy"
)

func startFixture() (*http.Server, net.Listener, func(), error) {
	upstream := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Path == "/api/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: ready\ndata: synthetic-upstream\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		_, _ = fmt.Fprintf(w, "synthetic sandbox host=%s path=%s\n", r.Host, r.URL.Path)
	})}
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	go func() { _ = upstream.Serve(upstreamListener) }()
	cleanupUpstream := func() { _ = upstream.Close(); _ = upstreamListener.Close() }
	_, upstreamPort, err := net.SplitHostPort(upstreamListener.Addr().String())
	if err != nil {
		cleanupUpstream()
		return nil, nil, nil, err
	}

	management := http.NewServeMux()
	management.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"healthy","fixture":true}`)
	})
	management.HandleFunc("/v1/mcp", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "synthetic control MCP endpoint\n") })
	management.HandleFunc("/swagger/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, "<!doctype html><title>OpenSBX QA fixture</title><h1>Control host: %s</h1><script src='/swagger/fixture.js'></script>", r.Host)
	})
	management.HandleFunc("/swagger/fixture.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = fmt.Fprint(w, "document.body.dataset.fixture = 'loaded';")
	})

	db := database.New(":memory:")
	sqlDB, err := db.DB()
	if err != nil {
		cleanupUpstream()
		return nil, nil, nil, err
	}
	cleanupDB := func() { _ = sqlDB.Close() }
	proxyServer := proxy.New(database.NewRepository(db))
	proxyServer.SetResolver(func(_ context.Context, sandboxName string) (string, error) {
		if sandboxName != "qa" {
			return "", errors.New("synthetic sandbox not found")
		}
		return upstreamPort, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cleanupDB()
		cleanupUpstream()
		return nil, nil, nil, err
	}
	router := proxy.LocalHandler(listener.Addr(), management, proxyServer.Handler())
	server := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second}
	cleanup := func() { cleanupDB(); cleanupUpstream() }
	return server, listener, cleanup, nil
}

func main() {
	server, listener, cleanup, err := startFixture()
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("fixture server: %v", err)
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Printf("Control:  http://localhost:%d/v1/health\n", port)
	fmt.Printf("Sandbox:  http://qa.localhost:%d/\n", port)
	fmt.Printf("SSE:      http://qa.localhost:%d/api/events\n", port)
	fmt.Printf("Swagger:  http://localhost:%d/swagger/\n", port)
	fmt.Println("Stop with Ctrl-C; this fixture does not create runtime resources.")
	<-stop.Done()
	ctx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("fixture shutdown: %v", err)
	}
}
