package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"opensbx/models"
)

// Handler holds dependencies for all API handlers.
type Handler struct {
	docker     DockerClient
	baseDomain string // base domain for proxy URLs (e.g. "localhost")
	proxyAddr  string // proxy listen address (e.g. ":3000")
}

// New creates a Handler with the given Docker client and proxy config.
func New(d DockerClient, baseDomain, proxyAddr string) *Handler {
	return &Handler{docker: d, baseDomain: baseDomain, proxyAddr: proxyAddr}
}

// proxyURL builds the public URL for a named sandbox.
// Local domains return http URLs and keep the proxy port when needed.
// Public domains return https URLs without exposing internal proxy ports.
func (h *Handler) proxyURL(name string) string {
	return buildSandboxURL(name, h.baseDomain, h.proxyAddr)
}

// healthCheck handles GET /health.
// @Summary      Health check
// @Description  Returns the health status of the API and its Docker daemon connection.
// @Tags         system
// @Produce      json
// @Success      200  {object}  map[string]string  "status: healthy"
// @Failure      503  {object}  map[string]string  "status: unhealthy"
// @Router       /health [get]
func (h *Handler) healthCheck(c *gin.Context) {
	if err := h.docker.Ping(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "unhealthy",
			"error":  err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "healthy"})
}

// listSandboxes handles GET /v1/sandboxes.
// @Summary      List sandboxes
// @Description  List all sandboxes (running and stopped).
// @Tags         sandboxes
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "List of sandboxes"
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes [get]
func (h *Handler) listSandboxes(c *gin.Context) {
	items, err := h.docker.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}

	for i := range items {
		items[i].URL = h.proxyURL(items[i].Name)
	}

	if len(items) == 0 {
		c.JSON(http.StatusOK, gin.H{"sandboxes": items, "message": "no sandboxes found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"sandboxes": items})
}

// createSandbox handles POST /v1/sandboxes.
// @Summary      Create a sandbox
// @Description  Create and start a new Docker container. Returns its ID and assigned host ports.
// @Tags         sandboxes
// @Accept       json
// @Produce      json
// @Param        body  body      models.CreateSandboxRequest  true  "Sandbox configuration"
// @Success      201   {object}  models.CreateSandboxResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes [post]
func (h *Handler) createSandbox(c *gin.Context) {
	var req models.CreateSandboxRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}

	if req.Timeout < 0 {
		badRequest(c, "timeout must be >= 0")
		return
	}
	if req.Resources != nil {
		if req.Resources.Memory < 0 {
			badRequest(c, "resources.memory must be >= 0")
			return
		}
		if req.Resources.Memory > 8192 {
			badRequest(c, "resources.memory must be <= 8192 (8GB)")
			return
		}
		if req.Resources.CPUs < 0 {
			badRequest(c, "resources.cpus must be >= 0")
			return
		}
		if req.Resources.CPUs > 4.0 {
			badRequest(c, "resources.cpus must be <= 4.0")
			return
		}
	}

	result, err := h.docker.Create(c.Request.Context(), req)
	if err != nil {
		internalError(c, err)
		return
	}

	result.URL = h.proxyURL(result.Name)
	c.JSON(http.StatusCreated, result)
}

// getSandbox handles GET /v1/sandboxes/:id.
// @Summary      Inspect a sandbox
// @Description  Returns detailed info about the sandbox including ports, resources, and expiration.
// @Tags         sandboxes
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  models.SandboxDetail
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id} [get]
func (h *Handler) getSandbox(c *gin.Context) {
	info, err := h.docker.Inspect(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}

	info.URL = h.proxyURL(info.Name)
	c.JSON(http.StatusOK, info)
}

// startSandbox handles POST /v1/sandboxes/:id/start.
// @Summary      Start a sandbox
// @Description  Start a stopped sandbox. Returns the port mappings and a fresh expiration timer.
// @Tags         sandboxes
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  models.RestartResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/start [post]
func (h *Handler) startSandbox(c *gin.Context) {
	result, err := h.docker.Start(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

// stopSandbox handles POST /v1/sandboxes/:id/stop.
// @Summary      Stop a sandbox
// @Description  Gracefully stop a running sandbox.
// @Tags         sandboxes
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  map[string]string  "status: stopped"
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/stop [post]
func (h *Handler) stopSandbox(c *gin.Context) {
	if err := h.docker.Stop(c.Request.Context(), c.Param("id")); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "stopped"})
}

