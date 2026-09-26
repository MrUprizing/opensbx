package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"opensbx/internal/api"
	"opensbx/internal/applecontainer"
	"opensbx/internal/config"
	"opensbx/internal/database"
	"opensbx/internal/docker"
	"opensbx/internal/logging"
	"opensbx/internal/proxy"
	"opensbx/internal/runtimechoice"

	"github.com/gin-gonic/gin"
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
	cfg := config.Load()
	logFileCloser, err := logging.Setup(cfg.LogFile)
	if err != nil {
		log.Fatalf("logging setup failed: %v", err)
	}
	defer logFileCloser.Close()

	mcpLocalhostProtection := "enabled"
	if cfg.MCPDisableLocalhostProtection {
		mcpLocalhostProtection = "disabled"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	choice, err := runtimechoice.Select(ctx, cfg.Runtime, runtime.GOOS, isatty.IsTerminal(os.Stdin.Fd()), os.Stdin, os.Stdout)
	if err != nil {
		log.Fatalf("runtime selection failed: %v", err)
	}
	dbPath := "sandbox.db"
	var appleRunner applecontainer.Runner
	if choice == "container" {
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		appleRunner, err = applecontainer.Resolve(checkCtx)
		cancel()
		if err != nil {
			log.Fatalf("runtime validation failed: %v", err)
		}
		dbPath = "sandbox-container.db"
	}
	db := database.New(dbPath)
	repo := database.NewRepository(db)
	var dc interface {
		api.DockerClient
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

	// --- Reverse proxy (multi-listen) ---
	proxyServer := proxy.New(cfg.BaseDomain, repo)
	dc.SetCacheInvalidator(proxyServer.InvalidateCache)
	proxyHandler := proxyServer.Handler()

	var proxySrvs []*http.Server
	for _, addr := range cfg.ProxyAddrs {
		srv := &http.Server{Addr: addr, Handler: proxyHandler}
		proxySrvs = append(proxySrvs, srv)
		go func(a string) {
			log.Printf("proxy listening on %s (domain: *.%s)", a, cfg.BaseDomain)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("proxy listen %s: %v", a, err)
			}
		}(addr)
	}
	log.Printf("proxy URLs via %s", strings.Join(cfg.ProxyAddrs, ", "))
	log.Printf("mcp localhost protection: %s (base-domain: %s)", mcpLocalhostProtection, cfg.BaseDomain)
	log.Printf("logs file: %s", cfg.LogFile)

	// --- API server ---
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	v1 := r.Group("/v1")
	if cfg.APIKey != "" {
		v1.Use(api.APIKeyAuth(cfg.APIKey))
	}

	h := api.New(dc, cfg.BaseDomain, cfg.PrimaryProxyAddr())
	h.RegisterHealthCheck(r)
	h.RegisterRoutes(v1)
	mcpHandler := api.NewMCPHandler(dc, cfg.BaseDomain, cfg.PrimaryProxyAddr(), cfg.MCPDisableLocalhostProtection)
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
	srv := &http.Server{Addr: cfg.Addr, Handler: r}

	go func() {
		log.Printf("api listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("api listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down: stopping incoming traffic...")

	httpShutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelHTTP()

	for _, ps := range proxySrvs {
		if err := ps.Shutdown(httpShutdownCtx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				log.Printf("proxy shutdown %s: timeout reached", ps.Addr)
			} else {
				log.Printf("proxy shutdown %s: %v", ps.Addr, err)
			}
		}
	}
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
