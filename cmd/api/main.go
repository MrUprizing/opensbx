package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"open-sandbox/internal/api"
	"open-sandbox/internal/config"
	"open-sandbox/internal/database"
	"open-sandbox/internal/docker"
	"open-sandbox/internal/proxy"

	_ "open-sandbox/docs"
)

// @title           Open Sandbox API
// @version         1.0
// @description     Docker sandbox orchestrator REST API. Create, manage, and execute commands inside isolated Docker containers.

// @host      localhost:8080
// @BasePath  /v1

// @securityDefinitions.apikey  ApiKeyAuth
// @in                          header
// @name                        Authorization
// @description                 Enter "Bearer {your-api-key}"

func main() {
	cfg := config.Load()

	db := database.New("sandbox.db")
	repo := database.NewRepository(db)
	dc := docker.New(repo)

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
	mcpHandler := api.NewMCPHandler(dc, cfg.BaseDomain, cfg.PrimaryProxyAddr())
	v1.Any("/mcp", gin.WrapH(mcpHandler))
	v1.Any("/mcp/*path", gin.WrapH(mcpHandler))

	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))

	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    "NOT_FOUND",
			"message": "route not found",
		})
	})

	// Graceful shutdown: listen for SIGINT/SIGTERM, then stop tracked containers.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: cfg.Addr, Handler: r}

	go func() {
		log.Printf("api listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("api listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down: stopping tracked sandboxes...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dc.Shutdown(shutdownCtx)
	for _, ps := range proxySrvs {
		if err := ps.Shutdown(shutdownCtx); err != nil {
			log.Printf("proxy shutdown %s: %v", ps.Addr, err)
		}
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("api shutdown: %v", err)
	}

	log.Println("server stopped")
}