// restartSandbox handles POST /v1/sandboxes/:id/restart.
// @Summary      Restart a sandbox
// @Description  Restart a sandbox (stop + start). Returns the new port mappings and a fresh expiration timer.
// @Tags         sandboxes
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  models.RestartResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/restart [post]
func (h *Handler) restartSandbox(c *gin.Context) {
	result, err := h.docker.Restart(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

// deleteSandbox handles DELETE /v1/sandboxes/:id.
// @Summary      Delete a sandbox
// @Description  Force-remove a sandbox regardless of its state.
// @Tags         sandboxes
// @Param        id   path      string  true  "Sandbox ID"
// @Success      204  "No Content"
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id} [delete]
func (h *Handler) deleteSandbox(c *gin.Context) {
	if err := h.docker.Remove(c.Request.Context(), c.Param("id")); err != nil {
		internalError(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// getStats handles GET /v1/sandboxes/:id/stats.
// @Summary      Get container stats
// @Description  Returns a snapshot of CPU, memory and process usage for the sandbox.
// @Tags         sandboxes
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  models.SandboxStats
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/stats [get]
func (h *Handler) getStats(c *gin.Context) {
	stats, err := h.docker.Stats(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, stats)
}

// execCommand handles POST /v1/sandboxes/:id/cmd.
// @Summary      Execute a command
// @Description  Execute a command asynchronously inside the sandbox. Returns a command ID immediately. Use ?wait=true to stream ND-JSON until completion.
// @Tags         commands
// @Accept       json
// @Produce      json
// @Param        id    path      string                       true  "Sandbox ID"
// @Param        body  body      models.ExecCommandRequest    true  "Command to execute"
// @Param        wait  query     bool                         false "Block until command finishes (ND-JSON stream)"
// @Success      200   {object}  models.CommandResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      409   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/cmd [post]
func (h *Handler) execCommand(c *gin.Context) {
	var req models.ExecCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}

	cmd, err := h.docker.ExecCommand(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		internalError(c, err)
		return
	}

	// If ?wait=true, stream ND-JSON until command finishes.
	if c.Query("wait") == "true" {
		h.streamWait(c, c.Param("id"), cmd.ID)
		return
	}

	c.JSON(http.StatusOK, models.CommandResponse{Command: cmd})
}

// listCommands handles GET /v1/sandboxes/:id/cmd.
// @Summary      List commands
// @Description  Returns all commands executed in the sandbox.
// @Tags         commands
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  models.CommandListResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/cmd [get]
func (h *Handler) listCommands(c *gin.Context) {
	cmds, err := h.docker.ListCommands(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, models.CommandListResponse{Commands: cmds})
}

// getCommand handles GET /v1/sandboxes/:id/cmd/:cmdId.
// @Summary      Get command status
// @Description  Returns the status of a command. Use ?wait=true to block until the command finishes (ND-JSON stream).
// @Tags         commands
// @Produce      json
// @Param        id      path      string  true  "Sandbox ID"
// @Param        cmdId   path      string  true  "Command ID"
// @Param        wait    query     bool    false "Block until command finishes (ND-JSON stream)"
// @Success      200  {object}  models.CommandResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/cmd/{cmdId} [get]
func (h *Handler) getCommand(c *gin.Context) {
	cmd, err := h.docker.GetCommand(c.Request.Context(), c.Param("id"), c.Param("cmdId"))
	if err != nil {
		internalError(c, err)
		return
	}

	// If ?wait=true, block until command finishes.
	if c.Query("wait") == "true" {
		h.streamWait(c, c.Param("id"), c.Param("cmdId"))
		return
	}

	c.JSON(http.StatusOK, models.CommandResponse{Command: cmd})
}

// killCommand handles POST /v1/sandboxes/:id/cmd/:cmdId/kill.
// @Summary      Kill a command
// @Description  Send a POSIX signal to a running command.
// @Tags         commands
// @Accept       json
// @Produce      json
// @Param        id      path      string                     true  "Sandbox ID"
// @Param        cmdId   path      string                     true  "Command ID"
// @Param        body    body      models.KillCommandRequest  true  "Signal to send"
// @Success      200  {object}  models.CommandResponse
// @Failure      400  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      409  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/cmd/{cmdId}/kill [post]
func (h *Handler) killCommand(c *gin.Context) {
	var req models.KillCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}

	cmd, err := h.docker.KillCommand(c.Request.Context(), c.Param("id"), c.Param("cmdId"), req.Signal)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, models.CommandResponse{Command: cmd})
}

// getCommandLogs handles GET /v1/sandboxes/:id/cmd/:cmdId/logs.
// @Summary      Get command logs
// @Description  Returns stdout and stderr of a command. By default returns a JSON snapshot. Use ?stream=true to stream as ND-JSON lines in real time.
// @Tags         commands
// @Produce      json
// @Produce      application/x-ndjson
// @Param        id      path      string  true  "Sandbox ID"
// @Param        cmdId   path      string  true  "Command ID"
// @Param        stream  query     bool    false "Stream logs as ND-JSON (default: false)"
// @Success      200  {object}  models.CommandLogsResponse  "JSON snapshot (default)"
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/cmd/{cmdId}/logs [get]
func (h *Handler) getCommandLogs(c *gin.Context) {
	sandboxID := c.Param("id")
	cmdID := c.Param("cmdId")

	// Stream mode: ND-JSON real-time logs.
	if c.Query("stream") == "true" {
		h.streamLogs(c, sandboxID, cmdID)
		return
	}

	// Default: JSON snapshot.
	logs, err := h.docker.GetCommandLogs(c.Request.Context(), sandboxID, cmdID)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, logs)
}

