package api

import "github.com/gin-gonic/gin"

// RegisterHealthCheck attaches the /v1/health endpoint directly to the engine (no auth).
func (h *Handler) RegisterHealthCheck(r *gin.Engine) {
	r.GET("/v1/health", h.healthCheck)
}

// RegisterRoutes attaches all sandbox routes to the given router group.
func (h *Handler) RegisterRoutes(v1 *gin.RouterGroup) {
	sb := v1.Group("/sandboxes")
	sb.GET("", h.listSandboxes)
	sb.POST("", h.createSandbox)
	sb.GET("/:id", h.getSandbox)
	sb.DELETE("/:id", h.deleteSandbox)
	sb.POST("/:id/start", h.startSandbox)
	sb.POST("/:id/stop", h.stopSandbox)
	sb.POST("/:id/restart", h.restartSandbox)
	sb.POST("/:id/pause", h.pauseSandbox)
	sb.POST("/:id/resume", h.resumeSandbox)
	sb.POST("/:id/renew-expiration", h.renewExpiration)
	sb.GET("/:id/network", h.getSandboxNetwork)
	sb.POST("/:id/cmd", h.execCommand)
	sb.GET("/:id/cmd", h.listCommands)
	sb.GET("/:id/cmd/:cmdId", h.getCommand)
	sb.POST("/:id/cmd/:cmdId/kill", h.killCommand)
	sb.GET("/:id/cmd/:cmdId/logs", h.getCommandLogs)
	sb.GET("/:id/stats", h.getStats)
	sb.GET("/:id/files", h.readFile)
	sb.PUT("/:id/files", h.writeFile)
	sb.DELETE("/:id/files", h.deleteFile)
	sb.GET("/:id/files/list", h.listDir)

	img := v1.Group("/images")
	img.GET("", h.listImages)
	img.GET("/:id", h.getImage)
	img.POST("/pull", h.pullImage)
	img.DELETE("/:id", h.deleteImage)
}
