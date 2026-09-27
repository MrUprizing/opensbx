package main

import (
	"context"
	"errors"
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
	"opensbx/internal/config"
	"opensbx/internal/database"
	"opensbx/internal/docker"
	"opensbx/internal/images"
	"opensbx/internal/logging"
	"opensbx/internal/proxy"
	"opensbx/internal/runtimechoice"
	"opensbx/internal/runtimeio"
	"opensbx/internal/service"

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
// @host      localhost:8080
// @BasePath  /v1

// @securityDefinitions.apikey  ApiKeyAuth
// @in                          header
// @name                        Authorization
// @description                 Enter "Bearer {your-api-key}"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "image" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := images.CLI(ctx, os.Args[2:], config.DefaultDataDir(), os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg := config.Load()
	logFileCloser, err := logging.Setup(cfg.LogFile)
	if err != nil {
		log.Fatalf("logging setup failed: %v", err)
	}
	defer logFileCloser.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	choice, err := runtimechoice.Select(ctx, cfg.Runtime, runtime.GOOS, isatty.IsTerminal(os.Stdin.Fd()), os.Stdin, os.Stdout)
	if err != nil {
		log.Fatalf("runtime selection failed: %v", err)
	}
	dbPath, err := cfg.ExecutionPath(choice)
	if err != nil {
		log.Fatalf("local state: %v", err)
	}
	executionLock := flock.New(dbPath + ".lock")
	locked, err := executionLock.TryLock()
	if err != nil || !locked {
		log.Fatalf("execution database already in use or inaccessible: %v", err)
	}
	defer executionLock.Unlock()
	var appleRunner applecontainer.Runner
	if choice == "container" {
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		appleRunner, err = applecontainer.Resolve(checkCtx)
		cancel()
		if err != nil {
			log.Fatalf("runtime validation failed: %v", err)
		}
	}
	db := database.New(dbPath)
	repo := database.NewRepository(db)
	var dc interface {
		runtimeio.Engine
		runtimeio.NativeCache
		SetCacheInvalidator(func(string))
		Shutdown(context.Context)
	}
	if choice == "container" {
		client := applecontainer.New(repo, appleRunner, nil)
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = client.Ping(checkCtx)
		cancel()
		if err != nil {
			log.Fatalf("runtime validation failed: %v", err)
		}
		dc = client
	} else {
		dc = docker.New(repo)
	}
	log.Printf("sandbox runtime: %s (database: %s)", choice, dbPath)

	store, err := images.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("image store: %v", err)
	}
	adapter := runtimeio.New(dc, dc, repo)
	app, err := service.New(ctx, adapter, adapter, store, repo)
	if err != nil {
		log.Fatalf("application setup: %v", err)
	}
	// Open one loopback listener only after runtime and store validation.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatalf("api listen: %v", err)
	}
	defer listener.Close()
	app.SetAddress(listener.Addr())
	proxyServer := proxy.New(repo)
	proxyServer.SetResolver(app.Route)
	dc.SetCacheInvalidator(proxyServer.InvalidateCache)
	proxyHandler := proxyServer.Handler()

	log.Printf("logs file: %s", cfg.LogFile)

	// --- API server ---
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

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

	// Graceful shutdown: listen for SIGINT/SIGTERM, then stop tracked containers.
	srv := &http.Server{Addr: listener.Addr().String(), Handler: proxy.LocalHandler(listener.Addr(), r, proxyHandler), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		log.Printf("local API and sandbox URLs listening on %s", listener.Addr())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("api listen: %v", err)
		}
	}()

	<-ctx.Done()
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
}