// streamLogs streams stdout/stderr as ND-JSON lines until the command finishes.
func (h *Handler) streamLogs(c *gin.Context, sandboxID, cmdID string) {
	stdoutR, stderrR, err := h.docker.StreamCommandLogs(
		c.Request.Context(), sandboxID, cmdID,
	)
	if err != nil {
		internalError(c, err)
		return
	}
	defer stdoutR.Close()
	defer stderrR.Close()

	c.Header("Content-Type", "application/x-ndjson")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	enc := json.NewEncoder(c.Writer)

	type logLine struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}

	// Read from both streams concurrently, write as ND-JSON.
	lines := make(chan logLine, 64)
	readStream := func(r io.ReadCloser, streamType string) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			lines <- logLine{Type: streamType, Data: scanner.Text() + "\n"}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); readStream(stdoutR, "stdout") }()
	go func() { defer wg.Done(); readStream(stderrR, "stderr") }()
	go func() { wg.Wait(); close(lines) }()

	for line := range lines {
		if c.IsAborted() {
			return
		}
		enc.Encode(line)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// streamWait streams ND-JSON with command status when started and when finished.
func (h *Handler) streamWait(c *gin.Context, sandboxID, cmdID string) {
	c.Header("Content-Type", "application/x-ndjson")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	enc := json.NewEncoder(c.Writer)

	// Emit initial status.
	cmd, err := h.docker.GetCommand(c.Request.Context(), sandboxID, cmdID)
	if err != nil {
		return
	}
	enc.Encode(models.CommandResponse{Command: cmd})
	if flusher != nil {
		flusher.Flush()
	}

	// Wait for completion.
	cmd, err = h.docker.WaitCommand(c.Request.Context(), sandboxID, cmdID)
	if err != nil {
		return
	}
	enc.Encode(models.CommandResponse{Command: cmd})
	if flusher != nil {
		flusher.Flush()
	}
}

// readFile handles GET /v1/sandboxes/:id/files?path=<path>.
// @Summary      Read a file
// @Description  Returns the content of a file at the given path inside the sandbox.
// @Tags         files
// @Produce      json
// @Param        id    path      string  true  "Sandbox ID"
// @Param        path  query     string  true  "File path inside the sandbox"
// @Success      200   {object}  models.FileReadResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/files [get]
func (h *Handler) readFile(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		badRequest(c, "path query param is required")
		return
	}

	content, err := h.docker.ReadFile(c.Request.Context(), c.Param("id"), path)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, models.FileReadResponse{Path: path, Content: content})
}

// writeFile handles PUT /v1/sandboxes/:id/files?path=<path>.
// @Summary      Write a file
// @Description  Write or overwrite a file inside the sandbox. Creates parent directories as needed.
// @Tags         files
// @Accept       json
// @Produce      json
// @Param        id    path      string                  true  "Sandbox ID"
// @Param        path  query     string                  true  "File path inside the sandbox"
// @Param        body  body      models.FileWriteRequest  true  "File content"
// @Success      200   {object}  map[string]string  "path and status"
// @Failure      400   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/files [put]
func (h *Handler) writeFile(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		badRequest(c, "path query param is required")
		return
	}

	var req models.FileWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}

	if err := h.docker.WriteFile(c.Request.Context(), c.Param("id"), path, req.Content); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"path": path, "status": "written"})
}

