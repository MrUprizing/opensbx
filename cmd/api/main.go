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
	"runtime"
	"syscall"
	"time"

	"opensbx/internal/api"
	"opensbx/internal/applecontainer"
	"opensbx/internal/cli"
	"opensbx/internal/config"
	"opensbx/internal/database"
	"opensbx/internal/docker"
	"opensbx/internal/images"
	"opensbx/internal/logging"
	"opensbx/internal/processctl"
	"opensbx/internal/proxy"
	"opensbx/internal/runtimechoice"
	"opensbx/internal/runtimeio"
	"opensbx/internal/service"
	"opensbx/internal/terminaltext"

	"github.com/gin-gonic/gin"
	"github.com/gofrs/flock"
	"github.com/mattn/go-isatty"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "opensbx/docs"
)

// @title           Opensbx API
// @version         1.0
// @description     Lightweight sandbox API for running untrusted code in isolated environments.
// @host      localhost:18089
// @BasePath  /v1

// @securityDefinitions.apikey  ApiKeyAuth
// @in                          header
// @name                        Authorization
// @description                 Enter "Bearer {your-api-key}"

func main() {
	if err := runCLI(os.Args[1:], os.Stdout); err != nil {
		var exit *cli.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		fmt.Fprintf(os.Stderr, "opensbx: %s\n", terminaltext.Escape(err.Error(), false))
		os.Exit(1)
	}
}

func runServer(args []string) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return err
	}
	logFileCloser, err := logging.Setup(cfg.LogFile)
	if err != nil {
		return fmt.Errorf("logging setup failed: %w", err)
	}
	defer logFileCloser.Close()

	ctx, stop := signal.NotifyContext(context.Background(), serverSignals()...)
	defer stop()
	choice, err := runtimechoice.Select(ctx, cfg.Runtime, runtime.GOOS, isatty.IsTerminal(os.Stdin.Fd()), os.Stdin, os.Stdout)
	if err != nil {
		return fmt.Errorf("runtime selection failed: %w", err)
	}
	dbPath, err := cfg.ExecutionPath(choice)
	if err != nil {
		return fmt.Errorf("local state: %w", err)
	}
	executionLock := flock.New(dbPath + ".lock")
	locked, err := executionLock.TryLock()
	if err != nil {
		return fmt.Errorf("could not lock execution database: %w", err)
	}
	if !locked {
		return errors.New("execution database already in use by another OpenSBX process; stop that server before starting another")
	}
	defer executionLock.Unlock()
	serverLock, err := processctl.Acquire(cfg.DataDir)
	if err != nil {
		return err
	}
	defer serverLock.Close()
	var appleRunner applecontainer.Runner
	if choice == "container" {
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		appleRunner, err = applecontainer.Resolve(checkCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("runtime validation failed: %w", err)
		}
	}
	db := database.New(dbPath)
	repo := database.NewRepository(db)
	var dc interface {
		runtimeio.Engine
		runtimeio.NativeCache
		SetCacheInvalidator(func(string))
		Shutdown(context.Context)
		Recover(context.Context) error
	}
	if choice == "container" {
		client := applecontainer.New(repo, appleRunner, nil)
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = client.Ping(checkCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("runtime validation failed: %w", err)
		}
		dc = client
	} else {
		dc = docker.New(repo)
	}
	log.Printf("sandbox runtime: %s (database: %s)", choice, dbPath)

	store, err := images.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("image store: %w", err)
	}
	adapter := runtimeio.New(dc, dc, repo)
	app, err := service.New(ctx, adapter, adapter, store, repo)
	if err != nil {
		return fmt.Errorf("application setup: %w", err)
	}
	proxyServer := proxy.New(repo)
	proxyServer.SetResolver(app.Route)
	dc.SetCacheInvalidator(proxyServer.InvalidateCache)
	// Recovery is intentionally not a constructor side effect. Runtime validation,
	// process/database locks and image-store initialization have all succeeded.
	err = dc.Recover(ctx)
	if err != nil {
		return fmt.Errorf("sandbox recovery incomplete (durable intents retained; restore runtime/database access and restart): %w", err)
	}
	// Open one loopback listener only after runtime and store validation.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("api listen: %w", listenError(cfg.Addr, err))
	}
	defer listener.Close()
	app.SetAddress(listener.Addr())
	proxyHandler := proxyServer.Handler()

	log.Printf("logs file: %s", cfg.LogFile)

	// --- API server ---
	r := gin.New()
	r.Use(logging.RequestLogger(), gin.Recovery())

	v1 := r.Group("/v1")
	if cfg.APIKey != "" {
		v1.Use(api.APIKeyAuth(cfg.APIKey))
	}

	h := api.New(app)
	h.RegisterHealthCheck(r)
	h.RegisterRoutes(v1)
	mcpHandler := api.NewMCPHandler(app)
	mcp := v1.Group("")
	mcp.Use(api.MCPMetadataLogger())
	mcp.Any("/mcp", gin.WrapH(mcpHandler))
	mcp.Any("/mcp/*path", gin.WrapH(mcpHandler))

	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))

	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    "NOT_FOUND",
			"message": "route not found",
		})
	})

	// Graceful shutdown on terminal or process-control signals stops tracked containers.
	srv := &http.Server{Addr: listener.Addr().String(), Handler: proxy.LocalHandler(listener.Addr(), r, proxyHandler), ReadHeaderTimeout: 10 * time.Second}

	serveErrors := make(chan error, 1)
	go func() {
		log.Printf("local API and sandbox URLs listening on %s", listener.Addr())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			serveErrors <- err
		}
	}()
	readyCtx, cancelReady := context.WithTimeout(ctx, 5*time.Second)
	if err := waitForAPIReady(readyCtx, listener.Addr().String()); err != nil {
		cancelReady()
		return err
	}
	cancelReady()
	if err := serverLock.Publish(os.Getpid()); err != nil {
		return fmt.Errorf("record server process: %w", err)
	}

	select {
	case <-ctx.Done():
	case err := <-serveErrors:
		return fmt.Errorf("api server stopped unexpectedly: %w", err)
	}
	log.Println("shutting down: stopping incoming traffic...")

	httpShutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelHTTP()

	if err := srv.Shutdown(httpShutdownCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("api shutdown: timeout reached")
		} else {
			log.Printf("api shutdown: %v", err)
		}
	}

	log.Println("shutting down: stopping tracked sandboxes...")
	sandboxShutdownCtx, cancelSandboxes := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelSandboxes()
	dc.Shutdown(sandboxShutdownCtx)

	log.Println("server stopped")
	return nil
}

func listenError(addr string, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("address %s is already in use; choose another loopback port with -addr 127.0.0.1:18090", addr)
	}
	return fmt.Errorf("cannot listen on %s: %w", addr, err)
}

func waitForAPIReady(ctx context.Context, addr string) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/swagger/index.html", nil)
		if err != nil {
			return fmt.Errorf("prepare API readiness check: %w", err)
		}
		response, requestErr := client.Do(req)
		if response != nil {
			_ = response.Body.Close()
			if requestErr == nil && response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("API did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