// deleteFile handles DELETE /v1/sandboxes/:id/files?path=<path>.
// @Summary      Delete a file
// @Description  Remove a file or directory (recursive) inside the sandbox.
// @Tags         files
// @Param        id    path      string  true  "Sandbox ID"
// @Param        path  query     string  true  "File path inside the sandbox"
// @Success      204  "No Content"
// @Failure      400   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/files [delete]
func (h *Handler) deleteFile(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		badRequest(c, "path query param is required")
		return
	}

	if err := h.docker.DeleteFile(c.Request.Context(), c.Param("id"), path); err != nil {
		internalError(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// listDir handles GET /v1/sandboxes/:id/files/list?path=<path>.
// @Summary      List a directory
// @Description  Returns the output of ls -la for the given directory. Defaults to root (/).
// @Tags         files
// @Produce      json
// @Param        id    path      string  true   "Sandbox ID"
// @Param        path  query     string  false  "Directory path (default: /)"
// @Success      200   {object}  models.FileListResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/files/list [get]
func (h *Handler) listDir(c *gin.Context) {
	path := c.DefaultQuery("path", "/")

	output, err := h.docker.ListDir(c.Request.Context(), c.Param("id"), path)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, models.FileListResponse{Path: path, Output: output})
}

// pauseSandbox handles POST /v1/sandboxes/:id/pause.
// @Summary      Pause a sandbox
// @Description  Freeze all processes inside the sandbox.
// @Tags         sandboxes
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  map[string]string  "status: paused"
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/pause [post]
func (h *Handler) pauseSandbox(c *gin.Context) {
	if err := h.docker.Pause(c.Request.Context(), c.Param("id")); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "paused"})
}

// resumeSandbox handles POST /v1/sandboxes/:id/resume.
// @Summary      Resume a sandbox
// @Description  Resume a paused sandbox.
// @Tags         sandboxes
// @Produce      json
// @Param        id   path      string  true  "Sandbox ID"
// @Success      200  {object}  map[string]string  "status: resumed"
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/resume [post]
func (h *Handler) resumeSandbox(c *gin.Context) {
	if err := h.docker.Resume(c.Request.Context(), c.Param("id")); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "resumed"})
}

// renewExpiration handles POST /v1/sandboxes/:id/renew-expiration.
// @Summary      Renew sandbox expiration
// @Description  Reset the auto-stop timer for a sandbox.
// @Tags         sandboxes
// @Accept       json
// @Produce      json
// @Param        id    path      string                          true  "Sandbox ID"
// @Param        body  body      models.RenewExpirationRequest   true  "New timeout"
// @Success      200   {object}  models.RenewExpirationResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /sandboxes/{id}/renew-expiration [post]
func (h *Handler) renewExpiration(c *gin.Context) {
	var req models.RenewExpirationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}

	if req.Timeout <= 0 {
		badRequest(c, "timeout must be > 0")
		return
	}

	if err := h.docker.RenewExpiration(c.Request.Context(), c.Param("id"), req.Timeout); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, models.RenewExpirationResponse{Status: "renewed", Timeout: req.Timeout})
}

// pullImage handles POST /v1/images/pull.
// @Summary      Pull a Docker image
// @Description  Downloads a Docker image from a registry to use in sandboxes.
// @Tags         images
// @Accept       json
// @Produce      json
// @Param        body  body      models.ImagePullRequest  true  "Image to pull"
// @Success      200   {object}  models.ImagePullResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /images/pull [post]
func (h *Handler) pullImage(c *gin.Context) {
	var req models.ImagePullRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}

	if err := h.docker.PullImage(c.Request.Context(), req.Image); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, models.ImagePullResponse{Status: "pulled", Image: req.Image})
}

// deleteImage handles DELETE /v1/images/:id.
// @Summary      Delete a local image
// @Description  Removes a Docker image from the local store. Use force=true if containers reference it.
// @Tags         images
// @Param        id     path      string  true   "Image ID or name:tag"
// @Param        force  query     bool    false  "Force removal even if referenced by containers"
// @Success      204  "No Content"
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /images/{id} [delete]
func (h *Handler) deleteImage(c *gin.Context) {
	force := c.Query("force") == "true"
	if err := h.docker.RemoveImage(c.Request.Context(), c.Param("id"), force); err != nil {
		internalError(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// getImage handles GET /v1/images/:id.
// @Summary      Inspect an image
// @Description  Returns details for a single local Docker image.
// @Tags         images
// @Produce      json
// @Param        id   path      string  true  "Image ID or name:tag"
// @Success      200  {object}  models.ImageDetail
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /images/{id} [get]
func (h *Handler) getImage(c *gin.Context) {
	detail, err := h.docker.InspectImage(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, detail)
}

// listImages handles GET /v1/images.
// @Summary      List local images
// @Description  Returns all Docker images available locally.
// @Tags         images
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "List of images"
// @Failure      500  {object}  ErrorResponse
// @Security     ApiKeyAuth
// @Router       /images [get]
func (h *Handler) listImages(c *gin.Context) {
	images, err := h.docker.ListImages(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}

	if len(images) == 0 {
		c.JSON(http.StatusOK, gin.H{"images": images, "message": "no images found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"images": images})
}
